package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// handleEventStream is the CQRS tap: it replays every committed event with
// raft_index >= from_index, then keeps the connection open and streams new
// events as newline-delimited JSON. ledgerline-balance-reader and
// ledgerline-persistence-sync both tail this instead of querying the engine
// synchronously for every balance change.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	fromIndex := uint64(0)
	if v := r.URL.Query().Get("from_index"); v != "" {
		parsed, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "from_index must be a non-negative integer", "")
			return
		}
		fromIndex = parsed
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported", "")
		return
	}

	live, backlog, cancel := s.cfg.Node.FSM.Events().Subscribe(fromIndex)
	defer cancel()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	for _, ev := range backlog {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-live:
			if !open {
				return
			}
			if err := enc.Encode(ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
