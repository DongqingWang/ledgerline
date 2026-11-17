package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

// AuthConfig is a demo-grade static API-key table loaded from JSON:
//
//	{"keys": [
//	  {"key": "sk-tenant-a", "tenant": "tenant-a"},
//	  {"key": "sk-internal", "tenant": "", "admin": true}
//	]}
//
// Admin keys are for internal services (gateway, balance-reader,
// persistence-sync): they bypass tenant-ownership checks, may set
// X-Tenant-ID to act on behalf of a tenant, and are the only keys allowed
// to tail the event stream when auth is enabled. A production system would
// replace this with a real identity provider / key management service; the
// enforcement points (middleware + ownership pre-checks) stay the same.
type AuthConfig struct {
	Keys []AuthKey `json:"keys"`

	byKey map[string]AuthKey
}

type AuthKey struct {
	Key    string `json:"key"`
	Tenant string `json:"tenant"`
	Admin  bool   `json:"admin"`
}

// LoadAuthConfig reads and validates the key table.
func LoadAuthConfig(path string) (*AuthConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read auth config: %w", err)
	}
	var cfg AuthConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse auth config: %w", err)
	}
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("auth config: no keys defined")
	}
	cfg.byKey = make(map[string]AuthKey, len(cfg.Keys))
	for _, k := range cfg.Keys {
		if k.Key == "" {
			return nil, fmt.Errorf("auth config: empty key")
		}
		if !k.Admin && k.Tenant == "" {
			return nil, fmt.Errorf("auth config: non-admin key %q must declare a tenant", k.Key)
		}
		if _, dup := cfg.byKey[k.Key]; dup {
			return nil, fmt.Errorf("auth config: duplicate key %q", k.Key)
		}
		cfg.byKey[k.Key] = k
	}
	return &cfg, nil
}

type ctxKey int

const (
	ctxTenant ctxKey = iota
	ctxAdmin
)

func tenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxTenant).(string)
	return v
}

func isAdminContext(ctx context.Context) bool {
	v, _ := ctx.Value(ctxAdmin).(bool)
	return v
}

// withAuth wraps the mux with API-key authentication when Auth is
// configured. /healthz and /v1/leader stay open (probes and leader
// discovery must work before a client knows which node to authenticate
// against). The event stream is admin-only: it carries every tenant's
// events, so tenant keys must never tail it.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.Auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/v1/leader":
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("X-API-Key")
		if key == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				key = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		k, ok := s.cfg.Auth.byKey[key]
		if !ok {
			writeError(w, http.StatusUnauthorized, ledgertypes.ErrUnauthorized, "missing or unknown API key", "")
			return
		}
		if r.URL.Path == "/v1/events/stream" && !k.Admin {
			writeError(w, http.StatusForbidden, ledgertypes.ErrForbidden, "event stream requires an admin key", "")
			return
		}

		tenant := k.Tenant
		if k.Admin {
			// Admin callers may act on behalf of a tenant via X-Tenant-ID;
			// without it they operate at platform scope (tenant "").
			tenant = r.Header.Get("X-Tenant-ID")
		}
		ctx := context.WithValue(r.Context(), ctxTenant, tenant)
		ctx = context.WithValue(ctx, ctxAdmin, k.Admin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
