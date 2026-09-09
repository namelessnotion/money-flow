package kafkareader

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// scriptedCheck stands in for the dial-and-read-partitions call, so the waiting
// policy — which broker gets asked, how long an unreachable cluster is
// tolerated — is exercisable without a broker. Each call advances the fake
// clock by one poll interval, which is what a real poll costs in wall time and
// what the unreachable budget is measured against.
type scriptedCheck struct {
	asked   []string
	results []checkResult
	clock   time.Time
}

type checkResult struct {
	exists bool
	err    error
}

func (s *scriptedCheck) check(_ context.Context, broker, _ string) (bool, error) {
	s.asked = append(s.asked, broker)
	s.clock = s.clock.Add(pollInterval)
	r := s.results[0]
	if len(s.results) > 1 {
		s.results = s.results[1:]
	}
	return r.exists, r.err
}

func (s *scriptedCheck) now() time.Time { return s.clock }

// repeat scripts one result for every remaining check.
func repeat(r checkResult) []checkResult { return []checkResult{r} }

var errUnreachable = errors.New("connection refused")

// pollInterval is short enough that a test spends microseconds where the real
// one spends seconds.
const pollInterval = time.Millisecond

func waiter(brokers []string, script *scriptedCheck) topicWait {
	return topicWait{
		brokers:        brokers,
		topic:          "transfer-events",
		check:          script.check,
		now:            script.now,
		pollInterval:   pollInterval,
		unreachableFor: 20 * pollInterval,
		logger:         log.New(io.Discard, "", 0),
	}
}

func TestTopicWait_AsksTheNextBrokerWhenTheFirstIsDown(t *testing.T) {
	t.Parallel()
	script := &scriptedCheck{results: []checkResult{
		{err: errUnreachable},
		{err: errUnreachable},
		{exists: true},
	}}
	w := waiter([]string{"down:9092", "also-down:9092", "up:9092"}, script)

	if err := w.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v, want nil once a broker answers", err)
	}
	if got, want := strings.Join(script.asked, ","), "down:9092,also-down:9092,up:9092"; got != want {
		t.Errorf("asked %s, want every broker in turn: %s", got, want)
	}
}

func TestTopicWait_StopsAskingOnceABrokerAnswers(t *testing.T) {
	t.Parallel()
	script := &scriptedCheck{results: repeat(checkResult{exists: true})}
	w := waiter([]string{"up:9092", "unused:9092"}, script)

	if err := w.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if got := strings.Join(script.asked, ","); got != "up:9092" {
		t.Errorf("asked %s, want only the broker that answered", got)
	}
}

func TestTopicWait_KeepsWaitingWhileTheClusterIsReachable(t *testing.T) {
	t.Parallel()
	// A reachable cluster with no such topic yet is the ordinary starting state
	// on a fresh CDC database: wait, do not fail.
	script := &scriptedCheck{results: repeat(checkResult{exists: false})}
	w := waiter([]string{"up:9092"}, script)

	ctx, cancel := context.WithTimeout(context.Background(), 50*pollInterval)
	defer cancel()
	if err := w.run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run() error = %v, want the context error", err)
	}
	if len(script.asked) < 2 {
		t.Errorf("asked %d time(s), want repeated polling", len(script.asked))
	}
}

func TestTopicWait_ReturnsWhenTheTopicAppears(t *testing.T) {
	t.Parallel()
	script := &scriptedCheck{results: []checkResult{
		{exists: false},
		{exists: false},
		{exists: true},
	}}
	w := waiter([]string{"up:9092"}, script)

	if err := w.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}
	if len(script.asked) != 3 {
		t.Errorf("asked %d time(s), want 3", len(script.asked))
	}
}

func TestTopicWait_GivesUpWhenNoBrokerIsReachable(t *testing.T) {
	t.Parallel()
	script := &scriptedCheck{results: repeat(checkResult{err: errUnreachable})}
	w := waiter([]string{"down:9092", "also-down:9092"}, script)

	// A deadline far beyond the budget: the point is that run gives up on its
	// own rather than blocking until something else cancels it.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	err := w.run(ctx)
	if err == nil {
		t.Fatal("run() error = nil, want a startup failure")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run() error = %v, want its own error rather than the context's", err)
	}
	if !errors.Is(err, errUnreachable) {
		t.Errorf("run() error = %v, want it to wrap what the brokers reported", err)
	}
	for _, broker := range []string{"down:9092", "also-down:9092"} {
		if !strings.Contains(err.Error(), broker) {
			t.Errorf("run() error = %q, want it to name %s", err, broker)
		}
	}
}

func TestTopicWait_ForgivesAnOutageThatClears(t *testing.T) {
	t.Parallel()
	// The budget is for a cluster that stays unreachable. A broker that goes
	// away and comes back before it runs out is not a reason to refuse to start,
	// and the time it was away is not held against the next outage.
	script := &scriptedCheck{results: []checkResult{
		{err: errUnreachable},
		{err: errUnreachable},
		{exists: false},
		{err: errUnreachable},
		{err: errUnreachable},
		{exists: true},
	}}
	w := waiter([]string{"restarting:9092"}, script)
	w.unreachableFor = 3 * pollInterval

	if err := w.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v, want nil once the cluster comes back", err)
	}
	if len(script.asked) != 6 {
		t.Errorf("asked %d time(s), want 6", len(script.asked))
	}
}

func TestWaitForTopic_RejectsAnEmptyBrokerList(t *testing.T) {
	t.Parallel()
	// Without this an empty list reads as "every broker answered, no topic",
	// which is an indefinite wait for a topic nothing is being asked about.
	if err := WaitForTopic(context.Background(), nil, "transfer-events"); err == nil {
		t.Fatal("WaitForTopic() error = nil, want a configuration failure")
	}
}
