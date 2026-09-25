package tlatrace_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/tlatrace"
)

// readTrace returns a trace file's lines, each decoded as a JSON object.
func readTrace(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	var lines []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("%s: line %d is not JSON: %v", path, len(lines)+1, err)
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return lines
}

// The handlers a trace is checked under come from the proto: the messages
// each response's result oneof can carry.
func TestDecisionEvents_AreTheMessagesAResponsesResultCanCarry(t *testing.T) {
	t.Parallel()
	got := tlatrace.DecisionEvents(&pb.RequestTransferResponse{})
	want := []string{
		eventstore.EventType(&pb.TransferRequestAccepted{}),
		eventstore.EventType(&pb.TransferRequestRejected{}),
	}
	if !slices.Equal(got, want) {
		t.Errorf("DecisionEvents() = %v, want %v", got, want)
	}
}

// Each command stream gets its own trace: the header, then every line its
// Requests recorded on it, in the order they were recorded. What those
// Requests read elsewhere (a wallet, a token) is an input to their decision,
// which the spec leaves open, so it isn't in the trace.
func TestRecorder_WritesOneTracePerCommandStream(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, b, elsewhere := testutil.ID("stream-a"), testutil.ID("stream-b"), testutil.ID("wallet")
	inner := eventstore.NewMemoryStore()
	if err := inner.Append(ctx, "wallet", elsewhere, 0, established(elsewhere)); err != nil {
		t.Fatalf("seed Append() error = %v", err)
	}
	rec := tlatrace.NewRecorder()
	store := tlatrace.NewStore(inner, rec)
	stub := &stubTransfers{resp: accepted(a)}
	client := serve(t, stub, rec)

	// r1 and r2 ask on a, r3 on b; r1 also reads a wallet.
	for _, id := range []string{a, a, b} {
		if _, err := client.RequestTransfer(ctx, &pb.RequestTransferRequest{Id: id}); err != nil {
			t.Fatalf("RequestTransfer() error = %v", err)
		}
	}
	r1 := stub.seen[0]
	if _, err := store.Load(tlatrace.WithRequest(ctx, r1), "wallet", elsewhere); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, err := store.Load(tlatrace.WithRequest(ctx, r1), "transfer", a); err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	header := tlatrace.Header{MaxAttempts: 3, Handlers: map[string][]string{
		"RequestTransfer": tlatrace.DecisionEvents(&pb.RequestTransferResponse{}),
	}}
	dir := t.TempDir()
	paths, err := rec.WriteStreams(dir, header)
	if err != nil {
		t.Fatalf("WriteStreams() error = %v", err)
	}
	slices.Sort(paths)
	want := []string{filepath.Join(dir, a+".ndjson"), filepath.Join(dir, b+".ndjson")}
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("WriteStreams() = %v, want %v", paths, want)
	}

	traceA := readTrace(t, filepath.Join(dir, a+".ndjson"))
	if traceA[0]["kind"] != "header" || traceA[0]["maxAttempts"] != float64(3) {
		t.Errorf("first line = %v, want the header", traceA[0])
	}
	var kinds []string
	for _, line := range traceA[1:] {
		if line["stream"] != a {
			t.Errorf("trace for %s holds a line on %v: %v", a, line["stream"], line)
		}
		kinds = append(kinds, line["kind"].(string))
	}
	wantKinds := []string{
		tlatrace.KindReceive, tlatrace.KindAnswer, tlatrace.KindReceive, tlatrace.KindAnswer, tlatrace.KindLoad,
	}
	if !slices.Equal(kinds, wantKinds) {
		t.Errorf("trace for %s = %v, want %v", a, kinds, wantKinds)
	}
	if traceB := readTrace(t, filepath.Join(dir, b+".ndjson")); len(traceB) != 3 {
		t.Errorf("trace for %s has %d lines, want the header, a Receive and an Answer", b, len(traceB))
	}
}

// Records from racing goroutines are kept whole and all kept.
func TestRecorder_KeepsEveryRecordFromConcurrentWriters(t *testing.T) {
	t.Parallel()
	rec := tlatrace.NewRecorder()
	store := tlatrace.NewStore(eventstore.NewMemoryStore(), rec)
	stream := testutil.ID("concurrent")

	const writers = 50
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			_, _ = store.Load(tlatrace.WithRequest(context.Background(), "r1"), "transfer", stream)
		})
	}
	wg.Wait()

	if got := len(rec.Records()); got != writers {
		t.Errorf("Records() holds %d, want %d", got, writers)
	}
}
