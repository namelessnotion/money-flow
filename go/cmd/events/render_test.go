package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	txpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

func eventRow(t *testing.T, seq int64, msg proto.Message) row {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return row{
		GlobalSeq:     seq,
		AggregateType: "transaction",
		AggregateID:   "01a0b5cf-a64c-76ea-99a0-d6270ab52e2c",
		Sequence:      2,
		EventType:     eventstore.EventType(msg),
		Payload:       payload,
		OccurredAt:    time.Date(2026, 9, 18, 18, 37, 59, 762_000_000, time.UTC),
	}
}

func render(t *testing.T, r renderer, e row) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.write(&buf, e); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRenderLineShowsWhenWhatAndWhichStream(t *testing.T) {
	r := renderer{payload: true, width: 200, location: time.UTC}

	got := render(t, r, eventRow(t, 920, &txpb.TransactionStarted{Id: "01a0b5cf-a64c-76ea-99a0-d6270ab52e2c"}))

	want := "18:37:59.762  #920  transaction  …0ab52e2c  #2  TransactionStarted  " +
		`{"id":"01a0b5cf-a64c-76ea-99a0-d6270ab52e2c"}` + "\n"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestRenderLineCanLeaveOutThePayload(t *testing.T) {
	r := renderer{payload: false, location: time.UTC}

	got := render(t, r, eventRow(t, 920, &txpb.TransactionStarted{Id: "x"}))

	if strings.Contains(got, "{") || !strings.HasSuffix(got, "TransactionStarted\n") {
		t.Fatalf("got %q, want no payload", got)
	}
}

func TestRenderLineTruncatesALongPayloadToTheWidth(t *testing.T) {
	r := renderer{payload: true, width: 30, location: time.UTC}

	got := render(t, r, eventRow(t, 1, &txpb.TransferFailedWithinTransaction{Reason: strings.Repeat("x", 100)}))

	payload := got[strings.Index(got, "{") : len(got)-1]
	if len([]rune(payload)) != 30 || !strings.HasSuffix(payload, "…") {
		t.Fatalf("payload = %q, want 30 runes ending in …", payload)
	}
}

func TestRenderLineColoursOutcomes(t *testing.T) {
	r := renderer{color: true, location: time.UTC}

	failed := render(t, r, eventRow(t, 1, &txpb.TransferFailedWithinTransaction{}))
	completed := render(t, r, eventRow(t, 2, &txpb.TransactionCompleted{}))

	if !strings.Contains(failed, red+bold+"TransferFailedWithinTransaction") {
		t.Errorf("failure not red: %q", failed)
	}
	if !strings.Contains(completed, green+bold+"TransactionCompleted") {
		t.Errorf("completion not green: %q", completed)
	}
}

func TestRenderJSONEmitsOneDecodedObjectPerLine(t *testing.T) {
	r := renderer{json: true}

	got := render(t, r, eventRow(t, 920, &txpb.TransactionStarted{Id: "abc"}))

	var decoded struct {
		GlobalSeq int64           `json:"global_seq"`
		EventType string          `json:"event_type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("not JSON: %q: %v", got, err)
	}
	if decoded.GlobalSeq != 920 || decoded.EventType != "transaction.v1.TransactionStarted" ||
		string(decoded.Payload) != `{"id":"abc"}` {
		t.Fatalf("decoded = %+v, payload %s", decoded, decoded.Payload)
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("want exactly one line, got %q", got)
	}
}

func TestRenderSaysSoWhenAPayloadCannotBeDecoded(t *testing.T) {
	r := renderer{payload: true, width: 200, location: time.UTC}
	e := eventRow(t, 1, &txpb.TransactionStarted{})
	e.EventType = "nothing.v1.Unknown"

	if got := render(t, r, e); !strings.Contains(got, "undecodable") {
		t.Fatalf("got %q, want an undecodable note", got)
	}
}

func TestRenderLinePadsTheAggregateTypeSoColumnsLineUp(t *testing.T) {
	r := renderer{location: time.UTC}
	e := eventRow(t, 1, &txpb.TransactionStarted{})
	e.AggregateType = "wallet"

	if got := render(t, r, e); !strings.Contains(got, "  wallet       …") {
		t.Fatalf("got %q, want wallet padded to %d", got, typeWidth)
	}
}
