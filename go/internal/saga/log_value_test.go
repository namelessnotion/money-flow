package saga

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// A halt names the exact message it stopped on (go/docs/adr/0003), and the
// runbook resumes from it. As structured fields rather than prose, an operator
// can filter every line about one partition, or one aggregate, without a regex.
func TestLogValues_AreStructuredFields(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))

	logger.Info("saga: handled",
		slog.Any("message", Message{Topic: "transfer-events", Partition: 2, Offset: 17, Value: []byte("not logged")}),
		slog.Any("trigger", Trigger{
			AggregateType: "transfer", AggregateID: "t1", EventType: "transfer.v1.TransferRequestAccepted",
			Sequence: 3, GlobalSeq: 99,
		}),
	)

	var line struct {
		Message map[string]any `json:"message"`
		Trigger map[string]any `json:"trigger"`
	}
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("log output %q is not JSON: %v", out.String(), err)
	}
	wantFields(t, "message", line.Message, map[string]any{
		"topic": "transfer-events", "partition": 2.0, "offset": 17.0,
	})
	wantFields(t, "trigger", line.Trigger, map[string]any{
		"aggregate_type": "transfer", "aggregate_id": "t1", "event_type": "transfer.v1.TransferRequestAccepted",
		"sequence": 3.0, "global_seq": 99.0,
	})
}

func wantFields(t *testing.T, name string, got, want map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want exactly %v", name, got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s.%s = %v, want %v", name, k, got[k], v)
		}
	}
}
