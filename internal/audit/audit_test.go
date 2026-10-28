package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLogger_WritesWellFormedJSONLine(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf)

	err := logger.Log(Entry{
		Timestamp:      time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		Tool:           "get_journal_entry",
		Args:           json.RawMessage(`{"transfer_dedup_id":"txn-2"}`),
		ResultRowCount: 2,
		RedactedFields: []string{"cost_center"},
	})
	if err != nil {
		t.Fatalf("Log returned error: %v", err)
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("expected audit log entry to end with a newline, got %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("expected exactly one line, got %q", out)
	}

	var decoded Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decoded); err != nil {
		t.Fatalf("audit log line is not valid JSON: %v (line: %s)", err, out)
	}

	if decoded.Tool != "get_journal_entry" {
		t.Errorf("tool = %q, want get_journal_entry", decoded.Tool)
	}
	if decoded.ResultRowCount != 2 {
		t.Errorf("result_row_count = %d, want 2", decoded.ResultRowCount)
	}
	if len(decoded.RedactedFields) != 1 || decoded.RedactedFields[0] != "cost_center" {
		t.Errorf("redacted_fields = %v, want [cost_center]", decoded.RedactedFields)
	}
	if decoded.Timestamp.IsZero() {
		t.Errorf("timestamp must not be zero")
	}

	// Required keys must be present verbatim in the raw JSON, per the
	// audit log format contract.
	for _, key := range []string{`"timestamp"`, `"tool"`, `"args"`, `"result_row_count"`, `"redacted_fields"`} {
		if !strings.Contains(out, key) {
			t.Errorf("audit log line missing required key %s: %s", key, out)
		}
	}
}

func TestLogger_EmptyRedactedFieldsSerializesAsEmptyArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf)

	if err := logger.Log(Entry{Tool: "get_account_balance", ResultRowCount: 1}); err != nil {
		t.Fatalf("Log returned error: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, `"redacted_fields":null`) {
		t.Errorf("redacted_fields must serialize as [] not null: %s", out)
	}
	if !strings.Contains(out, `"redacted_fields":[]`) {
		t.Errorf("expected redacted_fields:[] in output: %s", out)
	}
}

func TestLogger_RecordsErrorField(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf)

	if err := logger.Log(Entry{Tool: "get_account_balance", Error: "no account_balances row for account_id \"ghost\""}); err != nil {
		t.Fatalf("Log returned error: %v", err)
	}

	var decoded Entry
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode audit entry: %v", err)
	}
	if decoded.Error == "" {
		t.Errorf("expected error field to be populated for a failed call")
	}
}

func TestLogger_ConcurrentWritesStayOnSeparateLines(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf)

	done := make(chan struct{})
	for i := 0; i < 20; i++ {
		go func(n int) {
			_ = logger.Log(Entry{Tool: "search_transactions", ResultRowCount: n})
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 20 {
		t.Fatalf("expected 20 audit lines, got %d", len(lines))
	}
	for _, line := range lines {
		var decoded Entry
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Errorf("line is not valid standalone JSON (writes interleaved?): %v: %s", err, line)
		}
	}
}
