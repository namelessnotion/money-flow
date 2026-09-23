// Command resume drives one aggregate's saga forward by hand, out of band.
//
// Since the async cutover (go/docs/adr/0006) nothing in the RPC surface
// advances a saga: every handler records its decision and returns, and
// cmd/orchestrator folds the published event. That leaves one gap this tool
// exists to close. A trigger that is never published is a wake-up that never
// happens, and the CDC connector starts at the current end of the log — so
// anything written while it was down is never published at all, and no RPC can
// give it a nudge any more. Before the cutover the next call touching the id
// would have resumed it; now only a trigger will, and for those aggregates
// there is no trigger to come.
//
// So this is the out-of-band path, the way `make migrate` is: not something the
// system uses, something an operator runs.
//
// Two jobs:
//
//	go run ./cmd/resume 01a0... 01a1...   # drive these aggregates
//	go run ./cmd/resume -open             # drive every aggregate still in flight
//
// -open is the step to run once immediately after deploying the cutover, for
// work the old synchronous code left mid-saga: a Transaction at started whose
// child never got past accepted, or one part-way through a rollback. It is also
// the first concrete answer to go/docs/adr/0003's open question about what an
// operator can actually do with a halted consumer.
//
// It drives through the real saga.Orchestrator, so a Transfer wakes the
// Transaction that owns it exactly as a delivered trigger would. Redelivering a
// trigger is always safe (go/docs/adr/0001), so running this against an
// aggregate that needed nothing does nothing.
//
//	DATABASE_URL=postgres://... TIGERBEETLE_ADDRESS=127.0.0.1:3000 \
//	TIGERBEETLE_CLUSTER_ID=0 go run ./cmd/resume -open
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
)

const (
	defaultDatabaseURL          = "postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable"
	defaultTigerBeetleAddress   = "127.0.0.1:3000"
	defaultTigerBeetleClusterID = "0"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	open := flag.Bool("open", false, "drive every aggregate still in flight, rather than named ids")
	flag.Parse()

	if *open == (flag.NArg() > 0) {
		log.Fatal("resume: pass either -open or one or more aggregate ids, not both and not neither")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, env("DATABASE_URL", defaultDatabaseURL))
	if err != nil {
		log.Fatalf("resume: pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("resume: ping: %v", err)
	}

	clusterID, err := strconv.ParseUint(env("TIGERBEETLE_CLUSTER_ID", defaultTigerBeetleClusterID), 10, 64)
	if err != nil {
		log.Fatalf("resume: TIGERBEETLE_CLUSTER_ID: %v", err)
	}
	// Driving a saga stages, posts and voids in TigerBeetle. This is not a
	// read-only tool and cannot run without the ledger.
	tb, err := ledger.NewRealClient(clusterID, strings.Split(env("TIGERBEETLE_ADDRESS", defaultTigerBeetleAddress), ","))
	if err != nil {
		log.Fatalf("resume: tigerbeetle: %v", err)
	}
	defer tb.Close()

	store := eventstore.NewPostgresStore(pool)
	driver := driver{orchestrator: saga.Wire(store, tb).Orchestrator(), store: store}
	catalogue := catalogue{pool: pool}

	targets, err := resolveTargets(ctx, catalogue, driver, *open, flag.Args())
	if err != nil {
		log.Fatalf("resume: %v", err)
	}
	if len(targets) == 0 {
		log.Print("resume: nothing in flight")
		return
	}

	// Only aggregates that moved, or could not be, are worth a line each. Most
	// of what -open selects is waiting on the outside world — a staged ACH entry
	// legitimately sits for days — and a line per one of those buries the two
	// that matter.
	advanced, inert, failed := 0, 0, 0
	for _, t := range targets {
		rounds, err := driver.drive(ctx, t)
		switch {
		case err != nil:
			failed++
			log.Printf("resume: %s: %v", t, err)
		case rounds > 1:
			advanced++
			log.Printf("resume: %s: advanced over %d wake-ups", t, rounds-1)
		default:
			inert++
		}
	}
	log.Printf("resume: %d advanced, %d already where they should be, %d failed, of %d in flight",
		advanced, inert, failed, len(targets))
	if failed > 0 {
		log.Fatalf("resume: %d of %d aggregates could not be driven", failed, len(targets))
	}
}

// resolveTargets turns the command line into the aggregates to drive.
//
// -open asks the log which aggregates exist and each aggregate whether it is
// still going anywhere. Named ids are driven whatever state they are in: an
// operator naming an id has a reason, and redelivering a trigger to a finished
// aggregate does nothing anyway.
func resolveTargets(ctx context.Context, c catalogue, d driver, open bool, ids []string) ([]target, error) {
	if open {
		candidates, err := c.sagaStreams(ctx)
		if err != nil {
			return nil, err
		}
		targets := make([]target, 0, len(candidates))
		for _, t := range candidates {
			going, err := d.inFlight(ctx, t)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t, err)
			}
			if going {
				targets = append(targets, t)
			}
		}
		return targets, nil
	}

	targets := make([]target, 0, len(ids))
	for _, id := range ids {
		found, err := c.typesOf(ctx, id)
		if err != nil {
			return nil, err
		}
		switch len(found) {
		case 0:
			return nil, fmt.Errorf("no events for aggregate %s", id)
		case 1:
			targets = append(targets, target{aggregateType: found[0], aggregateID: id})
		default:
			// One id naming streams of two different types would mean the log
			// itself is inconsistent; guessing which was meant is not this
			// tool's call to make.
			return nil, fmt.Errorf("aggregate %s has streams of several types (%s); refusing to guess",
				id, strings.Join(found, ", "))
		}
	}
	return targets, nil
}
