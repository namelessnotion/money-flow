package kafkareader

import (
	"context"
	"errors"
	"fmt"

	"github.com/segmentio/kafka-go"
)

// GroupLag measures how far group is behind on topic: for each partition, the
// messages published that the group has not committed. A topic that does not
// exist yet has no partitions and so no lag.
//
// It asks the brokers directly instead of reading the Reader's own statistics.
// Those only count what this process has fetched, so they say nothing about a
// consumer that has stopped fetching, and a stopped consumer is the case that
// matters (go/docs/adr/0003).
//
// Like WaitForTopic, it goes through kafka.Client's Metadata, which never asks
// the broker to create the topic it names.
func GroupLag(ctx context.Context, brokers []string, group, topic string) (map[int]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	client := &kafka.Client{Addr: kafka.TCP(brokers...)}

	meta, err := client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return nil, fmt.Errorf("kafkareader: %s: metadata: %w", topic, err)
	}
	if len(meta.Topics) != 1 {
		return nil, fmt.Errorf("kafkareader: %s: metadata: got %d topic(s), want 1", topic, len(meta.Topics))
	}
	if err := meta.Topics[0].Error; err != nil {
		// Nothing has been published to it yet (see WaitForTopic), so nothing
		// is owed. That is the ordinary state of a fresh CDC database, not a
		// failure to report every time metrics are collected.
		if errors.Is(err, kafka.UnknownTopicOrPartition) {
			return map[int]int64{}, nil
		}
		return nil, fmt.Errorf("kafkareader: %s: metadata: %w", topic, err)
	}
	partitions := make([]int, 0, len(meta.Topics[0].Partitions))
	requests := make([]kafka.OffsetRequest, 0, 2*len(meta.Topics[0].Partitions))
	for _, p := range meta.Topics[0].Partitions {
		partitions = append(partitions, p.ID)
		requests = append(requests, kafka.FirstOffsetOf(p.ID), kafka.LastOffsetOf(p.ID))
	}

	ends, err := client.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: map[string][]kafka.OffsetRequest{topic: requests}})
	if err != nil {
		return nil, fmt.Errorf("kafkareader: %s: list offsets: %w", topic, err)
	}
	committed, err := client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group, Topics: map[string][]int{topic: partitions}})
	if err != nil {
		return nil, fmt.Errorf("kafkareader: %s: offset fetch for %s: %w", topic, group, err)
	}
	if committed.Error != nil {
		return nil, fmt.Errorf("kafkareader: %s: offset fetch for %s: %w", topic, group, committed.Error)
	}

	commits := make(map[int]int64, len(partitions))
	for _, c := range committed.Topics[topic] {
		if c.Error != nil {
			return nil, fmt.Errorf("kafkareader: %s[%d]: committed offset for %s: %w", topic, c.Partition, group, c.Error)
		}
		commits[c.Partition] = c.CommittedOffset
	}
	lag := make(map[int]int64, len(partitions))
	var errs []error
	for _, o := range ends.Topics[topic] {
		if o.Error != nil {
			errs = append(errs, fmt.Errorf("kafkareader: %s[%d]: offsets: %w", topic, o.Partition, o.Error))
			continue
		}
		c, ok := commits[o.Partition]
		if !ok {
			c = -1
		}
		lag[o.Partition] = partitionLag(c, o.FirstOffset, o.LastOffset)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return lag, nil
}

// partitionLag is the messages between where the group will next read and the
// partition's end. A committed offset below the first retained one, including
// -1 for nothing committed, resumes from the first retained message.
func partitionLag(committed, first, last int64) int64 {
	return max(0, last-max(committed, first))
}
