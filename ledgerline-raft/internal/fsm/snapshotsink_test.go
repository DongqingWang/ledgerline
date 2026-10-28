package fsm

import (
	"bytes"
	"io"
)

// memSnapshotSink is a minimal in-memory raft.SnapshotSink for exercising
// FSM.Snapshot/Restore round-trips without touching disk.
type memSnapshotSink struct {
	buf bytes.Buffer
}

func newMemSnapshotSink() *memSnapshotSink { return &memSnapshotSink{} }

func (s *memSnapshotSink) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *memSnapshotSink) Close() error                { return nil }
func (s *memSnapshotSink) ID() string                  { return "test-snapshot" }
func (s *memSnapshotSink) Cancel() error               { return nil }

func (s *memSnapshotSink) reader() io.ReadCloser {
	return io.NopCloser(bytes.NewReader(s.buf.Bytes()))
}
