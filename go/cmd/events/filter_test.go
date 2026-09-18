package main

import "testing"

func TestFilterMatchesEverythingByDefault(t *testing.T) {
	f := newFilter("", "")

	if !f.matches(row{AggregateType: "wallet", AggregateID: "abc"}) {
		t.Fatal("empty filter should match every event")
	}
}

func TestFilterKeepsOnlyTheNamedAggregateTypes(t *testing.T) {
	f := newFilter("transaction, transfer", "")

	for typ, want := range map[string]bool{"transaction": true, "transfer": true, "wallet": false} {
		if got := f.matches(row{AggregateType: typ}); got != want {
			t.Errorf("matches(%s) = %v, want %v", typ, got, want)
		}
	}
}

func TestFilterKeepsOnlyAggregatesWhoseIdContainsTheFragment(t *testing.T) {
	f := newFilter("", "AB52E2C")

	if !f.matches(row{AggregateID: "01a0b5cf-a64c-76ea-99a0-d6270ab52e2c"}) {
		t.Error("should match an id containing the fragment, ignoring case")
	}
	if f.matches(row{AggregateID: "01a0b5c1-0000-7000-8000-000000000000"}) {
		t.Error("should not match an id without the fragment")
	}
}
