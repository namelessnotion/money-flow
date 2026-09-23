// Command simulate drives N simulated transfers across M entities against a
// running go/cmd/server, each one randomly steered to either settle or roll
// back, then checks the resulting ledger for correctness: every simulated
// transfer reached a clean terminal state, every entity's balance matches
// what the run expects, and the total money held by entities is conserved.
// It reports load characteristics (throughput, latency percentiles)
// alongside the correctness report.
//
// -mode picks the shape each simulated transfer is driven in: "transaction"
// (the default) wraps every Transfer in a single-child Transaction, the
// production ACH shape; "transfer" drives TransferService.RequestTransfer
// directly, skipping Transaction/DAG dispatch entirely, to isolate
// TransferService's own throughput from that dispatch overhead.
//
// It talks to two boundaries: the running server over Twirp for every write
// (Holder/Wallet provisioning, Transaction/Transfer driving — the same
// surface Ruby drives in production), and Postgres directly, read-only — the
// same access cmd/events already uses to watch the log. It reads Postgres for
// two facts no Twirp RPC exposes: whether a Transfer leg has staged yet, which
// it must know before settling one, and each Wallet's balance, for the
// post-run correctness check. So Postgres is needed even with -skip-verify.
//
//	go run ./cmd/simulate -entities 50 -transactions 2000 -concurrency 16
//	go run ./cmd/simulate -mode transfer -entities 50 -transactions 2000 -concurrency 16
//
// SERVER_URL defaults to a local go/cmd/server; DATABASE_URL defaults to
// the same development database cmd/events and cmd/server use.
//
// go/cmd/orchestrator and a registered CDC connector are REQUIRED, not an
// optimisation for heavy runs. Since the async cutover (go/docs/adr/0006)
// nothing in the RPC surface advances a saga: every call this tool makes
// records a decision and returns, and the orchestrator folds the events that
// publishes. Without it nothing settles and every transaction reports open —
// so `make cdc-up && make orchestrator-up` before running this, and check
// `make orchestrator-logs` first if everything comes back still open.
//
// Every transaction therefore starts out open, and waiting is the normal path
// rather than a rescue. Each one is looked at up to -settle-wait-attempts more
// times, -settle-wait-delay apart, first until its leg stages — only then is
// it steered to settle or roll back, the way a provider only answers for an
// entry it has actually been sent — and then until the orchestrator has
// folded that answer. Anything still open after the load gets the same wait
// again. A transaction still open in the final report ran out of looks, not
// out of hope: looking again later may still resolve it. Seeding waits the
// same way, and fails the run if any seed does not commit, since the load
// would otherwise spend money that is not there yet.
//
// One genuine stall remains: a Transfer whose prepare step loses a concurrency
// race on a hot Wallet is left mid-flight, and only a later trigger moves it.
// Pushing -entities up relative to -concurrency reduces how often any one
// Wallet is hot enough to race in the first place.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
)

const (
	defaultServerURL   = "http://localhost:8080"
	defaultDatabaseURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable"
)

func main() {
	var (
		mode           = flag.String("mode", "transaction", `simulation mode: "transaction" (wrap each Transfer in a single-child Transaction, the production ACH shape) or "transfer" (drive TransferService.RequestTransfer directly, skipping Transaction/DAG dispatch, to isolate Transfer-level throughput)`)
		entitiesN      = flag.Int("entities", 20, "number of simulated entities (M)")
		transactionsN  = flag.Int("transactions", 200, "number of simulated transactions (N)")
		concurrency    = flag.Int("concurrency", 8, "concurrent in-flight transactions")
		rollbackRate   = flag.Float64("rollback-rate", 0.3, "fraction of transactions steered toward a rollback instead of completion")
		minAmount      = flag.Uint64("min-amount", 100, "minimum transfer amount, in minor units")
		maxAmount      = flag.Uint64("max-amount", 10_000, "maximum transfer amount, in minor units")
		initialBalance = flag.Uint64("initial-balance", 1_000_000, "starting balance seeded into every entity, in minor units")
		currency       = flag.String("currency", "USD", "currency for every simulated amount")
		serverURL      = flag.String("server-url", env("SERVER_URL", defaultServerURL), "base URL of a running go/cmd/server (no /twirp suffix)")
		databaseURL    = flag.String("database-url", env("DATABASE_URL", defaultDatabaseURL), "Postgres event log, for the post-run correctness check")
		seed           = flag.Uint64("seed", uint64(time.Now().UnixNano()), "RNG seed; fix it for a reproducible run")
		skipVerify     = flag.Bool("skip-verify", false, "skip the post-run ledger correctness check (Postgres is still read, to see when each leg stages)")
		timeout        = flag.Duration("timeout", 30*time.Second, "per-RPC timeout")
		waitAttempts   = flag.Int("settle-wait-attempts", 200, "how many more times to look at a still-open transaction, giving go/cmd/orchestrator time to reach it")
		waitDelay      = flag.Duration("settle-wait-delay", 50*time.Millisecond, "how long to wait before each look at a still-open transaction")
	)
	flag.Parse()

	if *entitiesN < 2 {
		log.Fatal("simulate: -entities must be at least 2")
	}
	if *maxAmount < *minAmount {
		log.Fatal("simulate: -max-amount must be >= -min-amount")
	}
	if *mode != "transaction" && *mode != "transfer" {
		log.Fatalf(`simulate: -mode must be "transaction" or "transfer", got %q`, *mode)
	}
	if *waitAttempts < 1 {
		// Nothing settles inside a call any more, so a tool that never looks
		// twice never sees anything finish.
		log.Fatal("simulate: -settle-wait-attempts must be at least 1")
	}
	wait := settleWait{attempts: *waitAttempts, delay: *waitDelay}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: *timeout}
	holders := holderpb.NewHolderServiceProtobufClient(*serverURL, httpClient)
	transactions := transactionpb.NewTransactionServiceProtobufClient(*serverURL, httpClient)
	transfers := transferpb.NewTransferServiceProtobufClient(*serverURL, httpClient)

	pool, err := openPool(ctx, *databaseURL, *concurrency)
	if err != nil {
		log.Fatalf("simulate: %v", err)
	}
	defer pool.Close()
	store := eventstore.NewPostgresStore(pool)

	log.Printf("simulate: seed=%d provisioning reserve + %d entities against %s", *seed, *entitiesN, *serverURL)
	reserve, entities, err := provisionAll(ctx, holders, *entitiesN)
	if err != nil {
		log.Fatalf("simulate: %v", err)
	}

	if err := seedAll(ctx, transactions, reserve, entities, *initialBalance, *currency, wait); err != nil {
		log.Fatalf("simulate: %v", err)
	}
	initial := make(map[string]int64, len(entities))
	for _, e := range entities {
		initial[e.walletID] = int64(*initialBalance)
	}
	log.Printf("simulate: seeded every entity with %d %s", *initialBalance, *currency)

	drive, advance := driverFor(*mode, transactions, transfers, storeLegReader(store), wait)

	cfg := runConfig{currency: *currency, minAmount: *minAmount, maxAmount: *maxAmount, rollbackRate: *rollbackRate}
	log.Printf("simulate: mode=%s running %d transactions, concurrency %d, rollback-rate %.2f", *mode, *transactionsN, *concurrency, *rollbackRate)
	results, wallClock := runLoad(ctx, entities, cfg, *transactionsN, *concurrency, *seed, drive)

	if stuck := len(stuckIndices(results)); stuck > 0 {
		log.Printf("simulate: %d transactions still open; waiting for the orchestrator, up to %d more looks, %s apart", stuck, wait.attempts, wait.delay)
		if remaining := awaitStuck(ctx, results, wait, advance); remaining > 0 {
			log.Printf("simulate: %d transactions still open after waiting", remaining)
		} else {
			log.Print("simulate: every transaction reached a terminal state")
		}
	}

	printSummary(summarize(results, wallClock))

	if *skipVerify {
		log.Print("simulate: -skip-verify set, not checking the ledger")
		return
	}

	expected := computeExpected(initial, results)
	reconciliations, err := reconcile(ctx, store, entities, expected)
	if err != nil {
		log.Fatalf("simulate: verify: %v", err)
	}
	printReconciliation(reconciliations)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// driverFor builds the drive/advance pair -mode calls for: "transaction" wraps
// every simulated Transfer in a single-child Transaction (transactionDriver);
// "transfer" drives TransferService directly (transferDriver). mode is
// assumed already validated.
func driverFor(
	mode string, transactions transactionpb.TransactionService, transfers transferpb.TransferService, leg legReader, wait settleWait,
) (driveFunc, advanceFunc) {
	if mode == "transfer" {
		d := transferDriver{transfers: transfers, leg: leg, wait: wait}
		return d.drive, d.advance
	}
	d := transactionDriver{transactions: transactions, transfers: transfers, leg: leg, wait: wait}
	return d.drive, d.advance
}

// openPool connects to the event log. Every worker reads it while waiting for
// its leg to stage, so the pool is sized to the load's concurrency rather than
// pgxpool's CPU-count default, which would queue those reads behind each
// other and show up as latency that is the tool's, not the system's.
func openPool(ctx context.Context, databaseURL string, concurrency int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	cfg.MaxConns = int32(max(concurrency, 4))

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", databaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s: %w", databaseURL, err)
	}
	return pool, nil
}

func printSummary(s summary) {
	fmt.Printf("\n--- load ---\n")
	fmt.Printf("%d transactions in %s (%.1f/sec)\n", s.total, s.wallClock.Round(time.Millisecond), s.throughput)
	fmt.Printf("latency: p50=%s p95=%s p99=%s\n", s.p50, s.p95, s.p99)

	states := make([]string, 0, len(s.byState))
	for state := range s.byState {
		states = append(states, state)
	}
	sort.Strings(states)
	for _, state := range states {
		fmt.Printf("  %-32s %d\n", state, s.byState[state])
	}
	if s.errors > 0 {
		fmt.Printf("WARNING: %d transactions never got a final state (transport/RPC error)\n", s.errors)
	}
	if s.openCount > 0 {
		fmt.Printf("NOTE: %d transactions are still open. Nothing but go/cmd/orchestrator advances a saga, so "+
			"check it is running and has a CDC connector to consume from (`make cdc-up && make orchestrator-up`, "+
			"then `make orchestrator-logs`) — with neither, every transaction ends here. Otherwise it has not "+
			"caught up, or a Transfer lost a concurrency race while preparing on a hot Wallet: try "+
			"-settle-wait-attempts/-settle-wait-delay higher, or reduce contention with more -entities relative "+
			"to -concurrency\n", s.openCount)
	}
}

func printReconciliation(reconciliations []reconciliation) {
	fmt.Printf("\n--- ledger correctness ---\n")

	var expectedTotal, actualTotal int64
	mismatches := 0
	for _, r := range reconciliations {
		expectedTotal += r.expected
		actualTotal += r.actual
		if !r.ok() {
			mismatches++
			fmt.Printf("MISMATCH %-16s expected=%d actual=%d diff=%d\n", r.name, r.expected, r.actual, r.actual-r.expected)
		}
	}

	if mismatches == 0 {
		fmt.Printf("every entity's balance matches what the run expects (%d entities)\n", len(reconciliations))
	} else {
		fmt.Printf("%d of %d entities have a balance mismatch\n", mismatches, len(reconciliations))
	}

	if expectedTotal == actualTotal {
		fmt.Printf("conserved: total entity balance is %d %s, matching the total seeded\n", actualTotal, "minor units")
	} else {
		fmt.Printf("NOT CONSERVED: total actual balance %d != total expected %d (diff %d)\n",
			actualTotal, expectedTotal, actualTotal-expectedTotal)
	}
}
