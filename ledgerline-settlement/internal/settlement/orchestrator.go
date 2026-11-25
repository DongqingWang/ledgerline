// Package settlement is the async bank-settlement driver. In the fat-ledger
// design the settlement LIFECYCLE lives inside the ledger FSM (raft-durable),
// not here: this service only submits commands to the engine and drives the
// simulated rail. It holds no authoritative state of its own, so a crash
// loses nothing — the in-flight settlements are recoverable from the ledger.
package settlement

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/ledgerline/ledgerline-settlement/internal/engineclient"
)

// Engine is the ledger engine API the driver needs. The FSM owns the
// settlement state machine; these commands advance it deterministically.
type Engine interface {
	CreateAccount(ctx context.Context, req engineclient.CreateAccountRequest) (*engineclient.Account, error)
	GetAccount(ctx context.Context, id string) (*engineclient.Account, error)
	CreateSettlement(ctx context.Context, req engineclient.CreateSettlementRequest) (*engineclient.Settlement, error)
	MarkSettlementProcessing(ctx context.Context, id string, req engineclient.SettlementActionRequest) (*engineclient.Settlement, error)
	SettleOutcome(ctx context.Context, id string, req engineclient.SettlementActionRequest) (*engineclient.Settlement, error)
	GetSettlement(ctx context.Context, id string) (*engineclient.Settlement, error)
	Reverse(ctx context.Context, req engineclient.ReverseRequest) (*engineclient.TransferResult, error)
}

// Rail submits a settlement to the (simulated) bank rail for async processing.
type Rail interface {
	Submit(id string, amount int64)
}

// Orchestrator drives the deposit/withdrawal loops by advancing FSM settlement
// state. It never rolls back committed history: failures void the reservation,
// delayed returns reverse the posted entry.
type Orchestrator struct {
	Engine         Engine
	Rail           Rail
	FundingAccount string
	TimeoutSeconds int64
	Logger         *log.Logger
}

// NewOrchestrator builds a driver with sane defaults.
func NewOrchestrator(engine Engine, rail Rail) *Orchestrator {
	return &Orchestrator{
		Engine:         engine,
		Rail:           rail,
		FundingAccount: "bank-clearing",
		TimeoutSeconds: 7 * 24 * 3600,
	}
}

func (o *Orchestrator) logf(format string, args ...interface{}) {
	if o.Logger != nil {
		o.Logger.Printf(format, args...)
	}
}

func (o *Orchestrator) ensureFunding(ctx context.Context, currency string) error {
	_, err := o.Engine.GetAccount(ctx, o.FundingAccount)
	if err == nil {
		return nil
	}
	var le *engineclient.Error
	if !errors.As(err, &le) || le.StatusCode != http.StatusNotFound {
		return err
	}
	_, err = o.Engine.CreateAccount(ctx, engineclient.CreateAccountRequest{
		AccountID: o.FundingAccount, Currency: currency, Unlimited: true, Type: "asset",
	})
	if err != nil {
		var ce *engineclient.Error
		if errors.As(err, &ce) && ce.Code == "account_exists" {
			return nil
		}
		return err
	}
	return nil
}

func (o *Orchestrator) create(ctx context.Context, kind engineclient.SettlementKind, dedupID, accountID string, amount int64, currency string) (*engineclient.Settlement, error) {
	if err := o.ensureFunding(ctx, currency); err != nil {
		return nil, err
	}
	stl, err := o.Engine.CreateSettlement(ctx, engineclient.CreateSettlementRequest{
		DedupID: dedupID, Kind: kind, AccountID: accountID, FundingAccount: o.FundingAccount,
		AmountMinor: amount, Currency: currency, TimeoutSeconds: o.TimeoutSeconds,
	})
	if err != nil {
		return nil, err
	}
	// Idempotent replay: a duplicate create returns the existing settlement.
	// If it has already progressed past pending, do not re-drive it.
	if stl.Status != "pending" {
		return stl, nil
	}
	stl, err = o.Engine.MarkSettlementProcessing(ctx, stl.SettlementID, engineclient.SettlementActionRequest{
		DedupID: dedupID + "-proc",
	})
	if err != nil {
		return nil, err
	}
	o.Rail.Submit(stl.SettlementID, amount)
	o.logf("%s %s submitted to rail (durable in ledger)", kind, stl.SettlementID)
	return stl, nil
}

// CreateWithdrawal reserves funds in the FSM and submits to the rail.
func (o *Orchestrator) CreateWithdrawal(ctx context.Context, dedupID, accountID string, amount int64, currency string) (*engineclient.Settlement, error) {
	return o.create(ctx, engineclient.SettlementWithdrawal, dedupID, accountID, amount, currency)
}

// CreateDeposit records a pending credit in the FSM (not spendable) and submits.
func (o *Orchestrator) CreateDeposit(ctx context.Context, dedupID, accountID string, amount int64, currency string) (*engineclient.Settlement, error) {
	return o.create(ctx, engineclient.SettlementDeposit, dedupID, accountID, amount, currency)
}

// OnRailResult applies an async rail outcome (settled/failed) to the FSM.
// Idempotent: the engine dedups the action, so re-delivery is safe.
func (o *Orchestrator) OnRailResult(ctx context.Context, settlementID, outcome, reason string) error {
	_, err := o.Engine.SettleOutcome(ctx, settlementID, engineclient.SettlementActionRequest{
		DedupID: settlementID + "-" + outcome,
		Outcome: outcome,
		Reason:  reason,
	})
	if err != nil {
		return fmt.Errorf("settle outcome %s for %s: %w", outcome, settlementID, err)
	}
	o.logf("settlement %s outcome %s applied", settlementID, outcome)
	return nil
}

// ReturnSettlement simulates a delayed rail return (ACH return / chargeback) on
// an already-posted settlement, reversing the posted entry via the engine.
func (o *Orchestrator) ReturnSettlement(ctx context.Context, settlementID, reason string) (*engineclient.Settlement, error) {
	stl, err := o.Engine.GetSettlement(ctx, settlementID)
	if err != nil {
		return nil, err
	}
	if stl.Status != "posted" {
		return nil, fmt.Errorf("settlement %s is %s, want posted to return", settlementID, stl.Status)
	}
	if _, err := o.Engine.Reverse(ctx, engineclient.ReverseRequest{
		DedupID: settlementID + "-rev", OriginalDedupID: stl.JournalDedupID,
	}); err != nil {
		return nil, fmt.Errorf("reverse posted settlement: %w", err)
	}
	o.logf("settlement %s returned (posted entry reversed): %s", settlementID, reason)
	return o.Engine.GetSettlement(ctx, settlementID)
}
