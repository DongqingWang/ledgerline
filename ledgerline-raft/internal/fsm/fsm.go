// Package fsm implements the ledgerline-raft replicated state machine: the
// event-sourced heart of the engine. Every balance change is first appended
// to the raft log (by the leader, replicated to a majority) and only then
// applied here — on every node, in the same deterministic order — which is
// what makes the committed log itself the durable, tamper-evident audit
// trail rather than a side effect of it.
package fsm

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

// CommandType distinguishes the two mutating operations the FSM understands.
type CommandType string

const (
	CmdCreateAccount  CommandType = "create_account"
	CmdAssertTransfer CommandType = "assert_transfer"
)

// Command is the single envelope type serialized into every raft log entry.
// Keeping one envelope (rather than raw per-command bytes) makes it trivial
// to add new command types later without touching the raft plumbing.
type Command struct {
	Type           CommandType                       `json:"type"`
	CreateAccount  *ledgertypes.CreateAccountRequest `json:"create_account,omitempty"`
	AssertTransfer *ledgertypes.TransferRequest      `json:"assert_transfer,omitempty"`
}

// Encode serializes a Command for raft.Raft.Apply.
func (c Command) Encode() ([]byte, error) { return json.Marshal(c) }

// ApplyResult is what FSM.Apply returns (retrieved via raft's ApplyFuture.Response()).
// Business-rule failures (account not found, insufficient funds, ...) are
// reported here rather than as a Go error, because raft.FSM.Apply must never
// return differently across replicas based on something non-deterministic —
// an error value here is just data, computed identically on every node.
type ApplyResult struct {
	Account  *ledgertypes.Account        `json:"account,omitempty"`
	Transfer *ledgertypes.TransferResult `json:"transfer,omitempty"`
	ErrCode  ledgertypes.ErrorCode       `json:"err_code,omitempty"`
	ErrMsg   string                      `json:"err_msg,omitempty"`
}

func (r *ApplyResult) IsError() bool { return r.ErrCode != "" }

type account struct {
	Currency     string
	BalanceMinor int64
	Unlimited    bool
}

// FSM is the raft.FSM implementation. All mutation happens inside Apply,
// which raft guarantees is called sequentially and deterministically on
// every replica for every committed log entry.
type FSM struct {
	mu       sync.RWMutex
	accounts map[string]*account
	// dedup maps a transfer's DedupID to the result it originally produced,
	// giving AssertTransfer exactly-once semantics under client retries.
	dedup map[string]ledgertypes.TransferResult

	events *EventBus
}

func New() *FSM {
	return &FSM{
		accounts: make(map[string]*account),
		dedup:    make(map[string]ledgertypes.TransferResult),
		events:   NewEventBus(),
	}
}

// Events exposes the event bus so the HTTP layer can let clients subscribe
// to the committed-event stream (the CQRS tap used by balance-reader and
// persistence-sync).
func (f *FSM) Events() *EventBus { return f.events }

// Account returns a point-in-time read of local state. On a follower this
// can be slightly behind the leader (it reflects whatever this replica has
// applied so far) — callers that need a linearizable read should go through
// the leader's AssertTransfer/GetLeader path instead.
func (f *FSM) Account(id string) (*ledgertypes.Account, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	a, ok := f.accounts[id]
	if !ok {
		return nil, false
	}
	return &ledgertypes.Account{AccountID: id, Currency: a.Currency, BalanceMinor: a.BalanceMinor, Unlimited: a.Unlimited}, true
}

// Apply implements raft.FSM.
func (f *FSM) Apply(log *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(log.Data, &cmd); err != nil {
		// A malformed log entry would be a programming bug in the writer, not
		// a recoverable business error — but we still must not panic, since
		// that would crash this replica while others continue, causing
		// divergence. Surface it as a result instead.
		return &ApplyResult{ErrCode: ledgertypes.ErrInternal, ErrMsg: fmt.Sprintf("decode command: %v", err)}
	}

	switch cmd.Type {
	case CmdCreateAccount:
		return f.applyCreateAccount(log.Index, cmd.CreateAccount)
	case CmdAssertTransfer:
		return f.applyAssertTransfer(log.Index, cmd.AssertTransfer)
	default:
		return &ApplyResult{ErrCode: ledgertypes.ErrInvalidRequest, ErrMsg: fmt.Sprintf("unknown command type %q", cmd.Type)}
	}
}

func (f *FSM) applyCreateAccount(index uint64, req *ledgertypes.CreateAccountRequest) *ApplyResult {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.accounts[req.AccountID]; exists {
		return &ApplyResult{ErrCode: ledgertypes.ErrAccountExists, ErrMsg: fmt.Sprintf("account %q already exists", req.AccountID)}
	}
	f.accounts[req.AccountID] = &account{Currency: req.Currency, BalanceMinor: 0, Unlimited: req.Unlimited}

	f.events.Publish(ledgertypes.LedgerEvent{
		RaftIndex: index,
		Kind:      ledgertypes.EventAccountCreated,
		Timestamp: time.Now().UTC(),
		AccountID: req.AccountID,
		Currency:  req.Currency,
	})

	return &ApplyResult{Account: &ledgertypes.Account{AccountID: req.AccountID, Currency: req.Currency, BalanceMinor: 0, Unlimited: req.Unlimited}}
}

func (f *FSM) applyAssertTransfer(index uint64, req *ledgertypes.TransferRequest) *ApplyResult {
	f.mu.Lock()
	defer f.mu.Unlock()

	if prior, seen := f.dedup[req.DedupID]; seen {
		dup := prior
		dup.Status = ledgertypes.TransferStatusDuplicate
		return &ApplyResult{Transfer: &dup}
	}

	from, ok := f.accounts[req.FromAccount]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("from_account %q not found", req.FromAccount)}
	}
	to, ok := f.accounts[req.ToAccount]
	if !ok {
		return &ApplyResult{ErrCode: ledgertypes.ErrAccountNotFound, ErrMsg: fmt.Sprintf("to_account %q not found", req.ToAccount)}
	}
	if req.AmountMinor <= 0 {
		return &ApplyResult{ErrCode: ledgertypes.ErrInvalidRequest, ErrMsg: "amount_minor must be positive"}
	}
	if from.Currency != req.Currency || to.Currency != req.Currency {
		return &ApplyResult{ErrCode: ledgertypes.ErrInvalidRequest, ErrMsg: "currency mismatch between request and accounts"}
	}
	if !from.Unlimited && from.BalanceMinor < req.AmountMinor {
		return &ApplyResult{ErrCode: ledgertypes.ErrInsufficientFunds, ErrMsg: fmt.Sprintf("account %q has insufficient balance", req.FromAccount)}
	}

	// The double-entry invariant: the amount debited from one side always
	// equals the amount credited to the other, by construction — there is
	// only ever one amount moved, never two independently-specified legs.
	from.BalanceMinor -= req.AmountMinor
	to.BalanceMinor += req.AmountMinor

	journalID := fmt.Sprintf("jrn-%d", index)
	result := ledgertypes.TransferResult{
		Status:    ledgertypes.TransferStatusApplied,
		JournalID: journalID,
		RaftIndex: index,
	}
	f.dedup[req.DedupID] = result

	f.events.Publish(ledgertypes.LedgerEvent{
		RaftIndex:   index,
		Kind:        ledgertypes.EventTransferApplied,
		Timestamp:   time.Now().UTC(),
		DedupID:     req.DedupID,
		JournalID:   journalID,
		FromAccount: req.FromAccount,
		ToAccount:   req.ToAccount,
		AmountMinor: req.AmountMinor,
		GLAccount:   req.GLAccount,
		CostCenter:  req.CostCenter,
	})

	return &ApplyResult{Transfer: &result}
}

// --- Snapshot / Restore: periodic log compaction ---

type snapshotState struct {
	Accounts map[string]*account                   `json:"accounts"`
	Dedup    map[string]ledgertypes.TransferResult `json:"dedup"`
}

func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	state := snapshotState{
		Accounts: make(map[string]*account, len(f.accounts)),
		Dedup:    make(map[string]ledgertypes.TransferResult, len(f.dedup)),
	}
	for k, v := range f.accounts {
		cp := *v
		state.Accounts[k] = &cp
	}
	for k, v := range f.dedup {
		state.Dedup[k] = v
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
	f.dedup = state.Dedup
	if f.dedup == nil {
		f.dedup = make(map[string]ledgertypes.TransferResult)
	}
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
