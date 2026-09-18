// Command events prints Go's event log as it is written, so what one request
// sets off — an ACH Transaction's saga, its Transfers, their Operations,
// Tokens and Wallets — can be watched as it happens.
//
// It reads the events table directly rather than the CDC topics, so it needs
// only Postgres: no Debezium connector, no Kafka. It is read-only.
//
//	go run ./cmd/events                         # follow from now
//	go run ./cmd/events -last 50                # the last 50, then follow
//	go run ./cmd/events -types transaction,transfer
//	go run ./cmd/events -id ab52e2c             # one aggregate, by id fragment
//	go run ./cmd/events -follow <transaction id> # one Transaction and all it set off
//	go run ./cmd/events -json | jq .            # one JSON object per event
//
// DATABASE_URL defaults to the development database on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	// Linked in so every event type is registered for decoding.
	_ "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	_ "github.com/namelessnotion/money_flow/go/gen/proto/operation/v1"
	_ "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	_ "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	_ "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	_ "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

const defaultDatabaseURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable"

// row is one event as read off the log.
type row = eventstore.Event

func main() {
	var (
		from   = flag.Int64("from", -1, "start after this global_seq (default: the end of the log)")
		last   = flag.Int64("last", 0, "start this many events back from the end of the log")
		types  = flag.String("types", "", "comma-separated aggregate types to show, e.g. transaction,transfer")
		id     = flag.String("id", "", "show only aggregates whose id contains this fragment")
		follow = flag.String("follow", "", "show one Transaction with its Transfers, Operations and Reversals, "+
			"from its first event unless -from or -last says otherwise")
		asJSON   = flag.Bool("json", false, "print one JSON object per event")
		payload  = flag.Bool("payload", true, "show each event's decoded payload")
		width    = flag.Int("width", 160, "truncate payloads to this many characters (0: never)")
		color    = flag.String("color", "auto", "colour output: auto, always or never")
		interval = flag.Duration("interval", 250*time.Millisecond, "how often to poll for new events")
		once     = flag.Bool("once", false, "print what is there and exit instead of following")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultDatabaseURL
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("events: connect: %v", err)
	}
	defer pool.Close()

	src := pgSource{pool: pool}
	start, err := startingPoint(ctx, src, *from, *last, *follow)
	if err != nil {
		log.Fatalf("events: %v", err)
	}

	useColor, err := wantColor(*color, os.Stdout)
	if err != nil {
		log.Fatalf("events: %v", err)
	}
	tl := tailer{
		source: src,
		out:    os.Stdout,
		filter: newFilter(*types, *id),
		follow: newFollower(*follow),
		render: renderer{json: *asJSON, color: useColor, payload: *payload, width: *width, location: time.Local},
		cursor: newCursor(start, 5*time.Second),
		now:    time.Now,
	}
	if !*asJSON && !*once {
		fmt.Fprintf(os.Stderr, "events: following the log after #%d (ctrl-c to stop)\n", start)
	}

	if err := tl.run(ctx, *interval, *once); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("events: %v", err)
	}
}

// startingPoint is the global_seq to read after: `from` when given, then
// `last` events back from the end of the log, then just before a followed
// Transaction's first event, and otherwise the end of the log.
func startingPoint(ctx context.Context, src source, from, last int64, followID string) (int64, error) {
	if from >= 0 {
		return from, nil
	}
	if last == 0 && followID != "" {
		first, err := src.first(ctx, followID)
		return first - 1, err
	}
	head, err := src.head(ctx)
	if err != nil {
		return 0, err
	}
	return max(head-last, 0), nil
}

func wantColor(mode string, out *os.File) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto":
		info, err := out.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "", nil
	default:
		return false, fmt.Errorf("-color must be auto, always or never, not %q", mode)
	}
}

// tailer reads the log forward from its cursor and prints what matches.
type tailer struct {
	source source
	out    io.Writer
	filter filter
	follow *follower
	render renderer
	cursor *cursor
	now    func() time.Time
}

const batchSize = 500

func (t *tailer) run(ctx context.Context, interval time.Duration, once bool) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := t.poll(ctx); err != nil {
			return err
		}
		if once {
			// Read on while the log runs past what has been printed; gaps are
			// not waited for.
			after, _ := t.cursor.position()
			if head, err := t.source.head(ctx); err != nil || head <= after {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll reads one batch, prints the events that match, and moves the cursor
// past every event read.
func (t *tailer) poll(ctx context.Context) error {
	after, holes := t.cursor.position()
	events, err := t.source.read(ctx, after, holes, batchSize)
	if err != nil {
		return err
	}
	seqs := make([]int64, 0, len(events))
	for _, e := range events {
		seqs = append(seqs, e.GlobalSeq)
		// The follower sees every event, in order, so it learns the ids of
		// what the Transaction set off even where the filter hides them.
		if !t.follow.admit(e) || !t.filter.matches(e) {
			continue
		}
		if err := t.render.write(t.out, e); err != nil {
			return err
		}
	}
	t.cursor.advance(seqs, t.now())
	return nil
}
