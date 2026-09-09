// Package saga is the event-triggered orchestrator: it consumes the domain
// events published from the event log and drives the Transfer and Transaction
// sagas forward in response.
//
// It exists because a saga step that waits on the outside world — a staged
// Transfer settling, a Reversal committing — has, until now, only ever been
// resumed by whichever RPC happened to touch the same id next. Consuming the
// published events gives every aggregate a durable, resumable wake-up of its
// own.
//
// # A delivered message is a trigger, never data
//
// Per go/docs/adr/0001, a message means only "this aggregate moved". The
// orchestrator reads the aggregate's identity off the envelope and then
// re-folds authoritative state from Postgres by resuming the aggregate's own
// saga. The published payload is never decoded — it is not present in the
// envelope this package parses at all, and TestPackageNeverDecodesPayload
// keeps it that way.
//
// Two consequences follow, and both are load-bearing:
//
//   - Idempotency lives on the write side. transfer and transaction each guard
//     their own appendSagaStep against recording a fact twice, so a
//     redelivered trigger converges rather than duplicating. There is
//     deliberately no read-side processed-message table.
//   - At-least-once delivery is sufficient. Offsets are committed after the
//     side effects, so a crash redelivers, and redelivery re-folds.
//
// # It runs alongside the synchronous saga, not instead of it
//
// Every RPC handler in transfer and transaction still runs its saga in
// process. This package adds a second driver of the same sagas rather than
// replacing the first, which is only safe because resuming an aggregate that
// has already reached its next wait state does nothing at all. That property
// is what the cutover will eventually rest on, so it is tested here directly.
package saga

import (
	"encoding/json"
	"fmt"
)

// Trigger is one delivered message, reduced to the only thing the orchestrator
// acts on: which aggregate moved.
//
// EventType, Sequence and GlobalSeq are carried for operators — a log line
// naming the event that woke a Transaction is worth a great deal during an
// incident — and for nothing else. Deciding anything from them would be
// reading the message as data, which go/docs/adr/0001 rejected on measured
// liveness grounds.
type Trigger struct {
	AggregateType string
	AggregateID   string

	EventType string
	Sequence  int64
	GlobalSeq int64
}

func (t Trigger) String() string {
	return fmt.Sprintf("%s %s (%s seq %d, global_seq %d)", t.AggregateType, t.AggregateID, t.EventType, t.Sequence, t.GlobalSeq)
}

// envelope is the published message body, as much of it as this package reads.
//
// The published envelope also carries the domain event itself, base64-encoded.
// There is deliberately no field for it here: a struct that cannot hold the
// payload cannot be tempted into decoding it, which makes go/docs/adr/0001's
// central decision structural rather than a convention to be remembered.
type envelope struct {
	AggregateType string `json:"aggregate_type"`
	EventType     string `json:"event_type"`
	Sequence      int64  `json:"sequence"`
	GlobalSeq     int64  `json:"global_seq"`
}

// ParseTrigger reads one delivered message into a Trigger.
//
// The aggregate's id comes from the message key rather than the body, because
// that is where the publication contract puts it: the connector routes
// aggregate_id to the key so that one aggregate's events share a partition and
// stay in sequence order (root docs/adr/0001, decisions 3 and 4). The body
// carries aggregate_type, which is what says who to wake.
func ParseTrigger(key, value []byte) (Trigger, error) {
	if len(key) == 0 {
		return Trigger{}, fmt.Errorf("saga: message has no key, so it names no aggregate")
	}
	if len(value) == 0 {
		return Trigger{}, fmt.Errorf("saga: message for %q has an empty body", key)
	}

	var env envelope
	if err := json.Unmarshal(value, &env); err != nil {
		return Trigger{}, fmt.Errorf("saga: message for %q: %w", key, err)
	}
	if env.AggregateType == "" {
		return Trigger{}, fmt.Errorf("saga: message for %q carries no aggregate_type", key)
	}

	return Trigger{
		AggregateType: env.AggregateType,
		AggregateID:   string(key),
		EventType:     env.EventType,
		Sequence:      env.Sequence,
		GlobalSeq:     env.GlobalSeq,
	}, nil
}
