package clearing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"

	"github.com/ledgerline/ledgerline-clearing/internal/engineclient"
)

// Engine is the ledger API clearing needs.
type Engine interface {
	CreateAccount(ctx context.Context, req engineclient.CreateAccountRequest) (*engineclient.Account, error)
	GetAccount(ctx context.Context, id string) (*engineclient.Account, error)
	AssertJournal(ctx context.Context, req engineclient.JournalRequest) (*engineclient.TransferResult, error)
}

// Service accumulates gross captures and, on a clearing run, nets them and
// records a clearing journal that moves the net obligation from the merchant
// payable to a clearing-suspense account, ready for settlement payout.
type Service struct {
	Engine          Engine
	PayableAccount  string // merchant-payable (gross, credited by acquiring)
	SuspenseAccount string // clearing-suspense (net, ready to settle)
	Logger          *log.Logger

	mu      sync.Mutex
	pending []Capture
	seenRef map[string]bool
	batches []Batch
	seq     int64
}

// NewService builds a clearing service with default account names.
func NewService(engine Engine) *Service {
	return &Service{
		Engine:          engine,
		PayableAccount:  "merchant-payable",
		SuspenseAccount: "clearing-suspense",
		seenRef:         make(map[string]bool),
	}
}

// Ingest accumulates a gross capture (or refund) for the next clearing run.
// Idempotent on Ref so event-stream replay never double-counts a capture.
func (s *Service) Ingest(c Capture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Ref != "" {
		if s.seenRef[c.Ref] {
			return
		}
		s.seenRef[c.Ref] = true
	}
	s.pending = append(s.pending, c)
}

// Pending returns a copy of the not-yet-cleared captures.
func (s *Service) Pending() []Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Capture, len(s.pending))
	copy(out, s.pending)
	return out
}

// Batches returns recorded clearing batches.
func (s *Service) Batches() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Batch, len(s.batches))
	copy(out, s.batches)
	return out
}

// RunClearing nets all pending captures, records the clearing journal(s), and
// returns the batch. Idempotent per batch via the journal dedup id.
func (s *Service) RunClearing(ctx context.Context) (*Batch, error) {
	s.mu.Lock()
	captures := s.pending
	s.pending = nil
	s.seq++
	batchID := fmt.Sprintf("clr-%d", s.seq)
	s.mu.Unlock()

	positions := Net(captures)
	if len(positions) == 0 {
		b := Batch{BatchID: batchID, Positions: positions, Count: 0}
		s.appendBatch(b)
		return &b, nil
	}

	// Aggregate net per currency for the clearing journal.
	perCcy := make(map[string]int64)
	for _, p := range positions {
		perCcy[p.Currency] += p.NetMinor
	}
	ccys := make([]string, 0, len(perCcy))
	for c := range perCcy {
		ccys = append(ccys, c)
	}
	sort.Strings(ccys)

	for _, ccy := range ccys {
		net := perCcy[ccy]
		if net == 0 {
			continue
		}
		if err := s.ensureSuspense(ctx, ccy); err != nil {
			return nil, err
		}
		// Netting reclassification: Dr merchant-payable / Cr clearing-suspense.
		_, err := s.Engine.AssertJournal(ctx, engineclient.JournalRequest{
			DedupID: batchID + "-" + ccy,
			Lines: []engineclient.JournalLine{
				{AccountID: s.PayableAccount, Direction: "debit", AmountMinor: net, GLAccount: "2200"},
				{AccountID: s.SuspenseAccount, Direction: "credit", AmountMinor: net, GLAccount: "2300"},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("clearing journal %s/%s: %w", batchID, ccy, err)
		}
	}

	b := Batch{BatchID: batchID, Positions: positions, Count: len(captures)}
	s.appendBatch(b)
	if s.Logger != nil {
		s.Logger.Printf("clearing batch %s: %d captures -> %d net positions", batchID, len(captures), len(positions))
	}
	return &b, nil
}

func (s *Service) appendBatch(b Batch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, b)
}

func (s *Service) ensureSuspense(ctx context.Context, currency string) error {
	_, err := s.Engine.GetAccount(ctx, s.SuspenseAccount)
	if err == nil {
		return nil
	}
	var le *engineclient.Error
	if !errors.As(err, &le) || le.StatusCode != http.StatusNotFound {
		return err
	}
	_, err = s.Engine.CreateAccount(ctx, engineclient.CreateAccountRequest{
		AccountID: s.SuspenseAccount, Currency: currency, Unlimited: true, Type: "liability",
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
