// Package redact implements the field-level data classification and
// redaction/masking rules applied to ledger data before it is returned to
// an AI agent through the MCP gateway.
//
// The policy itself lives in a YAML file (see redaction.yaml at the repo
// root) rather than being hardcoded, so that changing which fields are
// exposed, redacted, or masked is a config change, not a code change.
package redact

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Classification describes how sensitive a given field is considered to be
// in this demo's data-classification scheme.
type Classification string

const (
	// ClassificationPublic fields are safe to return to an AI agent as-is.
	ClassificationPublic Classification = "public"

	// ClassificationInternalSensitive fields are internal operational
	// metadata (e.g. cost-allocation codes, dedup identifiers) that should
	// not be exposed to an AI agent by default.
	ClassificationInternalSensitive Classification = "internal_sensitive"
)

// Action describes the concrete mechanism applied to a field that carries a
// given classification.
type Action string

const (
	// ActionNone passes the field through unchanged.
	ActionNone Action = "none"

	// ActionRedact removes the field's value entirely from the response
	// (it is omitted from the returned JSON object).
	ActionRedact Action = "redact"

	// ActionMask partially obscures the field's value, keeping a short
	// prefix and suffix visible (e.g. "txn-...3f9a") so the shape of the
	// value is still recognizable without exposing it in full.
	ActionMask Action = "mask"
)

// FieldPolicy is the policy for a single field name.
type FieldPolicy struct {
	Classification Classification `yaml:"classification"`
	Action         Action         `yaml:"action"`
}

// Policy is the full field-level redaction policy, keyed by field name.
type Policy struct {
	Fields map[string]FieldPolicy `yaml:"fields"`
}

// RawPolicy mirrors the top-level shape of redaction.yaml.
type RawPolicy struct {
	Fields map[string]FieldPolicy `yaml:"fields"`
}

// ActionFor returns the effective action for a field. If the field is not
// present in the policy at all, it defaults to ActionNone (pass-through).
// If the field is present with a classification but no explicit action,
// internal_sensitive defaults to ActionRedact and public defaults to
// ActionNone.
func (p Policy) ActionFor(field string) Action {
	fp, ok := p.Fields[field]
	if !ok {
		return ActionNone
	}
	if fp.Action != "" {
		return fp.Action
	}
	if fp.Classification == ClassificationInternalSensitive {
		return ActionRedact
	}
	return ActionNone
}

// Classify returns the classification recorded for a field, or
// ClassificationPublic if the field is not present in the policy.
func (p Policy) Classify(field string) Classification {
	fp, ok := p.Fields[field]
	if !ok {
		return ClassificationPublic
	}
	return fp.Classification
}

// LoadPolicy reads and parses a redaction policy from a YAML file on disk.
func LoadPolicy(path string) (Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, fmt.Errorf("redact: reading policy file %q: %w", path, err)
	}
	return ParsePolicy(data)
}

// ParsePolicy parses a redaction policy from raw YAML bytes.
func ParsePolicy(data []byte) (Policy, error) {
	var raw RawPolicy
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Policy{}, fmt.Errorf("redact: parsing policy YAML: %w", err)
	}
	return Policy{Fields: raw.Fields}, nil
}

// DefaultPolicy returns the built-in fallback policy, matching the
// repository's checked-in redaction.yaml. It is used if the config file
// cannot be found, and as the baseline for tests.
func DefaultPolicy() Policy {
	return Policy{
		Fields: map[string]FieldPolicy{
			"cost_center": {
				Classification: ClassificationInternalSensitive,
				Action:         ActionRedact,
			},
			"transfer_dedup_id": {
				Classification: ClassificationInternalSensitive,
				Action:         ActionMask,
			},
			"account_id":    {Classification: ClassificationPublic, Action: ActionNone},
			"direction":     {Classification: ClassificationPublic, Action: ActionNone},
			"amount_minor":  {Classification: ClassificationPublic, Action: ActionNone},
			"currency":      {Classification: ClassificationPublic, Action: ActionNone},
			"gl_account":    {Classification: ClassificationPublic, Action: ActionNone},
			"posted_at":     {Classification: ClassificationPublic, Action: ActionNone},
			"balance_minor": {Classification: ClassificationPublic, Action: ActionNone},
		},
	}
}
