package recon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/ledgerline/ledgerline-recon/internal/engineclient"
)

// Engine is the ledger API recon needs to post adjustment journals.
type Engine interface {
	CreateAccount(ctx context.Context, req engineclient.CreateAccountRequest) (*engineclient.Account, error)
	GetAccount(ctx context.Context, id string) (*engineclient.Account, error)
	AssertJournal(ctx context.Context, req engineclient.JournalRequest) (*engineclient.TransferResult, error)
}

// Service holds the two sides to reconcile (ledger records and external
// records), runs the match, and can post adjustment journals for breaks.
type Service struct {
	Engine          Engine
	SuspenseAccount string // recon-suspense (park unexplained differences)
	OffsetAccount   string // recon-clearing (balancing side)
	Logger          *log.Logger

	mu         sync.Mutex
	ledger     []Record
	ledgerSeen map[string]bool
	external   []Record
	breaks     []Break
}

// NewService builds a recon service with default suspense accounts.
func NewService(engine Engine) *Service {
	return &Service{Engine: engine, SuspenseAccount: "recon-suspense", OffsetAccount: "recon-clearing", ledgerSeen: make(map[string]bool)}
}

// IngestLedger sets the ledger-side records (manual/API feed; replaces).
func (s *Service) IngestLedger(records []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger = append(s.ledger[:0], records...)
	s.ledgerSeen = make(map[string]bool, len(records))
	for _, r := range records {
		s.ledgerSeen[r.Ref] = true
	}
}

// AddLedgerRecord appends one ledger record idempotently by Ref, for the
// event-stream consumer (replay-safe).
func (s *Service) AddLedgerRecord(r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledgerSeen[r.Ref] {
		return
	}
	s.ledgerSeen[r.Ref] = true
	s.ledger = append(s.ledger, r)
}

// IngestExternal sets the external-side records (scheme clearing file, bank
// statement).
func (s *Service) IngestExternal(records []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.external = append(s.external[:0], records...)
}

// Run matches the two sides and returns the breaks.
func (s *Service) Run() []Break {
	s.mu.Lock()
	ledger := make([]Record, len(s.ledger))
	copy(ledger, s.ledger)
	external := make([]Record, len(s.external))
	copy(external, s.external)
	s.mu.Unlock()

	breaks := Reconcile(ledger, external)
	s.mu.Lock()
	s.breaks = breaks
	s.mu.Unlock()
	if s.Logger != nil {
		s.Logger.Printf("reconciliation: %d ledger vs %d external -> %d breaks", len(ledger), len(external), len(breaks))
	}
	return breaks
}

// LastBreaks returns the most recent run's breaks.
func (s *Service) LastBreaks() []Break {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Break, len(s.breaks))
	copy(out, s.breaks)
	return out
}

// Adjust posts a balancing journal for a missing_in_ledger break: the external
// money arrived but was never recorded, so park it in recon-suspense pending
// investigation (append-only — never mutate history).
func (s *Service) Adjust(ctx context.Context, b Break) error {
	if b.Type != BreakMissingInLedger {
		return fmt.Errorf("adjust only supports missing_in_ledger breaks, got %s", b.Type)
	}
	if err := s.ensureSuspense(ctx, b.Currency); err != nil {
		return err
	}
	_, err := s.Engine.AssertJournal(ctx, engineclient.JournalRequest{
		DedupID: "recon-adj-" + b.Ref,
		Lines: []engineclient.JournalLine{
			{AccountID: s.SuspenseAccount, Direction: "debit", AmountMinor: b.ExternalAmount, GLAccount: "1900"},
			{AccountID: s.OffsetAccount, Direction: "credit", AmountMinor: b.ExternalAmount, GLAccount: "2900"},
		},
	})
	if err != nil {
		return fmt.Errorf("adjustment journal for %s: %w", b.Ref, err)
	}
	if s.Logger != nil {
		s.Logger.Printf("posted adjustment for missing_in_ledger %s (%d %s)", b.Ref, b.ExternalAmount, b.Currency)
	}
	return nil
}

func (s *Service) ensureSuspense(ctx context.Context, currency string) error {
	for _, acct := range []struct {
		id, typ string
	}{{s.SuspenseAccount, "asset"}, {s.OffsetAccount, "liability"}} {
		_, err := s.Engine.GetAccount(ctx, acct.id)
		if err == nil {
			continue
		}
		var le *engineclient.Error
		if !errors.As(err, &le) || le.StatusCode != http.StatusNotFound {
			return err
		}
		_, err = s.Engine.CreateAccount(ctx, engineclient.CreateAccountRequest{
			AccountID: acct.id, Currency: currency, Unlimited: true, Type: acct.typ,
		})
		if err != nil {
			var ce *engineclient.Error
			if errors.As(err, &ce) && ce.Code == "account_exists" {
				continue
			}
			return err
		}
	}
	return nil
}
