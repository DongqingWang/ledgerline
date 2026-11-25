// Package fsm implements the ledgerline-raft replicated state machine: the
// event-sourced heart of the engine. Every balance change is first appended
// to the raft log (by the leader, replicated to a majority) and only then
// applied here — on every node, in the same deterministic order — which is
// what makes the committed log itself the durable, tamper-evident audit
// trail rather than a side effect of it.
package fsm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

// CommandType distinguishes the mutating operations the FSM understands.
type CommandType string

const (
	CmdCreateAccount  CommandType = "create_account"
	CmdAssertTransfer CommandType = "assert_transfer"
	CmdAssertJournal  CommandType = "assert_journal"
	CmdReverse        CommandType = "reverse"
	CmdCreatePending  CommandType = "create_pending"
	CmdPostPending    CommandType = "post_pending"
	CmdVoidPending    CommandType = "void_pending"

	CmdCreateSettlement         CommandType = "create_settlement"
	CmdMarkSettlementProcessing CommandType = "mark_settlement_processing"
	CmdSettleOutcome            CommandType = "settle_outcome"
)

// Command is the single envelope type serialized into every raft log entry.
// Keeping one envelope (rather than raw per-command bytes) makes it trivial
// to add new command types later without touching the raft plumbing.
type Command struct {
	Type CommandType `json:"type"`
	// Timestamp is stamped by the leader's HTTP layer at admission time and
	// replicated inside the log entry. It is the ONLY clock Apply consults —
	// calling time.Now() inside Apply would diverge across replicas.
	Timestamp time.Time `json:"ts,omitempty"`
	// TenantID is assigned server-side from the authenticated caller, never
	// trusted from a request body.
	TenantID string `json:"tenant_id,omitempty"`

	CreateAccount  *ledgertypes.CreateAccountRequest   `json:"create_account,omitempty"`
	AssertTransfer *ledgertypes.TransferRequest        `json:"assert_transfer,omitempty"`
	AssertJournal  *ledgertypes.JournalRequest         `json:"assert_journal,omitempty"`
	Reverse        *ledgertypes.ReverseRequest         `json:"reverse,omitempty"`
	CreatePending  *ledgertypes.PendingTransferRequest `json:"create_pending,omitempty"`
	PendingAction  *ledgertypes.PendingActionRequest   `json:"pending_action,omitempty"`
	PendingID      string                              `json:"pending_id,omitempty"`

	CreateSettlement *ledgertypes.CreateSettlementRequest `json:"create_settlement,omitempty"`
	SettlementAction *ledgertypes.SettlementActionRequest `json:"settlement_action,omitempty"`
	SettlementID     string                               `json:"settlement_id,omitempty"`
}

// Encode serializes a Command for raft.Raft.Apply.
func (c Command) Encode() ([]byte, error) { return json.Marshal(c) }

// ApplyResult is what FSM.Apply returns (retrieved via raft's ApplyFuture.Response()).
// Business-rule failures (account not found, insufficient funds, ...) are
// reported here rather than as a Go error, because raft.FSM.Apply must never
// return differently across replicas based on something non-deterministic —
// an error value here is just data, computed identically on every node.
type ApplyResult struct {
	Account    *ledgertypes.Account         `json:"account,omitempty"`
	Transfer   *ledgertypes.TransferResult  `json:"transfer,omitempty"`
	Pending    *ledgertypes.PendingTransfer `json:"pending,omitempty"`
	Settlement *ledgertypes.Settlement      `json:"settlement,omitempty"`
	ErrCode    ledgertypes.ErrorCode        `json:"err_code,omitempty"`
	ErrMsg     string                       `json:"err_msg,omitempty"`
}

func (r *ApplyResult) IsError() bool { return r.ErrCode != "" }

type account struct {
	Currency      string                  `json:"currency"`
	BalanceMinor  int64                   `json:"balance_minor"`
	PendingDebits int64                   `json:"pending_debits"`
	Unlimited     bool                    `json:"unlimited"`
	Type          ledgertypes.AccountType `json:"type"`
	TenantID      string                  `json:"tenant_id"`
}

func (a *account) available() int64 { return a.BalanceMinor - a.PendingDebits }

type pending struct {
	ID        string    `json:"id"`
	DedupID   string    `json:"dedup_id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Amount    int64     `json:"amount"`
	Currency  string    `json:"currency"`
	ExpiresAt time.Time `json:"expires_at"`
}

// dedupEntry records everything needed to make a DedupID exactly-once AND
// conflict-safe: the original result, a fingerprint of the original
// parameters (same key + different params = 409, not a silent replay), the
// normalized journal lines (for reversals), and whether it was reversed.
type dedupEntry struct {
	Result      ledgertypes.TransferResult `json:"result"`
	Pending     *pending                   `json:"pending,omitempty"`
	Settlement  *settlementEntry           `json:"settlement,omitempty"`
	Fingerprint string                     `json:"fingerprint"`
	Lines       []ledgertypes.JournalLine  `json:"lines,omitempty"`
	Reversed    bool                       `json:"reversed,omitempty"`
}

// FSM is the raft.FSM implementation. All mutation happens inside Apply,
// which raft guarantees is called sequentially and deterministically on
// every replica for every committed log entry.
type FSM struct {
	mu          sync.RWMutex
	accounts    map[string]*account
	pendings    map[string]*pending
	settlements map[string]*settlementEntry
	dedup       map[string]dedupEntry
	// now is the replicated logical clock: the timestamp of the most recent
	// log entry that carried one. Hold expiry is evaluated against it, never
	// against a replica's wall clock.
	now time.Time

	events *EventBus
}

// New builds an FSM. eventLogPath, when non-empty, makes the event bus
// durable (append-only ndjson replayed on startup); "" keeps it in-memory,
// which is what unit tests use.
func New(eventLogPath string) *FSM {
	return &FSM{
		accounts:    make(map[string]*account),
		pendings:    make(map[string]*pending),
		settlements: make(map[string]*settlementEntry),
		dedup:       make(map[string]dedupEntry),
		events:      NewEventBus(eventLogPath),
	}
}

// Close releases the event bus's durable log file, if any.
func (f *FSM) Close() error { return f.events.Close() }

// Events exposes the event bus so the HTTP layer can let clients subscribe
// to the committed-event stream (the CQRS tap used by balance-reader and
// persistence-sync).
func (f *FSM) Events() *EventBus { return f.events }

func accountToWire(id string, a *account) *ledgertypes.Account {
	return &ledgertypes.Account{
		AccountID:          id,
		Currency:           a.Currency,
		BalanceMinor:       a.BalanceMinor,
		PendingDebitsMinor: a.PendingDebits,
		AvailableMinor:     a.available(),
		Unlimited:          a.Unlimited,
		Type:               a.Type,
		TenantID:           a.TenantID,
	}
}

// Account returns a point-in-time read of local state. On a follower this
// can be slightly behind the leader (it reflects whatever this replica has
// applied so far) — callers that need a linearizable read should go through
// the leader's write path instead.
func (f *FSM) Account(id string) (*ledgertypes.Account, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	a, ok := f.accounts[id]
	if !ok {
		return nil, false
	}
	return accountToWire(id, a), true
}

// Pending returns a point-in-time read of an active hold, for the HTTP
// layer's tenant-ownership pre-checks.
func (f *FSM) Pending(id string) (*ledgertypes.PendingTransfer, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	p, ok := f.pendings[id]
	if !ok {
		return nil, false
	}
	return pendingToWire(p), true
}

// OriginalLines returns the normalized legs of a previously applied
// transfer/journal, for the HTTP layer's tenant-ownership pre-checks on
// reversals.
func (f *FSM) OriginalLines(dedupID string) ([]ledgertypes.JournalLine, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	e, ok := f.dedup[dedupID]
	if !ok || len(e.Lines) == 0 {
		return nil, false
	}
	out := make([]ledgertypes.JournalLine, len(e.Lines))
	copy(out, e.Lines)
	return out, true
}

func pendingToWire(p *pending) *ledgertypes.PendingTransfer {
	return &ledgertypes.PendingTransfer{
		PendingID:   p.ID,
		DedupID:     p.DedupID,
		FromAccount: p.From,
		ToAccount:   p.To,
		AmountMinor: p.Amount,
		Currency:    p.Currency,
		ExpiresAt:   p.ExpiresAt,
	}
}

// Apply implements raft.FSM. Exactly one event is published per log entry —
// including rejections (command_rejected) and idempotent duplicates
// (command_duplicate) — so the event stream is 1:1 with raft indices and
// consumers can detect delivery gaps by checking index contiguity.
func (f *FSM) Apply(log *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		// A malformed log entry would be a programming bug in the writer, not
		// a recoverable business error — but we still must not panic, since
		// that would crash this replica while others continue, causing
		// divergence. Surface it as a result instead.
		f.mu.Lock()
		defer f.mu.Unlock()
		res := &ApplyResult{ErrCode: ledgertypes.ErrInternal, ErrMsg: fmt.Sprintf("decode command: %v", err)}
		f.publishLocked(log.Index, rejectedEvent(res, ""), nil, nil)
		return res
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if !cmd.Timestamp.IsZero() {
		f.now = cmd.Timestamp
	}
	expired := f.expirePendingsLocked(f.now)
	expiredStl := f.expireSettlementsLocked(f.now)

	res, ev := f.dispatchLocked(log.Index, cmd)
	if ev == nil {
		if res.IsError() {
			ev = rejectedEvent(res, cmdDedupID(cmd))
		} else {
			ev = &ledgertypes.LedgerEvent{Kind: ledgertypes.EventCommandDuplicate, DedupID: cmdDedupID(cmd)}
		}
	}
	f.publishLocked(log.Index, ev, expired, expiredStl)
	return res
}

func rejectedEvent(res *ApplyResult, dedupID string) *ledgertypes.LedgerEvent {
	return &ledgertypes.LedgerEvent{
		Kind:    ledgertypes.EventCommandRejected,
		DedupID: dedupID,
		ErrCode: res.ErrCode,
		ErrMsg:  res.ErrMsg,
	}
}

func cmdDedupID(cmd Command) string {
	switch {
	case cmd.AssertTransfer != nil:
		return cmd.AssertTransfer.DedupID
	case cmd.AssertJournal != nil:
		return cmd.AssertJournal.DedupID
	case cmd.Reverse != nil:
		return cmd.Reverse.DedupID
	case cmd.CreatePending != nil:
		return cmd.CreatePending.DedupID
	case cmd.PendingAction != nil:
		return cmd.PendingAction.DedupID
	case cmd.CreateSettlement != nil:
		return cmd.CreateSettlement.DedupID
	case cmd.SettlementAction != nil:
		return cmd.SettlementAction.DedupID
	}
	return ""
}

func (f *FSM) publishLocked(index uint64, ev *ledgertypes.LedgerEvent, expired []ledgertypes.ExpiredPending, expiredStl []ledgertypes.ExpiredSettlement) {
	ev.RaftIndex = index
	ev.Timestamp = f.now
	if len(expired) > 0 {
		ev.ExpiredPendings = expired
	}
	if len(expiredStl) > 0 {
		ev.ExpiredSettlements = expiredStl
	}
	f.events.Publish(*ev)
}

func (f *FSM) dispatchLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	switch cmd.Type {
	case CmdCreateAccount:
		if cmd.CreateAccount == nil {
			return invalidRequest("missing create_account payload"), nil
		}
		return f.applyCreateAccountLocked(cmd)
	case CmdAssertTransfer:
		if cmd.AssertTransfer == nil {
			return invalidRequest("missing assert_transfer payload"), nil
		}
		return f.applyAssertTransferLocked(index, cmd)
	case CmdAssertJournal:
		if cmd.AssertJournal == nil {
			return invalidRequest("missing assert_journal payload"), nil
		}
		return f.applyAssertJournalLocked(index, cmd)
	case CmdReverse:
		if cmd.Reverse == nil {
			return invalidRequest("missing reverse payload"), nil
		}
		return f.applyReverseLocked(index, cmd)
	case CmdCreatePending:
		if cmd.CreatePending == nil {
			return invalidRequest("missing create_pending payload"), nil
		}
		return f.applyCreatePendingLocked(index, cmd)
	case CmdPostPending:
		if cmd.PendingAction == nil {
			return invalidRequest("missing pending_action payload"), nil
		}
		return f.applyPostPendingLocked(index, cmd)
	case CmdVoidPending:
		if cmd.PendingAction == nil {
			return invalidRequest("missing pending_action payload"), nil
		}
		return f.applyVoidPendingLocked(index, cmd)
	case CmdCreateSettlement:
		if cmd.CreateSettlement == nil {
			return invalidRequest("missing create_settlement payload"), nil
		}
		return f.applyCreateSettlementLocked(index, cmd)
	case CmdMarkSettlementProcessing:
		if cmd.SettlementAction == nil {
			return invalidRequest("missing settlement_action payload"), nil
		}
		return f.applyMarkSettlementProcessingLocked(index, cmd)
	case CmdSettleOutcome:
		if cmd.SettlementAction == nil {
			return invalidRequest("missing settlement_action payload"), nil
		}
		return f.applySettleOutcomeLocked(index, cmd)
	default:
		return invalidRequest(fmt.Sprintf("unknown command type %q", cmd.Type)), nil
	}
}

func invalidRequest(msg string) *ApplyResult {
	return &ApplyResult{ErrCode: ledgertypes.ErrInvalidRequest, ErrMsg: msg}
}

// fingerprint hashes the command's parameters with the DedupID removed, so
// the same key submitted with different parameters is detectable as a
// conflict instead of silently replaying the original result.
func fingerprint(cmd Command) string {
	var payload interface{}
	switch cmd.Type {
	case CmdAssertTransfer:
		r := *cmd.AssertTransfer
		r.DedupID = ""
		payload = r
	case CmdAssertJournal:
		r := *cmd.AssertJournal
		r.DedupID = ""
		payload = r
	case CmdReverse:
		payload = struct{ OriginalDedupID string }{cmd.Reverse.OriginalDedupID}
	case CmdCreatePending:
		r := *cmd.CreatePending
		r.DedupID = ""
		payload = r
	case CmdPostPending, CmdVoidPending:
		payload = struct {
			Action    CommandType `json:"action"`
			PendingID string      `json:"pending_id"`
		}{cmd.Type, cmd.PendingID}
	case CmdCreateSettlement:
		r := *cmd.CreateSettlement
		r.DedupID = ""
		payload = r
	case CmdMarkSettlementProcessing, CmdSettleOutcome:
		payload = struct {
			Action       CommandType `json:"action"`
			SettlementID string      `json:"settlement_id"`
			Outcome      string      `json:"outcome,omitempty"`
		}{cmd.Type, cmd.SettlementID, cmd.SettlementAction.Outcome}
	default:
		payload = cmd.CreateAccount
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte(string(cmd.Type)+"|"), b...))
	return hex.EncodeToString(sum[:])
}

// checkDedupLocked returns a non-nil result when the DedupID was already
// processed (duplicate replay or parameter conflict) and the caller must
// stop. A nil result means "first time seen, proceed".
func (f *FSM) checkDedupLocked(dedupID, fp string) *ApplyResult {
	e, seen := f.dedup[dedupID]
	if !seen {
		return nil
	}
	if e.Fingerprint != fp {
		return &ApplyResult{
			ErrCode: ledgertypes.ErrConflict,
			ErrMsg:  fmt.Sprintf("dedup_id %q already used with different parameters", dedupID),
		}
	}
	if e.Settlement != nil {
		cp := *e.Settlement
		return &ApplyResult{Settlement: settlementToWire(&cp)}
	}
	if e.Pending != nil {
		cp := *e.Pending
		return &ApplyResult{Pending: pendingToWire(&cp)}
	}
	dup := e.Result
	dup.Status = ledgertypes.TransferStatusDuplicate
	return &ApplyResult{Transfer: &dup}
}

func (f *FSM) applyCreateAccountLocked(cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.CreateAccount
	if !ledgertypes.ValidAccountType(req.Type) {
		return invalidRequest(fmt.Sprintf("unknown account type %q", req.Type)), nil
	}
	if _, exists := f.accounts[req.AccountID]; exists {
		return &ApplyResult{ErrCode: ledgertypes.ErrAccountExists, ErrMsg: fmt.Sprintf("account %q already exists", req.AccountID)}, nil
	}
	a := &account{Currency: req.Currency, Unlimited: req.Unlimited, Type: req.Type, TenantID: cmd.TenantID}
	f.accounts[req.AccountID] = a

	ev := &ledgertypes.LedgerEvent{
		Kind:        ledgertypes.EventAccountCreated,
		AccountID:   req.AccountID,
		Currency:    req.Currency,
		AccountType: req.Type,
		TenantID:    cmd.TenantID,
	}
	return &ApplyResult{Account: accountToWire(req.AccountID, a)}, ev
}

func (f *FSM) applyAssertTransferLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.AssertTransfer
	if res := f.checkDedupLocked(req.DedupID, fingerprint(cmd)); res != nil {
		return res, nil
	}
	if req.AmountMinor <= 0 {
		return invalidRequest("amount_minor must be positive"), nil
	}
	from, to, res := f.pairLocked(req.FromAccount, req.ToAccount, req.Currency)
	if res != nil {
		return res, nil
	}
	if !from.Unlimited && from.available() < req.AmountMinor {
		return &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient available balance", req.FromAccount)}, nil
	}

	// The double-entry invariant: the amount debited from one side always
	// equals the amount credited to the other, by construction.
	from.BalanceMinor -= req.AmountMinor
	to.BalanceMinor += req.AmountMinor

	journalID := fmt.Sprintf("jrn-%d", index)
	result := ledgertypes.TransferResult{Status: ledgertypes.TransferStatusApplied, JournalID: journalID, RaftIndex: index}
	lines := []ledgertypes.JournalLine{
		{AccountID: req.FromAccount, Direction: ledgertypes.Debit, AmountMinor: req.AmountMinor, GLAccount: req.GLAccount, CostCenter: req.CostCenter},
		{AccountID: req.ToAccount, Direction: ledgertypes.Credit, AmountMinor: req.AmountMinor, GLAccount: req.GLAccount, CostCenter: req.CostCenter},
	}
	f.dedup[req.DedupID] = dedupEntry{Result: result, Fingerprint: fingerprint(cmd), Lines: lines}

	ev := &ledgertypes.LedgerEvent{
		Kind:        ledgertypes.EventTransferApplied,
		DedupID:     req.DedupID,
		JournalID:   journalID,
		FromAccount: req.FromAccount,
		ToAccount:   req.ToAccount,
		AmountMinor: req.AmountMinor,
		Currency:    req.Currency,
		GLAccount:   req.GLAccount,
		CostCenter:  req.CostCenter,
	}
	return &ApplyResult{Transfer: &result}, ev
}

// pairLocked resolves and currency-checks a from/to account pair.
func (f *FSM) pairLocked(fromID, toID, currency string) (*account, *account, *ApplyResult) {
	from, ok := f.accounts[fromID]
	if !ok {
		return nil, nil, &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("from_account %q not found", fromID)}
	}
	to, ok := f.accounts[toID]
	if !ok {
		return nil, nil, &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("to_account %q not found", toID)}
	}
	if from.Currency != currency || to.Currency != currency {
		return nil, nil, invalidRequest("currency mismatch between request and accounts")
	}
	return from, to, nil
}

func (f *FSM) applyAssertJournalLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.AssertJournal
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	return f.applyJournalLocked(index, req.DedupID, req.Lines, fp, "")
}

// validateJournalLocked checks the multi-leg invariants WITHOUT mutating:
// every leg references an existing account with a valid direction and
// positive amount; debits equal credits independently per currency; and
// each account's aggregate net debit fits its available balance. All map
// iteration that can surface in error messages is sorted so replicas
// produce byte-identical results.
func (f *FSM) validateJournalLocked(lines []ledgertypes.JournalLine) (map[string]int64, *ApplyResult) {
	if len(lines) < 2 {
		return nil, invalidRequest("a journal needs at least two lines")
	}
	perCurrency := make(map[string][2]int64) // currency -> [debits, credits]
	net := make(map[string]int64)            // account -> net debit (credit negative)
	for i, ln := range lines {
		if ln.AmountMinor <= 0 {
			return nil, invalidRequest(fmt.Sprintf("line %d: amount_minor must be positive", i))
		}
		if !ledgertypes.ValidDirection(ln.Direction) {
			return nil, invalidRequest(fmt.Sprintf("line %d: invalid direction %q", i, ln.Direction))
		}
		a, ok := f.accounts[ln.AccountID]
		if !ok {
			return nil, &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("line %d: account %q not found", i, ln.AccountID)}
		}
		sums := perCurrency[a.Currency]
		if ln.Direction == ledgertypes.Debit {
			sums[0] += ln.AmountMinor
			net[ln.AccountID] += ln.AmountMinor
		} else {
			sums[1] += ln.AmountMinor
			net[ln.AccountID] -= ln.AmountMinor
		}
		perCurrency[a.Currency] = sums
	}
	currencies := make([]string, 0, len(perCurrency))
	for c := range perCurrency {
		currencies = append(currencies, c)
	}
	sort.Strings(currencies)
	for _, c := range currencies {
		sums := perCurrency[c]
		if sums[0] != sums[1] {
			return nil, &ApplyResult{
				ErrCode: ledgertypes.ErrUnbalancedJournal,
				ErrMsg:  fmt.Sprintf("currency %s: debits %d != credits %d", c, sums[0], sums[1]),
			}
		}
	}
	ids := make([]string, 0, len(net))
	for id := range net {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if net[id] <= 0 {
			continue
		}
		a := f.accounts[id]
		if !a.Unlimited && a.available() < net[id] {
			return nil, &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient available balance for net debit %d", id, net[id])}
		}
	}
	return net, nil
}

// applyJournalLocked validates and applies a journal atomically: either all
// legs commit in this single log entry or none do. Shared by AssertJournal
// and Reverse.
func (f *FSM) applyJournalLocked(index uint64, dedupID string, lines []ledgertypes.JournalLine, fp, reversalOf string) (*ApplyResult, *ledgertypes.LedgerEvent) {
	net, res := f.validateJournalLocked(lines)
	if res != nil {
		return res, nil
	}
	for id, n := range net {
		f.accounts[id].BalanceMinor -= n
	}
	journalID := fmt.Sprintf("jrn-%d", index)
	result := ledgertypes.TransferResult{Status: ledgertypes.TransferStatusApplied, JournalID: journalID, RaftIndex: index}
	stored := make([]ledgertypes.JournalLine, len(lines))
	copy(stored, lines)
	f.dedup[dedupID] = dedupEntry{Result: result, Fingerprint: fp, Lines: stored}

	evLines := make([]ledgertypes.EventJournalLine, len(lines))
	for i, ln := range lines {
		evLines[i] = ledgertypes.EventJournalLine{
			AccountID:   ln.AccountID,
			Direction:   ln.Direction,
			AmountMinor: ln.AmountMinor,
			Currency:    f.accounts[ln.AccountID].Currency,
			GLAccount:   ln.GLAccount,
			CostCenter:  ln.CostCenter,
		}
	}
	ev := &ledgertypes.LedgerEvent{
		Kind:       ledgertypes.EventJournalApplied,
		DedupID:    dedupID,
		JournalID:  journalID,
		Lines:      evLines,
		ReversalOf: reversalOf,
	}
	return &ApplyResult{Transfer: &result}, ev
}

func (f *FSM) applyReverseLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.Reverse
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	orig, ok := f.dedup[req.OriginalDedupID]
	if !ok || len(orig.Lines) == 0 {
		return invalidRequest(fmt.Sprintf("original dedup_id %q not found or not reversible", req.OriginalDedupID)), nil
	}
	if orig.Reversed {
		return &ApplyResult{ErrCode: ledgertypes.ErrAlreadyReversed, ErrMsg: fmt.Sprintf("dedup_id %q was already reversed", req.OriginalDedupID)}, nil
	}
	flipped := make([]ledgertypes.JournalLine, len(orig.Lines))
	for i, ln := range orig.Lines {
		flipped[i] = ln
		if ln.Direction == ledgertypes.Debit {
			flipped[i].Direction = ledgertypes.Credit
		} else {
			flipped[i].Direction = ledgertypes.Debit
		}
	}
	// The reversal goes through the same balance checks as any journal —
	// reversing a credit can legitimately fail on insufficient funds.
	// reversal_of carries the original entry's DedupID: the stable business
	// key callers know and the only entry identifier in the durable
	// projection (journal_entries.transfer_dedup_id).
	res, ev := f.applyJournalLocked(index, req.DedupID, flipped, fp, req.OriginalDedupID)
	if res.IsError() {
		return res, nil
	}
	orig.Reversed = true
	f.dedup[req.OriginalDedupID] = orig
	return res, ev
}

// maxPendingTimeoutSeconds caps holds at 30 days so a forgotten hold cannot
// reserve funds forever.
const maxPendingTimeoutSeconds = 30 * 24 * 3600

func (f *FSM) applyCreatePendingLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.CreatePending
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	if req.AmountMinor <= 0 {
		return invalidRequest("amount_minor must be positive"), nil
	}
	if req.TimeoutSeconds <= 0 || req.TimeoutSeconds > maxPendingTimeoutSeconds {
		return invalidRequest(fmt.Sprintf("timeout_seconds must be in (0, %d]", maxPendingTimeoutSeconds)), nil
	}
	from, _, res := f.pairLocked(req.FromAccount, req.ToAccount, req.Currency)
	if res != nil {
		return res, nil
	}
	if !from.Unlimited && from.available() < req.AmountMinor {
		return &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient available balance", req.FromAccount)}, nil
	}

	p := &pending{
		ID:        fmt.Sprintf("pnd-%d", index),
		DedupID:   req.DedupID,
		From:      req.FromAccount,
		To:        req.ToAccount,
		Amount:    req.AmountMinor,
		Currency:  req.Currency,
		ExpiresAt: f.now.Add(time.Duration(req.TimeoutSeconds) * time.Second),
	}
	f.pendings[p.ID] = p
	from.PendingDebits += req.AmountMinor
	f.dedup[req.DedupID] = dedupEntry{Pending: p, Fingerprint: fp}

	expiresAt := p.ExpiresAt
	ev := &ledgertypes.LedgerEvent{
		Kind:        ledgertypes.EventPendingCreated,
		PendingID:   p.ID,
		DedupID:     req.DedupID,
		FromAccount: p.From,
		ToAccount:   p.To,
		AmountMinor: p.Amount,
		Currency:    p.Currency,
		ExpiresAt:   &expiresAt,
	}
	return &ApplyResult{Pending: pendingToWire(p)}, ev
}

func (f *FSM) applyPostPendingLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.PendingAction
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	p, ok := f.pendings[cmd.PendingID]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrPendingNotFound, ErrMsg: fmt.Sprintf("pending %q not found (posted, voided, or expired)", cmd.PendingID)}, nil
	}
	from := f.accounts[p.From]
	to := f.accounts[p.To]
	if !from.Unlimited && from.BalanceMinor < p.Amount {
		return &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient balance", p.From)}, nil
	}
	from.PendingDebits -= p.Amount
	from.BalanceMinor -= p.Amount
	to.BalanceMinor += p.Amount
	delete(f.pendings, p.ID)

	journalID := fmt.Sprintf("jrn-%d", index)
	result := ledgertypes.TransferResult{Status: ledgertypes.TransferStatusApplied, JournalID: journalID, RaftIndex: index}
	lines := []ledgertypes.JournalLine{
		{AccountID: p.From, Direction: ledgertypes.Debit, AmountMinor: p.Amount},
		{AccountID: p.To, Direction: ledgertypes.Credit, AmountMinor: p.Amount},
	}
	f.dedup[req.DedupID] = dedupEntry{Result: result, Fingerprint: fp, Lines: lines}

	ev := &ledgertypes.LedgerEvent{
		Kind:        ledgertypes.EventPendingPosted,
		PendingID:   p.ID,
		DedupID:     req.DedupID,
		JournalID:   journalID,
		FromAccount: p.From,
		ToAccount:   p.To,
		AmountMinor: p.Amount,
		Currency:    p.Currency,
	}
	return &ApplyResult{Transfer: &result}, ev
}

func (f *FSM) applyVoidPendingLocked(index uint64, cmd Command) (*ApplyResult, *ledgertypes.LedgerEvent) {
	req := cmd.PendingAction
	fp := fingerprint(cmd)
	if res := f.checkDedupLocked(req.DedupID, fp); res != nil {
		return res, nil
	}
	p, ok := f.pendings[cmd.PendingID]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrPendingNotFound, ErrMsg: fmt.Sprintf("pending %q not found (posted, voided, or expired)", cmd.PendingID)}, nil
	}
	f.accounts[p.From].PendingDebits -= p.Amount
	delete(f.pendings, p.ID)

	result := ledgertypes.TransferResult{Status: ledgertypes.TransferStatusApplied, JournalID: fmt.Sprintf("void-%d", index), RaftIndex: index}
	f.dedup[req.DedupID] = dedupEntry{Result: result, Fingerprint: fp}

	ev := &ledgertypes.LedgerEvent{
		Kind:        ledgertypes.EventPendingVoided,
		PendingID:   p.ID,
		DedupID:     req.DedupID,
		FromAccount: p.From,
		ToAccount:   p.To,
		AmountMinor: p.Amount,
		Currency:    p.Currency,
		VoidReason:  "void",
	}
	return &ApplyResult{Transfer: &result}, ev
}

// expirePendingsLocked releases holds whose deadline has passed on the
// replicated clock. It runs at the start of every Apply, so expiry is a
// deterministic function of the log — no background goroutine, no wall
// clock. Released holds ride on the current entry's event (ExpiredPendings)
// to preserve one-event-per-log-entry.
func (f *FSM) expirePendingsLocked(now time.Time) []ledgertypes.ExpiredPending {
	if now.IsZero() || len(f.pendings) == 0 {
		return nil
	}
	ids := make([]string, 0, len(f.pendings))
	for id := range f.pendings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var expired []ledgertypes.ExpiredPending
	for _, id := range ids {
		p := f.pendings[id]
		if p.ExpiresAt.After(now) {
			continue
		}
		f.accounts[p.From].PendingDebits -= p.Amount
		delete(f.pendings, id)
		expired = append(expired, ledgertypes.ExpiredPending{
			PendingID:   p.ID,
			FromAccount: p.From,
			ToAccount:   p.To,
			AmountMinor: p.Amount,
			Currency:    p.Currency,
		})
	}
	return expired
}

// --- Snapshot / Restore: periodic log compaction ---

type snapshotState struct {
	Accounts    map[string]*account         `json:"accounts"`
	Pendings    map[string]*pending         `json:"pendings"`
	Settlements map[string]*settlementEntry `json:"settlements"`
	Dedup       map[string]dedupEntry       `json:"dedup"`
	Now         time.Time                   `json:"now"`
}

func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	state := snapshotState{
		Accounts:    make(map[string]*account, len(f.accounts)),
		Pendings:    make(map[string]*pending, len(f.pendings)),
		Settlements: make(map[string]*settlementEntry, len(f.settlements)),
		Dedup:       make(map[string]dedupEntry, len(f.dedup)),
		Now:         f.now,
	}
	for k, v := range f.accounts {
		cp := *v
		state.Accounts[k] = &cp
	}
	for k, v := range f.pendings {
		cp := *v
		state.Pendings[k] = &cp
	}
	for k, v := range f.settlements {
		cp := *v
		state.Settlements[k] = &cp
	}
	for k, v := range f.dedup {
		cp := v
		if v.Pending != nil {
			p := *v.Pending
			cp.Pending = &p
		}
		if v.Settlement != nil {
			s := *v.Settlement
			cp.Settlement = &s
		}
		if v.Lines != nil {
			cp.Lines = make([]ledgertypes.JournalLine, len(v.Lines))
			copy(cp.Lines, v.Lines)
		}
		state.Dedup[k] = cp
	}
	return &fsmSnapshot{state: state}, nil
}

func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var state snapshotState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts = state.Accounts
	if f.accounts == nil {
		f.accounts = make(map[string]*account)
	}
	f.pendings = state.Pendings
	if f.pendings == nil {
		f.pendings = make(map[string]*pending)
	}
	f.settlements = state.Settlements
	if f.settlements == nil {
		f.settlements = make(map[string]*settlementEntry)
	}
	f.dedup = state.Dedup
	if f.dedup == nil {
		f.dedup = make(map[string]dedupEntry)
	}
	f.now = state.Now
	return nil
}

type fsmSnapshot struct {
	state snapshotState
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		enc := json.NewEncoder(sink)
		return enc.Encode(s.state)
	}()
	if err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
