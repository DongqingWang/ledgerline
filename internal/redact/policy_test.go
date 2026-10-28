package redact

import "testing"

func TestParsePolicy(t *testing.T) {
	yamlDoc := []byte(`
fields:
  cost_center:
    classification: internal_sensitive
    action: redact
  account_id:
    classification: public
`)
	policy, err := ParsePolicy(yamlDoc)
	if err != nil {
		t.Fatalf("ParsePolicy failed: %v", err)
	}

	if got := policy.ActionFor("cost_center"); got != ActionRedact {
		t.Errorf("ActionFor(cost_center) = %q, want %q", got, ActionRedact)
	}
	if got := policy.Classify("cost_center"); got != ClassificationInternalSensitive {
		t.Errorf("Classify(cost_center) = %q, want %q", got, ClassificationInternalSensitive)
	}
	// account_id has no explicit action -> public defaults to none.
	if got := policy.ActionFor("account_id"); got != ActionNone {
		t.Errorf("ActionFor(account_id) = %q, want %q", got, ActionNone)
	}
	// A field absent from the policy entirely must default to pass-through,
	// never to silently redacting something nobody classified.
	if got := policy.ActionFor("totally_unknown_field"); got != ActionNone {
		t.Errorf("ActionFor(unknown) = %q, want %q", got, ActionNone)
	}
}

func TestParsePolicy_InternalSensitiveDefaultsToRedactWithoutExplicitAction(t *testing.T) {
	yamlDoc := []byte(`
fields:
  cost_center:
    classification: internal_sensitive
`)
	policy, err := ParsePolicy(yamlDoc)
	if err != nil {
		t.Fatalf("ParsePolicy failed: %v", err)
	}
	if got := policy.ActionFor("cost_center"); got != ActionRedact {
		t.Errorf("ActionFor(cost_center) = %q, want %q (internal_sensitive must default to redact)", got, ActionRedact)
	}
}

func TestDefaultPolicy_MatchesRepoConfigShape(t *testing.T) {
	policy := DefaultPolicy()

	if policy.ActionFor("cost_center") != ActionRedact {
		t.Errorf("DefaultPolicy: cost_center must redact")
	}
	if policy.ActionFor("transfer_dedup_id") != ActionMask {
		t.Errorf("DefaultPolicy: transfer_dedup_id must mask")
	}
	for _, public := range []string{"account_id", "direction", "amount_minor", "currency", "gl_account", "posted_at", "balance_minor"} {
		if policy.ActionFor(public) != ActionNone {
			t.Errorf("DefaultPolicy: %s must pass through (none), got %s", public, policy.ActionFor(public))
		}
	}
}

func TestLoadPolicy_MissingFileReturnsError(t *testing.T) {
	if _, err := LoadPolicy("/nonexistent/path/redaction.yaml"); err == nil {
		t.Errorf("expected an error loading a nonexistent policy file, got nil")
	}
}
