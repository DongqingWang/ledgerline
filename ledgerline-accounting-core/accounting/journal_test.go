package accounting

import "testing"

func testRuleSet() *RuleSet {
	return &RuleSet{Rules: []Rule{
		{
			Match:     Match{Category: "seller_payout", Country: "US"},
			Debit:     Dimensions{GLAccount: "2100", CostCenter: "CC-PAYOUT-US"},
			Credit:    Dimensions{GLAccount: "4000", CostCenter: "CC-REVENUE-US"},
			FeeCredit: Dimensions{GLAccount: "4900", CostCenter: "CC-FEES-US"},
		},
		{
			Match:  Match{Category: "refund"},
			Debit:  Dimensions{GLAccount: "4000", CostCenter: "CC-REVENUE"},
			Credit: Dimensions{GLAccount: "2100", CostCenter: "CC-PAYOUT"},
		},
		{
			Match:  Match{Category: "*", Country: "*"},
			Debit:  Dimensions{GLAccount: "9999", CostCenter: "CC-UNCLASSIFIED"},
			Credit: Dimensions{GLAccount: "9999", CostCenter: "CC-UNCLASSIFIED"},
		},
	}}
}

func TestComposeJournal_TableDriven(t *testing.T) {
	rs := testRuleSet()

	tests := []struct {
		name         string
		ctx          TransferContext
		wantErr      bool
		wantNumLines int
		wantDebitGL  string
		wantCreditGL string
	}{
		{
			name: "exact category+country match",
			ctx: TransferContext{
				FromAccount: "platform", ToAccount: "seller-1",
				AmountMinor: 10_000, Currency: "USD",
				Category: "seller_payout", Country: "US",
			},
			wantNumLines: 2, wantDebitGL: "2100", wantCreditGL: "4000",
		},
		{
			name: "falls through to catch-all for unknown category",
			ctx: TransferContext{
				FromAccount: "a", ToAccount: "b",
				AmountMinor: 500, Currency: "USD",
				Category: "something_new", Country: "FR",
			},
			wantNumLines: 2, wantDebitGL: "9999", wantCreditGL: "9999",
		},
		{
			name: "payout with a fee produces a third line and stays balanced",
			ctx: TransferContext{
				FromAccount: "platform", ToAccount: "seller-1",
				AmountMinor: 10_000, FeeMinor: 300, Currency: "USD",
				Category: "seller_payout", Country: "US",
			},
			wantNumLines: 3, wantDebitGL: "2100", wantCreditGL: "4000",
		},
		{
			name: "zero amount is rejected",
			ctx: TransferContext{
				FromAccount: "a", ToAccount: "b", AmountMinor: 0, Currency: "USD", Category: "refund",
			},
			wantErr: true,
		},
		{
			name: "fee greater than amount is rejected",
			ctx: TransferContext{
				FromAccount: "a", ToAccount: "b", AmountMinor: 100, FeeMinor: 200, Currency: "USD", Category: "refund",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j, err := rs.ComposeJournal(tc.ctx)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(j.Lines) != tc.wantNumLines {
				t.Fatalf("got %d lines, want %d: %+v", len(j.Lines), tc.wantNumLines, j.Lines)
			}
			if j.Lines[0].GLAccount != tc.wantDebitGL {
				t.Fatalf("debit GL = %s, want %s", j.Lines[0].GLAccount, tc.wantDebitGL)
			}
			if j.Lines[1].GLAccount != tc.wantCreditGL {
				t.Fatalf("credit GL = %s, want %s", j.Lines[1].GLAccount, tc.wantCreditGL)
			}
			if err := Validate(j); err != nil {
				t.Fatalf("composed journal must always validate: %v", err)
			}
		})
	}
}

func TestComposeJournal_FeeSplit_SumsCorrectly(t *testing.T) {
	rs := testRuleSet()
	j, err := rs.ComposeJournal(TransferContext{
		FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10_000, FeeMinor: 300, Currency: "USD",
		Category: "seller_payout", Country: "US",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var netToSeller, fee int64
	for _, l := range j.Lines {
		switch l.AccountID {
		case "seller-1":
			netToSeller = l.AmountMinor
		case "platform-fee-revenue":
			fee = l.AmountMinor
		}
	}
	if netToSeller != 9_700 {
		t.Fatalf("seller should net 9700, got %d", netToSeller)
	}
	if fee != 300 {
		t.Fatalf("fee line should be 300, got %d", fee)
	}
	if netToSeller+fee != 10_000 {
		t.Fatalf("net + fee must equal the original amount: %d + %d != 10000", netToSeller, fee)
	}
}

func TestComposeJournal_MissingFeeCreditDimensions(t *testing.T) {
	rs := &RuleSet{Rules: []Rule{
		{
			Match:  Match{Category: "refund"}, // no fee_credit configured
			Debit:  Dimensions{GLAccount: "4000", CostCenter: "CC-REVENUE"},
			Credit: Dimensions{GLAccount: "2100", CostCenter: "CC-PAYOUT"},
		},
	}}
	_, err := rs.ComposeJournal(TransferContext{
		FromAccount: "a", ToAccount: "b",
		AmountMinor: 1_000, FeeMinor: 50, Currency: "USD", Category: "refund",
	})
	if err == nil {
		t.Fatalf("expected an error when a fee is requested but the matched rule has no fee_credit dimensions")
	}
}

func TestValidate_RejectsUnbalancedJournal(t *testing.T) {
	j := Journal{Lines: []JournalLine{
		{AccountID: "a", Direction: Debit, AmountMinor: 100},
		{AccountID: "b", Direction: Credit, AmountMinor: 99},
	}}
	if err := Validate(j); err == nil {
		t.Fatalf("expected Validate to reject an unbalanced journal")
	}
}

func TestValidate_RejectsSingleLineJournal(t *testing.T) {
	j := Journal{Lines: []JournalLine{
		{AccountID: "a", Direction: Debit, AmountMinor: 100},
	}}
	if err := Validate(j); err == nil {
		t.Fatalf("expected Validate to reject a journal with fewer than 2 lines")
	}
}

func TestResolve_NoMatchingRuleAndNoCatchAll(t *testing.T) {
	rs := &RuleSet{Rules: []Rule{
		{Match: Match{Category: "refund"}},
	}}
	_, err := rs.ComposeJournal(TransferContext{
		FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD", Category: "something_else",
	})
	if err == nil {
		t.Fatalf("expected an error when no rule matches and there's no catch-all")
	}
}

func TestLoadRuleSet_FromYAML(t *testing.T) {
	rs, err := LoadRuleSet("rules.yaml")
	if err != nil {
		t.Fatalf("load rules.yaml: %v", err)
	}
	if len(rs.Rules) == 0 {
		t.Fatalf("expected at least one rule loaded from rules.yaml")
	}

	j, err := rs.ComposeJournal(TransferContext{
		FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 5_000, Currency: "USD", Category: "seller_payout", Country: "US",
	})
	if err != nil {
		t.Fatalf("compose from loaded rules: %v", err)
	}
	if err := Validate(j); err != nil {
		t.Fatalf("journal composed from rules.yaml must validate: %v", err)
	}
}
