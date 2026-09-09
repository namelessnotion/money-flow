package kafkareader

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net"
	"path/filepath"
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

// stalledBroker accepts connections and then answers nothing, which is the
// failure a dial-based check cannot see: the TCP handshake succeeds, so
// nothing reports an error, and the metadata response simply never comes.
func stalledBroker(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Held open, never written to, closed only when the test ends.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	return listener.Addr().String()
}

// The check has to observe ctx for its whole duration, not only until the
// connection is established. kafka-go's Dialer stops watching ctx once the
// dial succeeds and Conn.ReadPartitions takes no ctx at all, so a check built
// on those hangs here forever — outlasting both the unreachable budget, which
// only advances between calls that return, and SIGTERM.
func TestDialAndCheckTopic_ReturnsWhenABrokerAcceptsButNeverAnswers(t *testing.T) {
	t.Parallel()
	broker := stalledBroker(t)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := dialAndCheckTopic(ctx, broker, "transfer-events")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dialAndCheckTopic() = nil error from a broker that never answered")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("returned after %s; the check must be bounded by ctx, not by the broker", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dialAndCheckTopic() never returned; the check is not bounded by ctx")
	}
}

// checkTimeout is what bounds the same call when the ctx carries no deadline
// of its own, which is cmd/orchestrator's case: its ctx is cancelled by
// SIGTERM but never expires. Asserted as a relationship rather than a value —
// a check allowed to run longer than the unreachable budget could never be
// measured against it.
func TestCheckTimeout_IsShorterThanTheUnreachableBudget(t *testing.T) {
	t.Parallel()

	if checkTimeout >= maxUnreachable {
		t.Errorf("checkTimeout = %s, maxUnreachable = %s; a single check must not be able to outlast the budget it is measured against",
			checkTimeout, maxUnreachable)
	}
}

// ReadPartitions is the API this package must not use to answer "does this
// topic exist?". kafka-go hardcodes AllowAutoTopicCreation on the metadata
// request it sends, so on any cluster that has not disabled broker-side
// auto-creation — including this repo's own compose broker — the check
// creates the topic it was asked about and then truthfully reports it there.
// A missing connector then looks exactly like a healthy idle consumer, which
// is the failure WaitForTopic exists to make impossible, and the topics
// silently inherit broker defaults for settings root docs/adr/0001 left
// undecided.
//
// Asserted at the source rather than against a broker: the difference is
// invisible in a passing check, so nothing at runtime would ever notice the
// mistake being reintroduced. Comments are not scanned, so explaining the
// hazard stays allowed.
func TestPackageNeverAsksForPartitionsWithAutoCreation(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	fset := token.NewFileSet()
	scanned := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++

		// Parsed without ParseComments, so the AST holds code only.
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if ok && ident.Name == "ReadPartitions" {
				t.Errorf("%s: calls ReadPartitions, which sends AllowAutoTopicCreation=true; use kafka.Client.Metadata",
					fset.Position(ident.Pos()))
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files; the check would pass vacuously")
	}
}
