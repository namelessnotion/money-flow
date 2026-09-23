package main

import (
	"context"
	"slices"
	"testing"
	"uuid"

	"google.golang.org/protobuf/proto"

	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
	"github.com/namelessnotion/money_flow/go/internal/transaction"
	"github.com/namelessnotion/money_flow/go/internal/transfer"
)

// -open must never replay a stream that can no longer move. The shared test
// log holds every other test's streams too, so this asserts only about the
// ones it wrote.
func TestSagaStreams_SkipsStreamsThatHaveFinished(t *testing.T) {
	t.Parallel()
	pool := testutil.Pool(t)
	store := eventstore.NewPostgresStore(pool)
	ctx := context.Background()

	write := func(aggregateType string, events ...proto.Message) string {
		t.Helper()
		id := uuid.NewV7().String()
		if err := store.Append(ctx, aggregateType, id, 0, events...); err != nil {
			t.Fatalf("Append(%s) error = %v", aggregateType, err)
		}
		return id
	}

	started := write(transaction.AggregateType, &transactionpb.TransactionInitialized{}, &transactionpb.TransactionStarted{})
	completed := write(transaction.AggregateType, &transactionpb.TransactionInitialized{}, &transactionpb.TransactionStarted{}, &transactionpb.TransactionCompleted{})
	rollbackFailed := write(transaction.AggregateType, &transactionpb.TransactionInitialized{}, &transactionpb.TransactionRollbackStarted{}, &transactionpb.TransactionRollbackFailed{})
	accepted := write(transfer.AggregateType, &transferpb.TransferRequestAccepted{})
	// A rejection recorded after the terminal is stream noise, not a way back.
	committed := write(transfer.AggregateType, &transferpb.TransferRequestAccepted{}, &transferpb.TransferCommitted{}, &transferpb.ConfirmStagedTransferRejected{})
	rejected := write(transfer.AggregateType, &transferpb.TransferRequestRejected{})

	candidates, err := catalogue{pool: pool}.sagaStreams(ctx)
	if err != nil {
		t.Fatalf("sagaStreams() error = %v", err)
	}
	listed := func(aggregateType, id string) bool {
		return slices.Contains(candidates, target{aggregateType: aggregateType, aggregateID: id})
	}

	for _, want := range []target{{transaction.AggregateType, started}, {transfer.AggregateType, accepted}} {
		if !listed(want.aggregateType, want.aggregateID) {
			t.Errorf("sagaStreams() is missing %s, which is still going", want)
		}
	}
	for _, skip := range []target{
		{transaction.AggregateType, completed}, {transaction.AggregateType, rollbackFailed},
		{transfer.AggregateType, committed}, {transfer.AggregateType, rejected},
	} {
		if listed(skip.aggregateType, skip.aggregateID) {
			t.Errorf("sagaStreams() lists %s, which has finished", skip)
		}
	}
}
