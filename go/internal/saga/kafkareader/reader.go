// Package kafkareader adapts a Kafka consumer group to saga.Reader.
//
// It is the only place in the orchestrator that knows Kafka exists. Everything
// the sagas actually depend on — parsing a trigger, driving an aggregate,
// deciding what a failure means — lives in package saga and is exercised
// without a broker. What is left here is transport: connect, fetch, commit,
// close.
package kafkareader

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/namelessnotion/money_flow/go/internal/saga"
)

// maxWait bounds how long a fetch blocks with nothing to read before returning
// to the loop. It is what makes shutdown prompt on an idle topic, which is the
// normal state of a low-volume event log, so it is deliberately short rather
// than kafka-go's ten-second default.
const maxWait = time.Second

// topicPollInterval is how often WaitForTopic re-checks for a topic that is
// not there yet.
const topicPollInterval = 2 * time.Second

// WaitForTopic blocks until topic exists and has at least one partition.
//
// This is not defensive tidiness, it is required. A consumer group that forms
// while its topic does not yet exist is assigned no partitions — which is
// correct and matches every other Kafka client — and kafka-go's partition
// watcher, the thing that is supposed to notice the topic appearing and
// rebalance, gives up permanently in exactly that case: its first
// readPartitions call returns UnknownTopicOrPartition and it returns rather
// than retrying. The reader then sits in a healthy-looking generation reading
// nothing, forever, and only a restart fixes it.
//
// Observed on the first local run against the CDC rig, where the topics are
// created by the connector's first publication rather than up front, so
// starting the orchestrator before anything had been published was the normal
// case rather than a corner of one.
//
// Waiting here also gives an operator the right signal: an orchestrator that
// logs "waiting for transfer-events" is telling them the connector is not
// registered, which silence never would.
func WaitForTopic(ctx context.Context, brokers []string, topic string) error {
	announced := false
	for {
		switch exists, err := topicExists(ctx, brokers, topic); {
		case err != nil:
			log.Printf("kafkareader: %s: checking for topic: %v", topic, err)
		case exists:
			return nil
		case !announced:
			log.Printf("kafkareader: waiting for topic %s; nothing has been published to it yet", topic)
			announced = true
		}

		timer := time.NewTimer(topicPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func topicExists(ctx context.Context, brokers []string, topic string) (bool, error) {
	conn, err := (&kafka.Dialer{}).DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return false, fmt.Errorf("dial %s: %w", brokers[0], err)
	}
	defer func() { _ = conn.Close() }()

	partitions, err := conn.ReadPartitions(topic)
	if err != nil {
		if errors.Is(err, kafka.UnknownTopicOrPartition) {
			return false, nil
		}
		return false, err
	}
	return len(partitions) > 0, nil
}

// Reader consumes one topic as a member of one consumer group.
//
// One group per topic, not one group across both, per root docs/adr/0001
// decision 6: the transfer and transaction topics get separate offsets,
// separate lag to watch, and separate blast radius when one of them halts.
// It also keeps the concurrency go/docs/adr/0001 predicted genuinely
// concurrent — a transfer-topic message and a transaction-topic message can
// reach the same Transaction's stream at the same time, which is exactly the
// path that needed proving rather than assuming.
type Reader struct {
	reader *kafka.Reader
}

// New opens a group reader for topic. Nothing connects until the first Fetch,
// so call WaitForTopic first: a group that forms before its topic exists never
// recovers on its own.
func New(brokers []string, groupID, topic string) *Reader {
	return &Reader{reader: kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		GroupID: groupID,
		Topic:   topic,
		MaxWait: maxWait,
		// A new group starts at the beginning of the topic. A trigger is
		// idempotent by construction, so replaying the whole log costs time and
		// changes nothing; starting at the end would silently skip every
		// aggregate that moved while no consumer existed.
		StartOffset: kafka.FirstOffset,
		// Offsets are committed explicitly, after the handler has run. This is
		// kafka-go's default, restated because at-least-once delivery depends
		// on it (go/docs/adr/0001 decision 3).
		CommitInterval: 0,
		// Rebalance when partitions are added to a topic that already exists.
		// It does NOT cover a topic that does not exist yet — see
		// WaitForTopic, which is what actually handles that case.
		WatchPartitionChanges: true,
		// Without this the transport fails silently: a reader that cannot reach
		// a broker, or cannot join its group, simply returns nothing, which is
		// indistinguishable from an idle topic — and an idle topic is the
		// normal state here, so there is nothing else to notice it by.
		ErrorLogger: kafka.LoggerFunc(func(format string, args ...any) {
			log.Printf("kafkareader: %s: %s", topic, fmt.Sprintf(format, args...))
		}),
	})}
}

func (r *Reader) Fetch(ctx context.Context) (saga.Message, error) {
	// FetchMessage, not ReadMessage: ReadMessage commits as it reads, which
	// would make delivery at-most-once and lose exactly the wake-ups this
	// orchestrator exists to deliver.
	m, err := r.reader.FetchMessage(ctx)
	if err != nil {
		return saga.Message{}, err
	}
	return saga.Message{
		Topic:     m.Topic,
		Partition: m.Partition,
		Offset:    m.Offset,
		Key:       m.Key,
		Value:     m.Value,
	}, nil
}

func (r *Reader) Commit(ctx context.Context, m saga.Message) error {
	return r.reader.CommitMessages(ctx, kafka.Message{
		Topic:     m.Topic,
		Partition: m.Partition,
		Offset:    m.Offset,
	})
}

// Close releases the group membership. Skipping it leaves the group waiting
// out its session timeout before rebalancing, which delays the next start.
func (r *Reader) Close() error {
	if err := r.reader.Close(); err != nil {
		return fmt.Errorf("kafkareader: close: %w", err)
	}
	return nil
}
