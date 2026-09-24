package main

import "google.golang.org/protobuf/reflect/protoreflect"

// follower narrows the log to one Transaction and everything it set off: its
// Transfers, and the Reversals its rollback requested. They
// are separate aggregates with ids of their own, so the set of ids to show
// grows as the Transaction's events name them.
//
// Tokens and Wallets are left out: a Wallet is shared by every Transaction
// that touches it, so following one would pull in unrelated history.
type follower struct {
	ids map[string]bool
}

// newFollower follows transactionID; an empty id follows nothing, and a nil
// follower admits every event.
func newFollower(transactionID string) *follower {
	if transactionID == "" {
		return nil
	}
	return &follower{ids: map[string]bool{transactionID: true}}
}

// admit reports whether e belongs to the followed Transaction, learning the
// ids e names when it does. Events must be offered in log order.
func (f *follower) admit(e row) bool {
	if f == nil {
		return true
	}
	msg, err := e.Decode()
	if err != nil {
		return f.ids[e.AggregateID]
	}
	fields := msg.ProtoReflect()

	// An aggregate's first event says what it belongs to: a Transfer or
	// Reversal its transaction_id, or the transfer_id of the Transfer it
	// reverses.
	joins := e.Sequence == 1 && (f.ids[stringField(fields, "transaction_id")] || f.ids[stringField(fields, "transfer_id")])
	if !f.ids[e.AggregateID] && !joins {
		return false
	}

	f.ids[e.AggregateID] = true
	for _, name := range []protoreflect.Name{"transfer_id", "reversal_id"} {
		if id := stringField(fields, name); id != "" {
			f.ids[id] = true
		}
	}
	// TransactionInitialized names every Transfer up front, including any
	// that is rejected before it could record whose it is.
	if transfers := fields.Descriptor().Fields().ByName("transfers"); transfers != nil && transfers.IsMap() {
		fields.Get(transfers).Map().Range(func(key protoreflect.MapKey, _ protoreflect.Value) bool {
			f.ids[key.String()] = true
			return true
		})
	}
	return true
}

func stringField(m protoreflect.Message, name protoreflect.Name) string {
	fd := m.Descriptor().Fields().ByName(name)
	if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return ""
	}
	return m.Get(fd).String()
}
