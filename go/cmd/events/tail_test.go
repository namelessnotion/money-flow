package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	txpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
)

type fakeSource struct {
	batches [][]row
	queries []query
}

type query struct {
	after int64
	holes []int64
}

func (s *fakeSource) head(context.Context) (int64, error) { return 100, nil }

func (s *fakeSource) first(_ context.Context, _, aggregateID string) (int64, error) {
	if aggregateID != "tx" {
		return 0, errors.New("no events")
	}
	return 40, nil
}

func (s *fakeSource) read(_ context.Context, after int64, holes []int64, _ int) ([]row, error) {
	s.queries = append(s.queries, query{after, slices.Clone(holes)})
	if len(s.batches) == 0 {
		return nil, nil
	}
	batch := s.batches[0]
	s.batches = s.batches[1:]
	return batch, nil
}

func TestPollPrintsMatchingEventsAndMovesOn(t *testing.T) {
	transfer := eventRow(t, 102, &txpb.TransactionStarted{})
	transfer.AggregateType = "transfer"
	src := &fakeSource{batches: [][]row{
		{eventRow(t, 101, &txpb.TransactionInitialized{}), transfer, eventRow(t, 104, &txpb.TransactionStarted{})},
		{eventRow(t, 103, &txpb.TransactionCompleted{})},
	}}
	var out bytes.Buffer
	tl := tailer{
		source: src, out: &out, filter: newFilter("transaction", ""),
		render: renderer{location: time.UTC}, cursor: newCursor(100, 5*time.Second), now: func() time.Time { return t0 },
	}

	for range 2 {
		if err := tl.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var names []string
	for _, l := range lines {
		names = append(names, strings.Fields(l)[5])
	}
	if !slices.Equal(names, []string{"TransactionInitialized", "TransactionStarted", "TransactionCompleted"}) {
		t.Fatalf("printed %v", names)
	}
	want := []query{{100, nil}, {104, []int64{103}}}
	if len(src.queries) != 2 || src.queries[0].after != want[0].after || len(src.queries[0].holes) != 0 ||
		src.queries[1].after != 104 || !slices.Equal(src.queries[1].holes, want[1].holes) {
		t.Fatalf("queries = %+v, want %+v", src.queries, want)
	}
}

func TestStartingPointDefaultsToTheEndOfTheLog(t *testing.T) {
	src := &fakeSource{}
	for _, tc := range []struct {
		from, last, want int64
	}{
		{from: -1, last: 0, want: 100},
		{from: -1, last: 30, want: 70},
		{from: -1, last: 500, want: 0},
		{from: 42, last: 30, want: 42},
	} {
		got, err := startingPoint(context.Background(), src, tc.from, tc.last, "")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("startingPoint(from=%d, last=%d) = %d, want %d", tc.from, tc.last, got, tc.want)
		}
	}
}

func TestStartingPointForAFollowedTransactionIsJustBeforeItsFirstEvent(t *testing.T) {
	src := &fakeSource{}

	got, err := startingPoint(context.Background(), src, -1, 0, "tx")
	if err != nil || got != 39 {
		t.Fatalf("startingPoint = (%d, %v), want (39, nil)", got, err)
	}
	if got, _ := startingPoint(context.Background(), src, 5, 0, "tx"); got != 5 {
		t.Fatalf("an explicit -from should win, got %d", got)
	}
	if _, err := startingPoint(context.Background(), src, -1, 0, "unknown"); err == nil {
		t.Fatal("want an error for a Transaction with no events")
	}
}

func TestPollFollowsATransactionThroughEventsTheFilterHides(t *testing.T) {
	src := &fakeSource{batches: [][]row{{
		streamEvent(t, "transaction", "tx", 1, &txpb.TransactionInitialized{Id: "tx"}),
		streamEvent(t, "transaction", "other", 1, &txpb.TransactionInitialized{Id: "other"}),
		streamEvent(t, "transaction", "tx", 2, &txpb.TransactionStarted{Id: "tx"}),
	}}}
	var out bytes.Buffer
	tl := tailer{
		source: src, out: &out, follow: newFollower("tx"), render: renderer{location: time.UTC},
		cursor: newCursor(0, 5*time.Second), now: func() time.Time { return t0 },
	}

	if err := tl.poll(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(out.String(), "\n"); got != 2 || strings.Contains(out.String(), "other") {
		t.Fatalf("printed:\n%s\nwant only tx's 2 events", out.String())
	}
}

func TestRunOnceReadsToTheHeadAndStops(t *testing.T) {
	src := &fakeSource{batches: [][]row{
		{eventRow(t, 98, &txpb.TransactionInitialized{})},
		{eventRow(t, 100, &txpb.TransactionStarted{})},
	}}
	var out bytes.Buffer
	tl := tailer{
		source: src, out: &out, render: renderer{location: time.UTC},
		cursor: newCursor(97, 5*time.Second), now: func() time.Time { return t0 },
	}

	if err := tl.run(context.Background(), time.Hour, true); err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(out.String(), "\n"); got != 2 {
		t.Fatalf("printed %d events, want 2:\n%s", got, out.String())
	}
}
