package fsm

import (
	"fmt"
	"sort"
	"time"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

// maxSettlementTimeoutSeconds caps how long an in-flight settlement may wait
// for a rail outcome before the deterministic sweep expires it.
const maxSettlementTimeoutSeconds = 30 * 24 * 3600

// settlementEntry is a fat-ledger settlement tracked INSIDE the FSM, so the
// in-flight state is raft-durable and survives crashes. Lifecycle:
//
//	pending -> processing -> posted / voided
//	pending/processing -> expired (sweep)
//
// Asymmetric by direction: a withdrawal is OPTIMISTICALLY POSTED at
// mark-processing (debit user, no expirable reservation → no double-spend
// race); failure/return reverses it, and the sweep never expires a posted
// (processing) withdrawal. A deposit records intent without balance until
// settled; the sweep may expire it, but a LATE rail confirmation is still
// honored (expired -> posted) because the money really arrived.
type settlementEntry struct {
	ID             string                       `json:"id"`
	DedupID        string                       `json:"dedup_id"`
	Kind           ledgertypes.SettlementKind   `json:"kind"`
	AccountID      string                       `json:"account_id"`
	FundingAccount string                       `json:"funding_account"`
	Amount         int64                        `json:"amount"`
	Currency       string                       `json:"currency"`
	Status         ledgertypes.SettlementStatus `json:"status"`
	ExpiresAt      time.Time                    `json:"expires_at"`
	JournalDedupID string                       `json:"journal_dedup_id,omitempty"`
}

func settlementToWire(s *settlementEntry) *ledgertypes.Settlement {
	return &ledgertypes.Settlement{
		SettlementID:   s.ID,
		DedupID:        s.DedupID,
		Kind:           s.Kind,
		AccountID:      s.AccountID,
		FundingAccount: s.FundingAccount,
		AmountMinor:    s.Amount,
		Currency:       s.Currency,
		Status:         s.Status,
		ExpiresAt:      s.ExpiresAt,
		JournalDedupID: s.JournalDedupID,
	}
}

// Settlement returns a point-in-time read of a settlement entry.
func (f *FSM) Settlement(id string) (*ledgertypes.Settlement, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	s, ok := f.settlements[id]
	if !ok {
		return nil, false
	}
	return settlementToWire(s), true
}

func settlementEvent(kind ledgertypes.EventKind, s *settlementEntry, dedupID string) *ledgertypes.LedgerEvent {
	return &ledgertypes.LedgerEvent{
		Kind:           kind,
		DedupID:        dedupID,
		SettlementID:   s.ID,
		SettlementKind: s.Kind,
		AccountID:      s.AccountID,
		FundingAccount: s.FundingAccount,
		AmountMinor:    s.Amount,
		Currency:       s.Currency,
	}
}

func (f *FSM) applyCreateSettlementLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.CreateSettlement
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	if req.AmountMinor <= 0 {
		return invalidRequest("amount_minor must be positive"), nil
	}
	if req.Kind != ledgertypes.SettlementDeposit && req.Kind != ledgertypes.SettlementWithdrawal {
		return invalidRequest(fmt.Sprintf("unknown settlement kind %q", req.Kind)), nil
	}
	if req.TimeoutSeconds <= 0 || req.TimeoutSeconds > maxSettlementTimeoutSeconds {
		return invalidRequest(fmt.Sprintf("timeout_seconds must be in (0, %d]", maxSettlementTimeoutSeconds)), nil
	}
	acct, ok := f.accounts[req.AccountID]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("account %q not found", req.AccountID)}, nil
	}
	funding, ok := f.accounts[req.FundingAccount]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("funding account %q not found", req.FundingAccount)}, nil
	}
	if acct.Currency != req.Currency || funding.Currency != req.Currency {
		return invalidRequest("currency mismatch between request and accounts"), nil
	}
	if req.Kind == ledgertypes.SettlementWithdrawal {
		// Fail-fast check only. Optimistic post: NO reservation here — the
		// posted balance moves at mark-processing (submission), so there is no
		// reservation that could expire and rebound available mid-settlement.
		if !acct.Unlimited && acct.available() < req.AmountMinor {
			return &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient available balance", req.AccountID)}, nil
		}
	}

	s := &settlementEntry{
		ID:             fmt.Sprintf("stl-%d", index),
		DedupID:        req.DedupID,
		Kind:           req.Kind,
		AccountID:      req.AccountID,
		FundingAccount: req.FundingAccount,
		Amount:         req.AmountMinor,
		Currency:       req.Currency,
		Status:         ledgertypes.SettlementPending,
		ExpiresAt:      f.now.Add(time.Duration(req.TimeoutSeconds) * time.Second),
	}
	f.settlements[s.ID] = s
	f.dedup[req.DedupID] = dedupEntry{Settlement: s, Fingerprint: fp}
	return &ApplyResult{Settlement: settlementToWire(s)}, settlementEvent(ledgertypes.EventSettlementCreated, s, req.DedupID)
}

func (f *FSM) applyMarkSettlementProcessingLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.SettlementAction
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	s, ok := f.settlements[cmd.SettlementID]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrSettlementNotFound, ErrMsg: fmt.Sprintf("settlement %q not found", cmd.SettlementID)}, nil
	}
	if s.Status != ledgertypes.SettlementPending {
		return invalidRequest(fmt.Sprintf("settlement %q is %s, want pending", s.ID, s.Status)), nil
	}
	if s.Kind == ledgertypes.SettlementWithdrawal {
		// Optimistic post at submission: debit the user now, so the funds are
		// gone from available balance throughout settlement (no expirable
		// reservation). A rail failure reverses this; a return reverses it too.
		acct, funding := f.accounts[s.AccountID], f.accounts[s.FundingAccount]
		if !acct.Unlimited && acct.available() < s.Amount {
			return &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient available balance", s.AccountID)}, nil
		}
		acct.BalanceMinor -= s.Amount
		funding.BalanceMinor += s.Amount
		s.JournalDedupID = s.DedupID + "-posted"
		f.dedup[s.JournalDedupID] = dedupEntry{
			Result: ledgertypes.TransferResult{Status: ledgertypes.TransferStatusApplied, JournalID: fmt.Sprintf("jrn-%d", index), RaftIndex: index},
			Lines:  postedSettlementLegs(s),
		}
	}
	s.Status = ledgertypes.SettlementProcessing
	f.dedup[req.DedupID] = dedupEntry{Settlement: s, Fingerprint: fp}
	return &ApplyResult{Settlement: settlementToWire(s)}, settlementEvent(ledgertypes.EventSettlementProcessing, s, req.DedupID)
}

func (f *FSM) applySettleOutcomeLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.SettlementAction
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	s, ok := f.settlements[cmd.SettlementID]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrSettlementNotFound, ErrMsg: fmt.Sprintf("settlement %q not found", cmd.SettlementID)}, nil
	}
	acct, funding := f.accounts[s.AccountID], f.accounts[s.FundingAccount]

	if s.Kind == ledgertypes.SettlementWithdrawal {
		// Withdrawal was optimistically posted at mark-processing.
		if s.Status != ledgertypes.SettlementProcessing {
			return invalidRequest(fmt.Sprintf("settlement %q is %s, want processing", s.ID, s.Status)), nil
		}
		switch req.Outcome {
		case "settled":
			// Already posted at submission; confirmation moves no balance.
			s.Status = ledgertypes.SettlementPosted
			f.dedup[req.DedupID] = dedupEntry{Settlement: s, Fingerprint: fp}
			return &ApplyResult{Settlement: settlementToWire(s)}, settlementEvent(ledgertypes.EventSettlementPosted, s, req.DedupID)
		case "failed":
			// Reverse the optimistic post: credit the user back.
			acct.BalanceMinor += s.Amount
			funding.BalanceMinor -= s.Amount
			s.Status = ledgertypes.SettlementVoided
			f.dedup[req.DedupID] = dedupEntry{Settlement: s, Fingerprint: fp}
			return &ApplyResult{Settlement: settlementToWire(s)}, settlementEvent(ledgertypes.EventSettlementVoided, s, req.DedupID)
		default:
			return invalidRequest(fmt.Sprintf("outcome %q must be settled or failed", req.Outcome)), nil
		}
	}

	// Deposit: credit only on confirmed settlement. Tolerate a LATE rail
	// confirmation even if the pending deposit already expired — the money
	// really arrived, so it must be credited (expired -> posted).
	switch req.Outcome {
	case "settled":
		if s.Status != ledgertypes.SettlementProcessing && s.Status != ledgertypes.SettlementExpired {
			return invalidRequest(fmt.Sprintf("settlement %q is %s, want processing or expired", s.ID, s.Status)), nil
		}
		funding.BalanceMinor -= s.Amount
		acct.BalanceMinor += s.Amount
		s.Status = ledgertypes.SettlementPosted
		s.JournalDedupID = s.DedupID + "-posted"
		f.dedup[s.JournalDedupID] = dedupEntry{
			Result: ledgertypes.TransferResult{Status: ledgertypes.TransferStatusApplied, JournalID: fmt.Sprintf("jrn-%d", index), RaftIndex: index},
			Lines:  postedSettlementLegs(s),
		}
		f.dedup[req.DedupID] = dedupEntry{Settlement: s, Fingerprint: fp}
		return &ApplyResult{Settlement: settlementToWire(s)}, settlementEvent(ledgertypes.EventSettlementPosted, s, req.DedupID)
	case "failed":
		if s.Status == ledgertypes.SettlementExpired {
			// Already expired and never credited; nothing to undo.
			return &ApplyResult{Settlement: settlementToWire(s)}, nil
		}
		if s.Status != ledgertypes.SettlementProcessing {
			return invalidRequest(fmt.Sprintf("settlement %q is %s, want processing", s.ID, s.Status)), nil
		}
		s.Status = ledgertypes.SettlementVoided
		f.dedup[req.DedupID] = dedupEntry{Settlement: s, Fingerprint: fp}
		return &ApplyResult{Settlement: settlementToWire(s)}, settlementEvent(ledgertypes.EventSettlementVoided, s, req.DedupID)
	default:
		return invalidRequest(fmt.Sprintf("outcome %q must be settled or failed", req.Outcome)), nil
	}
}

func postedSettlementLegs(s *settlementEntry) []ledgertypes.JournalLine {
	if s.Kind == ledgertypes.SettlementWithdrawal {
		return []ledgertypes.JournalLine{
			{AccountID: s.AccountID, Direction: ledgertypes.Debit, AmountMinor: s.Amount},
			{AccountID: s.FundingAccount, Direction: ledgertypes.Credit, AmountMinor: s.Amount},
		}
	}
	return []ledgertypes.JournalLine{
		{AccountID: s.FundingAccount, Direction: ledgertypes.Debit, AmountMinor: s.Amount},
		{AccountID: s.AccountID, Direction: ledgertypes.Credit, AmountMinor: s.Amount},
	}
}

// expireSettlementsLocked releases in-flight settlements whose rail deadline
// passed on the replicated clock. Runs at the start of every Apply (before
// dispatch), deterministic, no wall clock. Released settlements ride on the
// current entry's event (ExpiredSettlements) to preserve one-event-per-entry.
func (f *FSM) expireSettlementsLocked(now time.Time) []ledgertypes.ExpiredSettlement {
	if now.IsZero() || len(f.settlements) == 0 {
		return nil
	}
	ids := make([]string, 0, len(f.settlements))
	for id := range f.settlements {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var expired []ledgertypes.ExpiredSettlement
	for _, id := range ids {
		s := f.settlements[id]
		if s.Status != ledgertypes.SettlementPending && s.Status != ledgertypes.SettlementProcessing {
			continue
		}
		// A withdrawal in processing was optimistically posted at submission;
		// expiring it would credit back funds the rail may still debit, so it
		// is left posted for reconciliation. Only never-submitted (pending)
		// withdrawals expire. Deposits carry no balance until posted, so both
		// pending and processing may expire (a late confirmation re-credits).
		if s.Kind == ledgertypes.SettlementWithdrawal && s.Status == ledgertypes.SettlementProcessing {
			continue
		}
		if s.ExpiresAt.After(now) {
			continue
		}
		s.Status = ledgertypes.SettlementExpired
		expired = append(expired, ledgertypes.ExpiredSettlement{
			SettlementID: s.ID,
			Kind:         s.Kind,
			AccountID:    s.AccountID,
			AmountMinor:  s.Amount,
			Currency:     s.Currency,
		})
	}
	return expired
}
