package accounting

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Dimensions is the SAP-style financial classification a rule assigns to
// one leg of a journal: which GL account, which cost center, and (for
// tax-relevant legs) which tax code.
type Dimensions struct {
	GLAccount  string `yaml:"gl_account"`
	CostCenter string `yaml:"cost_center"`
	TaxCode    string `yaml:"tax_code,omitempty"`
}

// Match is the set of TransferContext fields a rule applies to. An empty or
// "*" field matches any value — so a catch-all rule is just one with every
// field set to "*" (or omitted).
type Match struct {
	Category string `yaml:"category,omitempty"`
	Country  string `yaml:"country,omitempty"`
}

func (m Match) matches(ctx TransferContext) bool {
	if m.Category != "" && m.Category != "*" && m.Category != ctx.Category {
		return false
	}
	if m.Country != "" && m.Country != "*" && m.Country != ctx.Country {
		return false
	}
	return true
}

// Rule maps a TransferContext to the GL dimensions for the debit leg, the
// credit leg, and (when a fee is present) the fee leg.
type Rule struct {
	Match     Match      `yaml:"match"`
	Debit     Dimensions `yaml:"debit"`
	Credit    Dimensions `yaml:"credit"`
	FeeCredit Dimensions `yaml:"fee_credit,omitempty"`
}

// RuleSet is an ordered list of Rules — first match wins, the usual
// condition-rule-engine semantics. Always define a final catch-all rule
// (category: "*", country: "*") so every transfer resolves to *something*,
// even if that something is an "unclassified" GL bucket that flags for
// manual review.
type RuleSet struct {
	Rules []Rule `yaml:"rules"`
}

// LoadRuleSet reads a YAML rule file (see rules.yaml for the shape).
func LoadRuleSet(path string) (*RuleSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rule file: %w", err)
	}
	var rs RuleSet
	if err := yaml.Unmarshal(data, &rs); err != nil {
		return nil, fmt.Errorf("parse rule file: %w", err)
	}
	return &rs, nil
}

// Resolve returns the first rule matching ctx, in file order.
func (rs *RuleSet) Resolve(ctx TransferContext) (Rule, error) {
	for _, r := range rs.Rules {
		if r.Match.matches(ctx) {
			return r, nil
		}
	}
	return Rule{}, fmt.Errorf("no matching rule (and no catch-all rule defined) for category=%q country=%q", ctx.Category, ctx.Country)
}
