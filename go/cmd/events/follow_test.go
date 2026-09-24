package main

import (
	"testing"

	"google.golang.org/protobuf/proto"

	txpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	trpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
)

func streamEvent(t *testing.T, aggregateType, aggregateID string, sequence int64, msg proto.Message) row {
	t.Helper()
	e := eventRow(t, 0, msg)
	e.AggregateType, e.AggregateID, e.Sequence = aggregateType, aggregateID, sequence
	return e
}

// One ACH withdrawal whose ACH entry was returned: its Transaction, both legs
// (one rejected, carrying no transaction id of its own), and the Reversal the
// rollback asked for — interleaved with another Transaction's Transfer and a
// Wallet shared by everything.
func TestFollowerTakesInATransactionsTransfersAndReversals(t *testing.T) {
	const (
		tx, real, shadow, reversal = "tx", "real", "shadow", "reversal"
	)
	log := []struct {
		e    row
		want bool
	}{
		{streamEvent(t, "transaction", tx, 1, &txpb.TransactionInitialized{Id: tx, Transfers: map[string]*txpb.Transfer{real: {}, shadow: {}}}), true},
		{streamEvent(t, "transfer", "other", 1, &trpb.TransferRequestAccepted{Id: "other", TransactionId: "other-tx"}), false},
		{streamEvent(t, "transfer", shadow, 1, &trpb.TransferRequestRejected{Id: shadow, Reason: "short"}), true},
		{streamEvent(t, "transfer", real, 1, &trpb.TransferRequestAccepted{Id: real, TransactionId: tx}), true},
		{streamEvent(t, "transfer", real, 2, &trpb.TransferPrepared{Id: real}), true},
		{streamEvent(t, "transfer", "other", 2, &trpb.TransferPrepared{Id: "other"}), false},
		{streamEvent(t, "transaction", tx, 5, &txpb.TransferReversalRequestedWithinTransaction{Id: tx, TransferId: real, ReversalId: reversal}), true},
		{streamEvent(t, "transfer", reversal, 1, &trpb.ReversalRequestAccepted{Id: reversal, TransferId: real, TransactionId: tx}), true},
		{streamEvent(t, "transfer", reversal, 3, &trpb.TransferStaged{Id: reversal}), true},
	}

	f := newFollower(tx)
	for i, entry := range log {
		if got := f.admit(entry.e); got != entry.want {
			t.Errorf("event %d (%s %s #%d %s): admit = %v, want %v",
				i, entry.e.AggregateType, entry.e.AggregateID, entry.e.Sequence, entry.e.EventType, got, entry.want)
		}
	}
}

func TestFollowerWithNoIdAdmitsEverything(t *testing.T) {
	var f *follower

	if !f.admit(streamEvent(t, "wallet", "w", 3, &txpb.TransactionStarted{})) {
		t.Fatal("a nil follower should admit every event")
	}
}
