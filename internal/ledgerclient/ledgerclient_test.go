package ledgerclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// addrOf strips the "http://" scheme off an httptest.Server URL, since
// Client builds requests as "http://" + addr + path.
func addrOf(s *httptest.Server) string {
	return strings.TrimPrefix(s.URL, "http://")
}

func notLeaderResponse(w http.ResponseWriter, hint string) {
	w.WriteHeader(http.StatusConflict)
	resp := map[string]interface{}{
		"error": map[string]string{
			"code":        "not_leader",
			"message":     "not the leader",
			"leader_hint": hint,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func transferAppliedResponse(w http.ResponseWriter, journalID string) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(TransferResult{
		Status:    "applied",
		JournalID: journalID,
		RaftIndex: 1,
	})
}

// TestTransfer_FollowsLeaderHint verifies that a 409 not_leader response
// with a non-empty leader_hint causes the client to retry against the
// hinted address and succeed.
func TestTransfer_FollowsLeaderHint(t *testing.T) {
	var followerCalls, leaderCalls int32

	var leaderAddr string
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followerCalls, 1)
		notLeaderResponse(w, leaderAddr)
	}))
	defer follower.Close()

	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&leaderCalls, 1)
		transferAppliedResponse(w, "jrn-1")
	}))
	defer leader.Close()
	leaderAddr = addrOf(leader)

	client := New([]string{addrOf(follower)})
	result, err := client.Transfer(TransferRequest{
		DedupID: "txn-1", FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "applied" || result.JournalID != "jrn-1" {
		t.Errorf("unexpected result: %+v", result)
	}
	if atomic.LoadInt32(&followerCalls) != 1 {
		t.Errorf("expected exactly 1 call to follower, got %d", followerCalls)
	}
	if atomic.LoadInt32(&leaderCalls) != 1 {
		t.Errorf("expected exactly 1 call to leader, got %d", leaderCalls)
	}
}

// TestTransfer_RemembersLastKnownLeader verifies that after a successful
// call, the client tries the last-known leader first on the next call,
// without re-contacting the node that originally redirected it there.
func TestTransfer_RemembersLastKnownLeader(t *testing.T) {
	var followerCalls, leaderCalls int32

	var leaderAddr string
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&followerCalls, 1)
		notLeaderResponse(w, leaderAddr)
	}))
	defer follower.Close()

	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&leaderCalls, 1)
		transferAppliedResponse(w, fmt.Sprintf("jrn-%d", n))
	}))
	defer leader.Close()
	leaderAddr = addrOf(leader)

	client := New([]string{addrOf(follower)})

	// First call: follower redirects to leader.
	if _, err := client.Transfer(TransferRequest{DedupID: "txn-1", FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD"}); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if atomic.LoadInt32(&followerCalls) != 1 || atomic.LoadInt32(&leaderCalls) != 1 {
		t.Fatalf("after first call: followerCalls=%d leaderCalls=%d, want 1/1", followerCalls, leaderCalls)
	}

	// Second call: should go straight to the remembered leader, never
	// touching the follower again.
	if _, err := client.Transfer(TransferRequest{DedupID: "txn-2", FromAccount: "a", ToAccount: "b", AmountMinor: 200, Currency: "USD"}); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	if atomic.LoadInt32(&followerCalls) != 1 {
		t.Errorf("expected follower to still have 1 call after second request (leader should be tried first), got %d", followerCalls)
	}
	if atomic.LoadInt32(&leaderCalls) != 2 {
		t.Errorf("expected leader to have 2 calls after second request, got %d", leaderCalls)
	}
}

// TestTransfer_GivesUpAfterMaxAttempts verifies that a chain of not_leader
// responses that never resolves to a working node gives up after
// MaxAttempts (5) tries and returns an error, rather than looping forever.
func TestTransfer_GivesUpAfterMaxAttempts(t *testing.T) {
	const n = 5
	servers := make([]*httptest.Server, n)
	addrs := make([]string, n)
	var callCount int32

	// Each server S_i hints at S_(i+1 mod n), forming a cycle that never
	// reaches a leader. Server 0 is hit first and last (on the cycle
	// wraparound), so the client should recognize it has already tried
	// that hint and give up rather than looping indefinitely.
	for i := 0; i < n; i++ {
		idx := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&callCount, 1)
			notLeaderResponse(w, addrs[(idx+1)%n])
		}))
		defer servers[i].Close()
	}
	for i := 0; i < n; i++ {
		addrs[i] = addrOf(servers[i])
	}

	client := New([]string{addrs[0]})
	_, err := client.Transfer(TransferRequest{DedupID: "txn-1", FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD"})
	if err == nil {
		t.Fatal("expected an error after exhausting max attempts, got nil")
	}
	if !strings.Contains(err.Error(), "giving up after 5 attempts") {
		t.Errorf("error message = %q, want it to mention giving up after 5 attempts", err.Error())
	}
	if got := atomic.LoadInt32(&callCount); got != MaxAttempts {
		t.Errorf("expected exactly %d total attempts, got %d", MaxAttempts, got)
	}
}

// TestTransfer_TerminalApplicationError verifies that a non-"not_leader"
// error (e.g. insufficient_funds) is returned immediately without retrying
// other nodes.
func TestTransfer_TerminalApplicationError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "insufficient_funds",
				"message": "not enough balance",
			},
		})
	}))
	defer server.Close()

	client := New([]string{addrOf(server)})
	_, err := client.Transfer(TransferRequest{DedupID: "txn-1", FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD"})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var le *Error
	if le, _ = err.(*Error); le == nil {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if le.Code != "insufficient_funds" {
		t.Errorf("code = %q, want insufficient_funds", le.Code)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected exactly 1 call (no retry on terminal error), got %d", calls)
	}
}
