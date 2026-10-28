// Package audit implements structured audit logging for every MCP tool
// invocation handled by the gateway, successful or not. Because an AI
// agent's queries are driven by natural language and are not statically
// known in advance, the only way to make its actual behavior
// reconstructable after the fact is to log every real invocation as it
// happens.
package audit

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Entry is one audit log record, emitted as a single JSON line.
type Entry struct {
	// Timestamp is when the tool call was handled, in UTC RFC3339Nano.
	Timestamp time.Time `json:"timestamp"`

	// Tool is the MCP tool name, e.g. "get_account_balance".
	Tool string `json:"tool"`

	// Args is the raw input arguments the caller supplied, unredacted,
	// so the exact request can be reconstructed later.
	Args json.RawMessage `json:"args"`

	// ResultRowCount is the number of ledger rows the query matched/
	// returned (0 on error or an empty result set).
	ResultRowCount int `json:"result_row_count"`

	// RedactedFields lists the distinct field names that were
	// redacted/masked anywhere in this response. Always a non-nil,
	// possibly-empty slice so it serializes as `[]` rather than `null`.
	RedactedFields []string `json:"redacted_fields"`

	// Error holds the error message if the call failed; omitted on
	// success.
	Error string `json:"error,omitempty"`
}

// Logger writes audit Entry records as newline-delimited JSON to an
// underlying writer (e.g. stdout, or a log file). It is safe for
// concurrent use.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time
}

// NewLogger creates a Logger that writes to out.
func NewLogger(out io.Writer) *Logger {
	return &Logger{out: out, now: time.Now}
}

// Log writes a single audit entry as one JSON line. It stamps Timestamp
// with the current time (UTC) if it is zero, and ensures RedactedFields is
// never nil. Returns any write/marshal error, but callers should treat
// audit logging as best-effort and not fail the underlying tool call
// because of it.
func (l *Logger) Log(e Entry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = l.now().UTC()
	} else {
		e.Timestamp = e.Timestamp.UTC()
	}
	if e.RedactedFields == nil {
		e.RedactedFields = []string{}
	}
	if e.Args == nil {
		e.Args = json.RawMessage("{}")
	}

	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	_, err = l.out.Write(line)
	return err
}
