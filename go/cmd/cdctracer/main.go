// Command cdctracer appends real domain events to the event store so the CDC
// tracer bullet has something to publish. It writes through
// eventstore.PostgresStore — the same path the sagas use — rather than
// hand-writing rows, so the pipeline sees exactly what the application
// produces.
//
// Throwaway. It exists to produce the evidence in docs/cdc-tracer-bullet.md
// and should be deleted along with the rest of the rig.
//
//	DATABASE_URL=postgres://money_flow:money_flow@postgres:5432/money_flow_cdc?sslmode=disable \
//	  go run ./cmd/cdctracer
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// concurrentAppends is the size of the racing burst at the end of the run.
// global_seq is GENERATED ALWAYS AS IDENTITY, so it is allocated at INSERT and
// only visible at COMMIT; several writers overlapping is what exposes the gap
// a polling relay would skip on (docs/adr/0001, "Why not a polling relay").
const concurrentAppends = 8

func main() {
	ctx := context.Background()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("cdctracer: DATABASE_URL is not set")
	}
	if err := run(ctx, dbURL); err != nil {
		log.Fatalf("cdctracer: %v", err)
	}
}

func run(ctx context.Context, dbURL string) error {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	store := eventstore.NewPostgresStore(pool)

	// Two events on one Transfer stream, appended as two separate commits, so
	// their arrival order on transfer-events is a real ordering observation
	// rather than an artefact of a single batch insert.
	transferID := uuid.NewV7().String()
	if err := store.Append(ctx, transfer.AggregateType, transferID, 0,
		&transferpb.TransferRequestAccepted{Id: transferID}); err != nil {
		return fmt.Errorf("append transfer seq 1: %w", err)
	}
	if err := store.Append(ctx, transfer.AggregateType, transferID, 1,
		&transferpb.PreparingTransferStarted{Id: transferID}); err != nil {
		return fmt.Errorf("append transfer seq 2: %w", err)
	}
	fmt.Printf("transfer      %s  (sequence 1, 2)\n", transferID)

	// A second Transfer, so the two ids can be seen landing on different
	// partitions of the same topic — decision 4 is about partitioning, and one
	// aggregate cannot show that.
	otherTransferID := uuid.NewV7().String()
	if err := store.Append(ctx, transfer.AggregateType, otherTransferID, 0,
		&transferpb.TransferRequestAccepted{Id: otherTransferID}); err != nil {
		return fmt.Errorf("append second transfer: %w", err)
	}
	fmt.Printf("transfer      %s  (sequence 1)\n", otherTransferID)

	// The same table, a different aggregate_type: this is what has to end up
	// on transaction-events instead.
	transactionID := uuid.NewV7().String()
	if err := store.Append(ctx, transaction.AggregateType, transactionID, 0,
		&transactionpb.TransactionInitialized{Id: transactionID}); err != nil {
		return fmt.Errorf("append transaction seq 1: %w", err)
	}
	if err := store.Append(ctx, transaction.AggregateType, transactionID, 1,
		&transactionpb.TransactionStarted{Id: transactionID}); err != nil {
		return fmt.Errorf("append transaction seq 2: %w", err)
	}
	fmt.Printf("transaction   %s  (sequence 1, 2)\n", transactionID)

	return appendConcurrently(ctx, store)
}

// appendConcurrently races several single-event appends so their identity
// allocation and their commits interleave.
func appendConcurrently(ctx context.Context, store *eventstore.PostgresStore) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		start = make(chan struct{})
	)
	for range concurrentAppends {
		id := uuid.NewV7().String()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := store.Append(ctx, transfer.AggregateType, id, 0,
				&transferpb.TransferRequestAccepted{Id: id}); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		return fmt.Errorf("concurrent appends: %w", errs[0])
	}
	fmt.Printf("transfer      %d concurrent single-event appends\n", concurrentAppends)
	return nil
}
