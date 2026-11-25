package clearing

import (
	"reflect"
	"testing"
)

func TestNet_GroupsAndSumsPerMerchantCurrency(t *testing.T) {
	captures := []Capture{
		{MerchantID: "m1", Currency: "USD", NetMinor: 9700, Ref: "t1"},
		{MerchantID: "m1", Currency: "USD", NetMinor: 19400, Ref: "t2"},
		{MerchantID: "m2", Currency: "USD", NetMinor: 4850, Ref: "t3"},
	}
	got := Net(captures)
	want := []NetPosition{
		{MerchantID: "m1", Currency: "USD", NetMinor: 29100, Count: 2},
		{MerchantID: "m2", Currency: "USD", NetMinor: 4850, Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Net = %+v, want %+v", got, want)
	}
}

func TestNet_SeparatesByCurrency(t *testing.T) {
	captures := []Capture{
		{MerchantID: "m1", Currency: "USD", NetMinor: 100, Ref: "a"},
		{MerchantID: "m1", Currency: "EUR", NetMinor: 200, Ref: "b"},
	}
	got := Net(captures)
	if len(got) != 2 || got[0].Currency != "EUR" || got[1].Currency != "USD" {
		t.Errorf("expected per-currency split sorted, got %+v", got)
	}
	if got[0].NetMinor != 200 || got[1].NetMinor != 100 {
		t.Errorf("currency nets wrong: %+v", got)
	}
}

func TestNet_RefundsNetAgainstCaptures(t *testing.T) {
	captures := []Capture{
		{MerchantID: "m1", Currency: "USD", NetMinor: 9700, Ref: "cap"},
		{MerchantID: "m1", Currency: "USD", NetMinor: -5000, Ref: "refund"},
	}
	got := Net(captures)
	if len(got) != 1 || got[0].NetMinor != 4700 || got[0].Count != 2 {
		t.Errorf("net after refund = %+v, want 4700 count 2", got)
	}
}

func TestNet_FullRefundZeroPositionKept(t *testing.T) {
	captures := []Capture{
		{MerchantID: "m1", Currency: "USD", NetMinor: 1000, Ref: "cap"},
		{MerchantID: "m1", Currency: "USD", NetMinor: -1000, Ref: "refund"},
	}
	got := Net(captures)
	if len(got) != 1 || got[0].NetMinor != 0 {
		t.Errorf("fully-refunded merchant should net to 0 and be kept: %+v", got)
	}
}

func TestNet_DeterministicOrder(t *testing.T) {
	captures := []Capture{
		{MerchantID: "zeta", Currency: "USD", NetMinor: 1},
		{MerchantID: "alpha", Currency: "USD", NetMinor: 2},
		{MerchantID: "alpha", Currency: "EUR", NetMinor: 3},
	}
	got := Net(captures)
	order := []string{got[0].MerchantID + got[0].Currency, got[1].MerchantID + got[1].Currency, got[2].MerchantID + got[2].Currency}
	want := []string{"alphaEUR", "alphaUSD", "zetaUSD"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestNet_Empty(t *testing.T) {
	if got := Net(nil); len(got) != 0 {
		t.Errorf("expected empty, got %+v", got)
	}
}
