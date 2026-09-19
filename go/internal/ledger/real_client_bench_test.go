package ledger_test

import (
	"context"
	"testing"
	"uuid"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

// BenchmarkRealClient_PendingThenPost isolates TigerBeetle's own throughput
// ceiling from the Postgres/HTTP overhead go/cmd/simulate measures end to
// end: it drives the same two-phase lifecycle transfer.Server's own
// stage+commit does (see internal/transfer/saga.go's submitBatch) — a
// pending CreateTransfers call followed by its post-pending counterpart —
// against one fixed, deliberately hot account pair, with no Postgres event
// store or HTTP server in the loop at all.
//
// Run with -cpu to sweep concurrency the same way go/cmd/simulate's own
// -concurrency flag does, e.g. against the dockerized stack:
//
//	docker compose run --rm --no-deps -T \
//	  -e TIGERBEETLE_ADDRESS=tigerbeetle:3000 go \
//	  go test ./internal/ledger/... -run '^$' \
//	  -bench BenchmarkRealClient_PendingThenPost -benchtime 2s -cpu 8,32,64
//
// ns/op converts to a comparable throughput figure the same way
// go/cmd/simulate's own "N/sec" does: 1e9 / ns_per_op.
func BenchmarkRealClient_PendingThenPost(b *testing.B) {
	c := testRealClient(b)
	ctx := context.Background()

	debit := ledger.Account{ID: uuid.NewV7().String(), Currency: "USD"}
	credit := ledger.Account{ID: uuid.NewV7().String(), Currency: "USD"}
	if _, err := c.CreateAccounts(ctx, []ledger.Account{debit, credit}); err != nil {
		b.Fatalf("CreateAccounts: %v", err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			pendingID := uuid.NewV7().String()
			pending := ledger.Transfer{
				ID: pendingID, DebitAccountID: debit.ID, CreditAccountID: credit.ID,
				MinorUnits: 100, Currency: "USD", Kind: ledger.TransferKindPending, Timeout: 3600,
			}
			if _, err := c.CreateTransfers(ctx, []ledger.Transfer{pending}); err != nil {
				b.Fatalf("CreateTransfers(pending): %v", err)
			}

			post := ledger.Transfer{
				ID: uuid.NewV7().String(), DebitAccountID: debit.ID, CreditAccountID: credit.ID,
				MinorUnits: 100, Currency: "USD", Kind: ledger.TransferKindPostPending, PendingID: pendingID,
			}
			if _, err := c.CreateTransfers(ctx, []ledger.Transfer{post}); err != nil {
				b.Fatalf("CreateTransfers(post): %v", err)
			}
		}
	})
}
