package tlatrace_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/tlatrace"
)

const aggregate = "holder"

func established(id string) *holderpb.HolderEstablished { return &holderpb.HolderEstablished{Id: id} }

func onlyRecord(t *testing.T, rec *tlatrace.Recorder) tlatrace.Record {
	t.Helper()
	records := rec.Records()
	if len(records) != 1 {
		t.Fatalf("Records() = %+v, want exactly one", records)
	}
	return records[0]
}

// A Load is recorded as the database answered it: the stream length the
// handler will decide against, and the event types it saw.
func TestStore_RecordsALoadAsTheDatabaseAnsweredIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := eventstore.NewMemoryStore()
	stream := testutil.ID("load-stream")
	if err := inner.Append(ctx, aggregate, stream, 0, established(stream)); err != nil {
		t.Fatalf("seed Append() error = %v", err)
	}
	rec := tlatrace.NewRecorder()

	events, err := tlatrace.NewStore(inner, rec).Load(tlatrace.WithRequest(ctx, "r1"), aggregate, stream)
	if err != nil || len(events) != 1 {
		t.Fatalf("Load() = %d events, %v; want 1, nil", len(events), err)
	}

	got := onlyRecord(t, rec)
	if got.Kind != tlatrace.KindLoad || got.Req != "r1" || got.Stream != stream {
		t.Errorf("record = %+v, want a Load by r1 on %s", got, stream)
	}
	if got.Len == nil || *got.Len != 1 {
		t.Errorf("Len = %v, want 1", got.Len)
	}
	if want := eventstore.EventType(established(stream)); len(got.Types) != 1 || got.Types[0] != want {
		t.Errorf("Types = %v, want [%s]", got.Types, want)
	}
}

// An empty stream is still a Load of length 0, not a missing length: the
// spec decides against it.
func TestStore_RecordsAnEmptyStreamAsLengthZero(t *testing.T) {
	t.Parallel()
	rec := tlatrace.NewRecorder()

	if _, err := tlatrace.NewStore(eventstore.NewMemoryStore(), rec).Load(
		tlatrace.WithRequest(context.Background(), "r1"), aggregate, testutil.ID("empty")); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got := onlyRecord(t, rec); got.Len == nil || *got.Len != 0 {
		t.Errorf("Len = %v, want 0", got.Len)
	}
}

// Each Append records whether the INSERT landed, lost the race for its
// sequence, or failed some other way: the three branches of the spec's
// TryAppend and Fail.
func TestStore_RecordsEachAppendsOutcome(t *testing.T) {
	t.Parallel()
	ctx := tlatrace.WithRequest(context.Background(), "r1")
	stream := testutil.ID("append-stream")
	rec := tlatrace.NewRecorder()
	store := tlatrace.NewStore(eventstore.NewMemoryStore(), rec)

	if err := store.Append(ctx, aggregate, stream, 0, established(stream)); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}
	if err := store.Append(ctx, aggregate, stream, 0, established(stream)); !errors.Is(err, eventstore.ErrConcurrencyConflict) {
		t.Fatalf("second Append() error = %v, want ErrConcurrencyConflict", err)
	}
	if err := store.Append(ctx, aggregate, stream, -1, established(stream)); err == nil {
		t.Fatal("Append() at a negative expectedSeq succeeded, want an error")
	}

	records := rec.Records()
	want := []string{tlatrace.OutcomeOK, tlatrace.OutcomeConflict, tlatrace.OutcomeError}
	if len(records) != len(want) {
		t.Fatalf("Records() = %+v, want %d Appends", records, len(want))
	}
	for i, r := range records {
		if r.Kind != tlatrace.KindAppend || r.Outcome != want[i] {
			t.Errorf("record %d = %+v, want an Append with outcome %q", i, r, want[i])
		}
	}
	if seq := records[1].ExpectedSeq; seq == nil || *seq != 0 {
		t.Errorf("conflicting Append's ExpectedSeq = %v, want 0", seq)
	}
	if want := eventstore.EventType(established(stream)); len(records[0].Types) != 1 || records[0].Types[0] != want {
		t.Errorf("Types = %v, want [%s]", records[0].Types, want)
	}
}

// A write made on nobody's behalf is still written down, so a trace can't
// silently leave out a writer the spec doesn't model.
func TestStore_RecordsUnattributedCalls(t *testing.T) {
	t.Parallel()
	rec := tlatrace.NewRecorder()
	stream := testutil.ID("unattributed")

	if err := tlatrace.NewStore(eventstore.NewMemoryStore(), rec).Append(
		context.Background(), aggregate, stream, 0, established(stream)); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	if got := onlyRecord(t, rec); got.Req != "" || got.Stream != stream {
		t.Errorf("record = %+v, want an Append on %s by no request", got, stream)
	}
}

// AppendAtomic is outside the modeled design, so it is recorded as its own
// kind with every stream it wrote, for validation to reject rather than skip.
func TestStore_RecordsAppendAtomicWithEveryStreamItWrote(t *testing.T) {
	t.Parallel()
	a, b := testutil.ID("atomic-a"), testutil.ID("atomic-b")
	rec := tlatrace.NewRecorder()

	err := tlatrace.NewStore(eventstore.NewMemoryStore(), rec).AppendAtomic(
		tlatrace.WithRequest(context.Background(), "r1"),
		eventstore.StreamWrite{AggregateType: aggregate, AggregateID: a, Events: []proto.Message{established(a)}},
		eventstore.StreamWrite{AggregateType: aggregate, AggregateID: b, Events: []proto.Message{established(b)}},
	)
	if err != nil {
		t.Fatalf("AppendAtomic() error = %v", err)
	}

	got := onlyRecord(t, rec)
	if got.Kind != tlatrace.KindAppendAtomic || got.Outcome != tlatrace.OutcomeOK {
		t.Errorf("record = %+v, want a successful AppendAtomic", got)
	}
	if len(got.Streams) != 2 || got.Streams[0] != a || got.Streams[1] != b {
		t.Errorf("Streams = %v, want [%s %s]", got.Streams, a, b)
	}
}

type unreachableStore struct{ eventstore.Store }

var errUnreachable = errors.New("database unreachable")

func (unreachableStore) Load(context.Context, string, string) ([]eventstore.Event, error) {
	return nil, errUnreachable
}

// A Load that failed observed nothing, so there is nothing to record; the
// handler's Failed answer is what the trace sees.
func TestStore_DoesNotRecordAFailedLoad(t *testing.T) {
	t.Parallel()
	rec := tlatrace.NewRecorder()

	_, err := tlatrace.NewStore(unreachableStore{eventstore.NewMemoryStore()}, rec).Load(
		tlatrace.WithRequest(context.Background(), "r1"), aggregate, testutil.ID("unreachable"))

	if !errors.Is(err, errUnreachable) {
		t.Errorf("Load() error = %v, want %v", err, errUnreachable)
	}
	if records := rec.Records(); len(records) != 0 {
		t.Errorf("Records() = %+v, want none", records)
	}
}
