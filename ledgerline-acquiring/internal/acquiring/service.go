package acquiring

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/ledgerline/ledgerline-acquiring/internal/engineclient"
	"github.com/ledgerline/ledgerline-acquiring/internal/scheme"
)

// Engine is the ledger API the acquiring service needs. The ledger is the
// fund truth; acquiring records journals into it.
type Engine interface {
	CreateAccount(ctx context.Context, req engineclient.CreateAccountRequest) (*engineclient.Account, error)
	GetAccount(ctx context.Context, id string) (*engineclient.Account, error)
	AssertJournal(ctx context.Context, req engineclient.JournalRequest) (*engineclient.TransferResult, error)
	Reverse(ctx context.Context, req engineclient.ReverseRequest) (*engineclient.TransferResult, error)
}

// Service drives the acquiring flow with dual-write protection:
// durable intent FIRST, then scheme auth/capture, then an idempotent journal
// write that is retried. A crash or a failed journal write never loses the
// knowledge that money was captured — re-driving the intent resumes it.
type Service struct {
	Engine            Engine
	Scheme            scheme.Scheme
	Store             *Store
	ReceivableAccount string // scheme-receivable (unlimited asset)
	PayableAccount    string // merchant-payable (liability)
	FeeAccount        string // acquiring-fee-revenue (revenue)
	JournalRetries    int
	Logger            *log.Logger
}

// NewService builds a service with sane defaults.
func NewService(engine Engine, sch scheme.Scheme, store *Store) *Service {
	return &Service{
		Engine:            engine,
		Scheme:            sch,
		Store:             store,
		ReceivableAccount: "scheme-receivable",
		PayableAccount:    "merchant-payable",
		FeeAccount:        "acquiring-fee-revenue",
		JournalRetries:    5,
	}
}

func (s *Service) logf(format string, args ...interface{}) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

func (s *Service) ensureAccount(ctx context.Context, id, currency, typ string, unlimited bool) error {
	_, err := s.Engine.GetAccount(ctx, id)
	if err == nil {
		return nil
	}
	var le *engineclient.Error
	if !errors.As(err, &le) || le.StatusCode != http.StatusNotFound {
		return err
	}
	_, err = s.Engine.CreateAccount(ctx, engineclient.CreateAccountRequest{
		AccountID: id, Currency: currency, Unlimited: unlimited, Type: typ,
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

func (s *Service) ensureAccounts(ctx context.Context, currency string) error {
	if err := s.ensureAccount(ctx, s.ReceivableAccount, currency, "asset", true); err != nil {
		return err
	}
	if err := s.ensureAccount(ctx, s.PayableAccount, currency, "liability", false); err != nil {
		return err
	}
	return s.ensureAccount(ctx, s.FeeAccount, currency, "revenue", false)
}

// CreatePayment processes a card payment. It is idempotent on dedupID and
// RESUMABLE: re-driving an existing intent continues from its current status,
// so a failed journal write after capture can be retried without double-charging.
func (s *Service) CreatePayment(ctx context.Context, dedupID, merchantID string, amount, fee int64, currency string) (*PaymentIntent, error) {
	pi, _, err := s.Store.Create(dedupID, merchantID, amount, fee, currency)
	if err != nil {
		return nil, err
	}
	// Terminal states: nothing to resume.
	if pi.Status == StatusCaptured || pi.Status == StatusFailed || pi.Status == StatusVoided {
		return pi, nil
	}
	if err := s.ensureAccounts(ctx, currency); err != nil {
		return nil, err
	}

	// Step 1: auth (only if still pending).
	if pi.Status == StatusPending {
		auth := s.Scheme.Auth(amount, currency)
		if !auth.OK {
			s.Store.Transition(pi.ID, StatusFailed, func(p *PaymentIntent) { p.FailureReason = auth.Reason })
			return nil, fmt.Errorf("auth declined: %s", auth.Reason)
		}
		pi, err = s.Store.Transition(pi.ID, StatusAuthorized, func(p *PaymentIntent) { p.SchemeRef = auth.Ref })
		if err != nil {
			return nil, err
		}
	}

	// Step 2: capture (idempotent at the scheme) then record the journal.
	cap := s.Scheme.Capture(amount, currency)
	if !cap.OK {
		s.Store.Transition(pi.ID, StatusFailed, func(p *PaymentIntent) { p.FailureReason = cap.Reason })
		return nil, fmt.Errorf("capture declined: %s", cap.Reason)
	}

	// Step 3: idempotent journal write, retried. The durable intent + capture
	// are already recorded, so this can be re-driven safely.
	journalDedup := dedupID + "-journal"
	if err := s.recordJournal(ctx, journalDedup, pi); err != nil {
		// Capture succeeded but the ledger write failed: intent stays
		// authorized (money in flight, durably known). Re-drive retries.
		return pi, fmt.Errorf("journal write failed (capture done, re-drive to retry): %w", err)
	}

	pi, err = s.Store.Transition(pi.ID, StatusCaptured, func(p *PaymentIntent) { p.JournalDedupID = journalDedup })
	if err != nil {
		return nil, err
	}
	s.logf("payment %s captured + journal recorded (%d %s, fee %d)", pi.ID, amount, currency, fee)
	return pi, nil
}

// recordJournal writes the acquiring journal with retries. Idempotent via
// journalDedup, so retries never double-record.
func (s *Service) recordJournal(ctx context.Context, journalDedup string, pi *PaymentIntent) error {
	lines := []engineclient.JournalLine{
		{AccountID: s.ReceivableAccount, Direction: "debit", AmountMinor: pi.AmountMinor, GLAccount: "1100"},
		{AccountID: s.PayableAccount, Direction: "credit", AmountMinor: pi.AmountMinor - pi.FeeMinor, GLAccount: "2200"},
	}
	if pi.FeeMinor > 0 {
		lines = append(lines, engineclient.JournalLine{
			AccountID: s.FeeAccount, Direction: "credit", AmountMinor: pi.FeeMinor, GLAccount: "4000",
		})
	}
	req := engineclient.JournalRequest{DedupID: journalDedup, Lines: lines}

	var lastErr error
	for attempt := 0; attempt < s.JournalRetries; attempt++ {
		_, err := s.Engine.AssertJournal(ctx, req)
		if err == nil {
			return nil
		}
		lastErr = err
		// Terminal business errors (e.g. unbalanced) won't heal on retry.
		var le *engineclient.Error
		if errors.As(err, &le) && le.StatusCode != http.StatusBadGateway && le.StatusCode != http.StatusServiceUnavailable {
			return err
		}
	}
	return lastErr
}

// VoidPayment cancels an intent before capture. If already captured, it
// reverses the recorded journal (refund).
func (s *Service) VoidPayment(ctx context.Context, id string) (*PaymentIntent, error) {
	pi, ok := s.Store.Get(id)
	if !ok {
		return nil, fmt.Errorf("intent %q not found", id)
	}
	if pi.Status == StatusCaptured {
		if _, err := s.Engine.Reverse(ctx, engineclient.ReverseRequest{
			DedupID: pi.DedupID + "-rev", OriginalDedupID: pi.JournalDedupID,
		}); err != nil {
			return nil, fmt.Errorf("reverse captured journal: %w", err)
		}
	}
	return s.Store.Transition(id, StatusVoided, nil)
}
