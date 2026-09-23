// Command orchestrator consumes the published domain events and drives the
// Transfer and Transaction sagas forward in response. See internal/saga and
// go/docs/adr/0001 and 0003.
//
// It is a separate binary from cmd/server rather than a goroutine inside it.
// The two have opposite failure requirements: the RPC server should stay up
// answering reads while something is wrong with consumption, and the
// orchestrator should stop dead on a message it cannot process
// (go/docs/adr/0003) without taking the API down with it. Separating them also
// means saga throughput and API traffic scale independently, and it is what
// let the cutover be staged — this consumer ran everywhere first, and only then
// was the synchronous dispatch removed.
//
// Since that removal (go/docs/adr/0006) this is the only thing that advances a
// saga. cmd/server records decisions and answers; nothing in its RPC surface
// dispatches anything. So this process is not an optional accelerator: with it
// stopped, or with publication stalled, accepted work sits where it was
// accepted. cmd/resume is the out-of-band way to move one aggregate by hand
// when no trigger will ever arrive for it.
//
//	DATABASE_URL=postgres://... DATABASE_MAX_CONNS=16 KAFKA_BROKERS=kafka:9092 \
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
	defaultDatabaseURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable"
	// defaultDatabaseMaxConns is deliberately explicit rather than pgxpool's
	// own default (max(4, runtime.NumCPU())) — see cmd/server's own constant
	// of the same name for why that matters. This orchestrator handles one
	// trigger at a time per partition, every partition at once (saga.Consumer),
	// across both aggregate-type topics — 2 × 6 partitions on the dev topics —
	// so the pool is sized to let each of those hold a connection with a
	// little headroom, rather than queueing them behind one another. It stays
	// below cmd/server's to leave the rest of Postgres's max_connections=100
	// for it and everything else sharing the instance; raise it with the
	// partition count.
	defaultDatabaseMaxConns     = "16"
	defaultKafkaBrokers         = "localhost:9092"
	defaultTigerBeetleAddress   = "127.0.0.1:3000"
	defaultTigerBeetleClusterID = "0"

	// groupPrefix names this orchestrator's consumer groups. One group per
	// aggregate type, per root docs/adr/0001 decision 6: separate offsets and
	// separate lag to watch per topic. It does not give one topic's halt any
	// fault isolation from the other's — run cancels both on the first error,
	// per go/docs/adr/0003.
	groupPrefix = "money-flow-saga-"
)

// poolConfig parses databaseURL into a pgxpool.Config with MaxConns
// overridden to maxConns — see cmd/server's own poolConfig for why this is
// a separate override rather than a "?pool_max_conns=" query parameter on
// DATABASE_URL itself.
func poolConfig(databaseURL string, maxConns int32) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	config.MaxConns = maxConns
	return config, nil
}

// consumedAggregateTypes is what this orchestrator subscribes to. Both are
// needed and neither is optional: a Transaction cannot decide whether its
// children are done without hearing from the transfer topic, and a Transfer
// that has just been dispatched cannot be reached without the transaction one.
var consumedAggregateTypes = []string{transfer.AggregateType, transaction.AggregateType}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	maxConns, err := strconv.ParseInt(env("DATABASE_MAX_CONNS", defaultDatabaseMaxConns), 10, 32)
	if err != nil {
		log.Fatalf("orchestrator: DATABASE_MAX_CONNS: %v", err)
	}
	cfg, err := poolConfig(env("DATABASE_URL", defaultDatabaseURL), int32(maxConns))
	if err != nil {
		log.Fatalf("orchestrator: database url: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

	if err := run(ctx, kafkaTransport(brokers), orchestrator); err != nil {
		log.Fatalf("orchestrator: %v", err)
	}
	log.Print("orchestrator: stopped")
}

// topicReader is the transport a consumer holds: a saga.Reader whose group
// membership has to be released when it stops.
type topicReader interface {
	saga.Reader
	Close() error
}

// transport is the two Kafka calls run makes, held as values rather than made
// directly, so that run's own start-up — which topic blocks what — is
// exercisable without a broker.
type transport struct {
	waitForTopic func(ctx context.Context, topic string) error
	newReader    func(groupID, topic string) topicReader
}

func kafkaTransport(brokers []string) transport {
	return transport{
		waitForTopic: func(ctx context.Context, topic string) error {
			return kafkareader.WaitForTopic(ctx, brokers, topic)
		},
		newReader: func(groupID, topic string) topicReader {
			return kafkareader.New(brokers, groupID, topic)
		},
	}
}

// run consumes every topic until the process is asked to stop or one of the
// consumers gives up.
//
// A halt on one topic cancels the other. The two are not independent in
// practice — a Transaction stuck because transfer triggers stopped arriving is
// not usefully "still working" — and stopping together makes the failure
// visible as one incident rather than as a partial system that looks healthy.
func run(ctx context.Context, tr transport, handler saga.Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	for _, aggregateType := range consumedAggregateTypes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := consume(ctx, tr, aggregateType, handler); err != nil {
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
	return first
}

// consume waits for one topic to exist, opens its reader, and consumes it
// until ctx is cancelled or the consumer gives up.
//
// The wait belongs here, inside the topic's own goroutine, rather than in
// run's loop: waited for in sequence, a topic nothing has published to yet
// holds up every consumer behind it. On a fresh CDC database that is the
// ordinary starting state rather than a corner of one — a Transaction whose
// children are all auto_process:false, or one rejected at initialization,
// leaves transfer-events uncreated — and after the cutover it is a bootstrap
// deadlock, because the only thing that can dispatch the first Transfer is a
// transaction trigger this orchestrator would be blocked from consuming.
func consume(ctx context.Context, tr transport, aggregateType string, handler saga.Handler) error {
	topic, group := saga.Topic(aggregateType), groupPrefix+aggregateType

	// Before the reader, not after: a consumer group that forms while its
	// topic does not exist is assigned nothing and never recovers. See
	// kafkareader.WaitForTopic.
	if err := tr.waitForTopic(ctx, topic); err != nil {
		if ctx.Err() != nil {
			return nil // shutting down before we got started
		}
		return err
	}

	reader := tr.newReader(group, topic)
	defer func() {
		if err := reader.Close(); err != nil {
			log.Printf("orchestrator: %v", err)
		}
	}()

	log.Printf("orchestrator: consuming %s as %s", topic, group)
	return saga.NewConsumer(reader, handler).Run(ctx)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
