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
// surface Ruby drives in production), and Postgres directly, read-only,
// for the post-run correctness check — the same access cmd/events already
// uses to watch the log, since no Twirp RPC exposes a Wallet's balance.
//
//	go run ./cmd/simulate -entities 50 -transactions 2000 -concurrency 16
//	go run ./cmd/simulate -mode transfer -entities 50 -transactions 2000 -concurrency 16
//
// SERVER_URL defaults to a local go/cmd/server; DATABASE_URL defaults to
// the same development database cmd/events and cmd/server use.
//
// Run go/cmd/orchestrator (with CDC publishing) alongside the server for a
// heavily concurrent run. A Transfer whose prepare step loses a
// concurrency race on a hot Wallet is left mid-flight until something
// resumes its saga — only the orchestrator's event-triggered Resume does
// that. This tool gives every such transaction -retry-stuck-attempts more
// tries, waiting -retry-stuck-delay before each so the orchestrator has a
// chance to catch up; a TRANSACTION_STATE_STARTED transaction still in the
// final report ran out of retries, not out of hope — rerunning verification
// a little later, or with a longer delay, may still resolve it. Pushing
// -entities up relative to -concurrency also reduces how often any one
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
		skipVerify     = flag.Bool("skip-verify", false, "skip the post-run ledger correctness check (no Postgres access needed)")
		timeout        = flag.Duration("timeout", 30*time.Second, "per-RPC timeout")
		retryAttempts  = flag.Int("retry-stuck-attempts", 3, "how many more times to try settling a still-open transaction, giving go/cmd/orchestrator a chance to catch up (0 disables retrying)")
		retryDelay     = flag.Duration("retry-stuck-delay", 2*time.Second, "how long to wait before each retry of a still-open transaction")
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: *timeout}
	holders := holderpb.NewHolderServiceProtobufClient(*serverURL, httpClient)
	transactions := transactionpb.NewTransactionServiceProtobufClient(*serverURL, httpClient)
	transfers := transferpb.NewTransferServiceProtobufClient(*serverURL, httpClient)

	log.Printf("simulate: seed=%d provisioning reserve + %d entities against %s", *seed, *entitiesN, *serverURL)
	reserve, entities, err := provisionAll(ctx, holders, *entitiesN)
	if err != nil {
		log.Fatalf("simulate: %v", err)
	}

	initial := make(map[string]int64, len(entities))
	for _, e := range entities {
		if err := seedEntity(ctx, transactions, reserve, e, *initialBalance, *currency); err != nil {
			log.Fatalf("simulate: %v", err)
		}
		initial[e.walletID] = int64(*initialBalance)
	}
	log.Printf("simulate: seeded every entity with %d %s", *initialBalance, *currency)

	drive, retry := driverFor(*mode, transactions, transfers)

	cfg := runConfig{currency: *currency, minAmount: *minAmount, maxAmount: *maxAmount, rollbackRate: *rollbackRate}
	log.Printf("simulate: mode=%s running %d transactions, concurrency %d, rollback-rate %.2f", *mode, *transactionsN, *concurrency, *rollbackRate)
	results, wallClock := runLoad(ctx, entities, cfg, *transactionsN, *concurrency, *seed, drive)

	if stuck := len(stuckIndices(results)); stuck > 0 && *retryAttempts > 0 {
		log.Printf("simulate: %d transactions still open; retrying up to %d times, %s apart", stuck, *retryAttempts, *retryDelay)
		if remaining := retryStuck(ctx, results, *retryAttempts, *retryDelay, retry); remaining > 0 {
			log.Printf("simulate: %d transactions still open after retrying", remaining)
		} else {
			log.Print("simulate: every transaction reached a terminal state after retrying")
		}
	}

	printSummary(summarize(results, wallClock))

	if *skipVerify {
		log.Print("simulate: -skip-verify set, not checking the ledger")
		return
	}

	pool, err := pgxpool.New(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("simulate: connect to %s: %v", *databaseURL, err)
	}
	defer pool.Close()
	store := eventstore.NewPostgresStore(pool)

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

// driverFor builds the drive/retry pair -mode calls for: "transaction" wraps
// every simulated Transfer in a single-child Transaction (driveOne/settle,
// translated through fromTransactionState); "transfer" drives
// TransferService directly (driveOneTransfer/settleTransfer). mode is
// assumed already validated.
func driverFor(mode string, transactions transactionpb.TransactionService, transfers transferpb.TransferService) (driveFunc, retryFunc) {
	if mode == "transfer" {
		drive := func(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult {
			return driveOneTransfer(ctx, transfers, from, to, amount, currency, planned)
		}
		retry := func(ctx context.Context, r *txResult) {
			r.final, r.moved, r.open, r.reason, r.err = settleTransfer(ctx, transfers, r.transferID, r.planned)
		}
		return drive, retry
	}

	drive := func(ctx context.Context, from, to entity, amount uint64, currency string, planned outcome) txResult {
		return driveOne(ctx, transactions, transfers, from, to, amount, currency, planned)
	}
	retry := func(ctx context.Context, r *txResult) {
		state, reason, err := settle(ctx, transactions, transfers, r.transactionID, r.transferID, r.planned)
		r.final, r.moved, r.open = fromTransactionState(state)
		r.reason, r.err = reason, err
	}
	return drive, retry
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
		fmt.Printf("NOTE: %d transactions are still open even after retrying — likely a Transfer that lost a "+
			"concurrency race while preparing on a hot Wallet and go/cmd/orchestrator either isn't running or "+
			"hasn't caught up yet; try -retry-stuck-attempts/-retry-stuck-delay higher, or reduce contention "+
			"with more -entities relative to -concurrency\n", s.openCount)
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
