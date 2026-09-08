// Command orchestrator consumes the published domain events and drives the
// Transfer and Transaction sagas forward in response. See internal/saga and
// go/docs/adr/0001 and 0003.
//
// It is a separate binary from cmd/server rather than a goroutine inside it.
// The two have opposite failure requirements: the RPC server should stay up
// answering reads while something is wrong with consumption, and the
// orchestrator should stop dead on a message it cannot process
// (go/docs/adr/0003) without taking the API down with it. Separating them also
// means saga throughput and API traffic scale independently, and that the
// eventual cutover can be staged — the consumer already running everywhere
// before the synchronous dispatch is removed.
//
// It runs alongside the synchronous saga, not instead of it. Every RPC handler
// still drives its own saga in process; this adds a second driver of the same
// sagas, which is safe precisely because resuming an aggregate that has already
// reached its next wait state does nothing.
//
//	DATABASE_URL=postgres://... KAFKA_BROKERS=kafka:9092 \
//	TIGERBEETLE_ADDRESS=127.0.0.1:3000 TIGERBEETLE_CLUSTER_ID=0 \
//	go run ./cmd/orchestrator
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/saga/kafkareader"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

const (
	defaultDatabaseURL          = "postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable"
	defaultKafkaBrokers         = "localhost:9092"
	defaultTigerBeetleAddress   = "127.0.0.1:3000"
	defaultTigerBeetleClusterID = "0"

	// groupPrefix names this orchestrator's consumer groups. One group per
	// aggregate type, per root docs/adr/0001 decision 6: separate offsets,
	// separate lag to watch, and a halt on one topic that does not stop the
	// other.
	groupPrefix = "money-flow-saga-"
)

// consumedAggregateTypes is what this orchestrator subscribes to. Both are
// needed and neither is optional: a Transaction cannot decide whether its
// children are done without hearing from the transfer topic, and a Transfer
// that has just been dispatched cannot be reached without the transaction one.
var consumedAggregateTypes = []string{transfer.AggregateType, transaction.AggregateType}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, env("DATABASE_URL", defaultDatabaseURL))
	if err != nil {
		log.Fatalf("orchestrator: pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("orchestrator: ping: %v", err)
	}

	clusterID, err := strconv.ParseUint(env("TIGERBEETLE_CLUSTER_ID", defaultTigerBeetleClusterID), 10, 64)
	if err != nil {
		log.Fatalf("orchestrator: TIGERBEETLE_CLUSTER_ID: %v", err)
	}
	// The orchestrator drives the same sagas the RPC server does, and those
	// sagas stage, post and void in TigerBeetle. It is not a read-only
	// consumer and cannot run without the ledger.
	tb, err := ledger.NewRealClient(clusterID, strings.Split(env("TIGERBEETLE_ADDRESS", defaultTigerBeetleAddress), ","))
	if err != nil {
		log.Fatalf("orchestrator: tigerbeetle: %v", err)
	}
	defer tb.Close()

	brokers := strings.Split(env("KAFKA_BROKERS", defaultKafkaBrokers), ",")
	orchestrator := saga.Wire(eventstore.NewPostgresStore(pool), tb).Orchestrator()

	if err := run(ctx, brokers, orchestrator); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
	log.Print("orchestrator: stopped")
}

// run consumes every topic until the process is asked to stop or one of the
// consumers gives up.
//
// A halt on one topic cancels the other. The two are not independent in
// practice — a Transaction stuck because transfer triggers stopped arriving is
// not usefully "still working" — and stopping together makes the failure
// visible as one incident rather than as a partial system that looks healthy.
func run(ctx context.Context, brokers []string, orchestrator *saga.Orchestrator) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		first   error
		readers []*kafkareader.Reader
	)
	for _, aggregateType := range consumedAggregateTypes {
		topic := saga.Topic(aggregateType)

		// Before the reader, not after: a consumer group that forms while its
		// topic does not exist is assigned nothing and never recovers. See
		// kafkareader.WaitForTopic.
		if err := kafkareader.WaitForTopic(ctx, brokers, topic); err != nil {
			if ctx.Err() != nil {
				break // shutting down before we got started
			}
			return err
		}

		reader := kafkareader.New(brokers, groupPrefix+aggregateType, topic)
		readers = append(readers, reader)

		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("orchestrator: consuming %s as %s", topic, groupPrefix+aggregateType)
			if err := saga.NewConsumer(reader, orchestrator).Run(ctx); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
				cancel()
			}
		}()
	}

	wg.Wait()
	for _, reader := range readers {
		if err := reader.Close(); err != nil {
			log.Printf("orchestrator: %v", err)
		}
	}
	return first
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
