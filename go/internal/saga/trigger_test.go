package saga

import (
	"testing"
)

// The exact bytes docs/cdc-tracer-bullet.md recorded off transfer-events. The
// published shape is a contract with the connector configuration, so the test
// that reads it uses a message the pipeline actually produced rather than one
// invented here.
const (
	observedKey   = "01a07ef8-3431-71de-ac29-e6f034732db6"
	observedValue = `{"payload":"CiQwMWEwN2VmOC0zNDMxLTcxZGUtYWMyOS1lNmYwMzQ3MzJkYjY=",` +
		`"aggregate_type":"transfer","event_type":"transfer.v1.TransferRequestAccepted",` +
		`"sequence":1,"global_seq":1}`
)

func TestParseTrigger_ReadsAPublishedMessage(t *testing.T) {
	t.Parallel()

	got, err := ParseTrigger([]byte(observedKey), []byte(observedValue))
	if err != nil {
		t.Fatalf("ParseTrigger() error = %v", err)
	}
	want := Trigger{
		AggregateType: "transfer",
		AggregateID:   observedKey,
		EventType:     "transfer.v1.TransferRequestAccepted",
		Sequence:      1,
		GlobalSeq:     1,
	}
	if got != want {
		t.Errorf("ParseTrigger() = %+v, want %+v", got, want)
	}
}

// The aggregate's identity is the message key, not a body field — the
// connector routes aggregate_id there and nowhere else.
func TestParseTrigger_TakesTheAggregateIDFromTheKey(t *testing.T) {
	t.Parallel()

	got, err := ParseTrigger([]byte("01a07ef8-3436-7c9e-b44c-ab3a861f3f8c"),
		[]byte(`{"aggregate_type":"transaction","event_type":"transaction.v1.TransactionStarted","sequence":2,"global_seq":5}`))
	if err != nil {
		t.Fatalf("ParseTrigger() error = %v", err)
	}
	if got.AggregateID != "01a07ef8-3436-7c9e-b44c-ab3a861f3f8c" {
		t.Errorf("AggregateID = %q, want the message key", got.AggregateID)
	}
	if got.AggregateType != "transaction" {
		t.Errorf("AggregateType = %q, want transaction", got.AggregateType)
	}
}

func TestParseTrigger_RejectsMessagesThatNameNoAggregate(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ key, value string }{
		"no key":            {"", observedValue},
		"empty body":        {observedKey, ""},
		"body is not json":  {observedKey, "not json at all"},
		"no aggregate_type": {observedKey, `{"event_type":"transfer.v1.TransferRequestAccepted","sequence":1}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseTrigger([]byte(tc.key), []byte(tc.value)); err == nil {
				t.Error("ParseTrigger() = nil error, want an error")
			}
		})
	}
}

// A message whose payload is neither valid base64 nor a valid protobuf still
// parses: a trigger says only which aggregate moved, so the payload is not
// merely unused here, it is not looked at.
func TestParseTrigger_IgnoresAnUnreadablePayload(t *testing.T) {
	t.Parallel()

	got, err := ParseTrigger([]byte(observedKey),
		[]byte(`{"payload":"!!! this is not base64 !!!","aggregate_type":"transfer","event_type":"transfer.v1.TransferCommitted","sequence":4,"global_seq":9}`))
	if err != nil {
		t.Fatalf("ParseTrigger() error = %v", err)
	}
	if got.AggregateType != "transfer" || got.AggregateID != observedKey {
		t.Errorf("ParseTrigger() = %+v, want the transfer named by the key", got)
	}
}
