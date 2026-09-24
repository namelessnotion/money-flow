package token

import (
	"context"
	"errors"
	"fmt"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	"github.com/namelessnotion/money_flow/go/internal/contention"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

// tokenStream is one Token's stream as RecordBalances needs it: where to
// append next, which Wallet the Token belongs to, and the last balance
// already published.
type tokenStream struct {
	seq      int64
	walletID string
	last     *pb.TokenBalanceRecorded
}

// RecordBalances publishes a TokenBalanceRecorded for each Token in
// tokenIDs, read from its TigerBeetle account, one append per Token. A saga
// step records the balances its ledger write moved together with its own
// outcome instead (BalanceWrites); this is for a ledger write no outcome
// follows, such as a refused chain after earlier chains applied.
//
// Each Token's stream is loaded BEFORE its account is looked up, and the
// append expects the loaded length. A caller that loses the race loads and
// looks up again, so the last TokenBalanceRecorded on a stream was read
// after every earlier append on it — and each earlier append after its own
// ledger write — meaning it reflects every write that preceded it. That
// makes the stream sequence a safe ordering key for the read side.
//
// A balance equal to the last one recorded is not recorded again, so a
// retried saga step converges without adding events.
func RecordBalances(ctx context.Context, store eventstore.Store, lc ledger.Client, tokenIDs []string) error {
	ids, streams, balances, err := observe(ctx, store, lc, tokenIDs)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := recordBalance(ctx, store, lc, id, streams[id], balances); err != nil {
			return err
		}
	}
	return nil
}

// BalanceWrites builds the TokenBalanceRecorded write for each Token in
// tokenIDs whose balance differs from the last one recorded, for a caller to
// append atomically with the outcome of the ledger write that moved them
// (go/docs/adr/0010). It is RecordBalances without the appends: each Token's
// stream is loaded before its account is read, and each write expects the
// loaded length, so a write that lands was read after every earlier balance
// on its stream — the same ordering RecordBalances guarantees. A caller whose
// atomic append loses must build again, never retry these writes as they are.
//
// A Token whose balance already matches its last recording gets no write, so
// a retried step converges without adding events.
func BalanceWrites(ctx context.Context, store eventstore.Store, lc ledger.Client, tokenIDs []string) ([]eventstore.StreamWrite, error) {
	ids, streams, balances, err := observe(ctx, store, lc, tokenIDs)
	if err != nil {
		return nil, err
	}
	writes := make([]eventstore.StreamWrite, 0, len(ids))
	for _, id := range ids {
		event, err := balanceRecorded(id, streams[id].walletID, balances)
		if err != nil {
			return nil, err
		}
		if proto.Equal(event, streams[id].last) {
			continue
		}
		writes = append(writes, eventstore.StreamWrite{
			AggregateType: AggregateType, AggregateID: id, ExpectedSeq: streams[id].seq,
			Events: []proto.Message{event},
		})
	}
	return writes, nil
}

// observe loads each distinct Token's stream, then reads every account's
// balance from the ledger — in that order, which is what makes the stream
// sequence a safe ordering key (see RecordBalances).
func observe(ctx context.Context, store eventstore.Store, lc ledger.Client, tokenIDs []string) ([]string, map[string]tokenStream, map[string]ledger.Balance, error) {
	ids := distinct(tokenIDs)
	streams := make(map[string]tokenStream, len(ids))
	for _, id := range ids {
		s, err := loadTokenStream(ctx, store, id)
		if err != nil {
			return nil, nil, nil, err
		}
		streams[id] = s
	}
	balances, err := lc.Balances(ctx, ids)
	if err != nil {
		return nil, nil, nil, twirp.InternalErrorWith(fmt.Errorf("ledger: Balances: %w", err))
	}
	return ids, streams, balances, nil
}

// recordBalance appends one Token's observation, starting from a stream and
// ledger read already made, and re-reading both on each lost race.
//
// A lost race is contention, not a fault: it means another recording landed
// on the Token. So recordBalance keeps trying for as long as it keeps losing,
// and a hot Token — a source every partition's Transfers debit at once —
// drains rather than halting the orchestrator. No fixed number of attempts is
// enough: while its ledger account keeps moving, a re-read rarely matches
// what just landed, so the last of k recorders contending for one Token loses
// k-1 times, and k grows with the orchestrator's partition count.
//
// Two things end the loop other than landing: ctx, and the check that a lost
// race was lost *to* something. A conflict with nothing landed on the stream
// is a fault, and goes back to the saga step to retry and halt over.
func recordBalance(
	ctx context.Context, store eventstore.Store, lc ledger.Client,
	tokenID string, stream tokenStream, balances map[string]ledger.Balance,
) error {
	for attempt := 0; ; attempt++ {
		event, err := balanceRecorded(tokenID, stream.walletID, balances)
		if err != nil {
			return err
		}
		if proto.Equal(event, stream.last) {
			return nil
		}
		conflict := store.Append(ctx, AggregateType, tokenID, stream.seq, event)
		switch {
		case conflict == nil:
			return nil
		case !errors.Is(conflict, eventstore.ErrConcurrencyConflict):
			return twirp.InternalErrorWith(conflict)
		}

		if err := contention.Wait(ctx, attempt); err != nil {
			return err
		}
		lost := stream.seq
		if stream, err = loadTokenStream(ctx, store, tokenID); err != nil {
			return err
		}
		if stream.seq == lost {
			return twirp.InternalErrorWith(fmt.Errorf(
				"token %q: recording balance: %w, yet its stream has not moved", tokenID, conflict))
		}
		if balances, err = lc.Balances(ctx, []string{tokenID}); err != nil {
			return twirp.InternalErrorWith(fmt.Errorf("ledger: Balances: %w", err))
		}
	}
}

func balanceRecorded(tokenID, walletID string, balances map[string]ledger.Balance) (*pb.TokenBalanceRecorded, error) {
	b, ok := balances[tokenID]
	if !ok {
		return nil, twirp.InternalError(fmt.Sprintf("token %q has no TigerBeetle account", tokenID))
	}
	posted, err := b.PostedNet()
	if err != nil {
		return nil, twirp.InternalErrorWith(fmt.Errorf("token %q: %w", tokenID, err))
	}
	return &pb.TokenBalanceRecorded{
		Id: tokenID, WalletId: walletID, Currency: b.Currency,
		PostedMinorUnits:          posted,
		PendingOutgoingMinorUnits: b.DebitsPending,
		PendingIncomingMinorUnits: b.CreditsPending,
	}, nil
}

func loadTokenStream(ctx context.Context, store eventstore.Store, tokenID string) (tokenStream, error) {
	events, err := store.Load(ctx, AggregateType, tokenID)
	if err != nil {
		return tokenStream{}, twirp.InternalErrorWith(err)
	}
	if len(events) == 0 {
		return tokenStream{}, twirp.InternalError(fmt.Sprintf("token %q has not been minted", tokenID))
	}

	s := tokenStream{seq: int64(len(events))}
	for _, e := range events {
		msg, err := e.Decode()
		if err != nil {
			return tokenStream{}, twirp.InternalErrorWith(err)
		}
		switch m := msg.(type) {
		case *pb.TokenMinted:
			s.walletID = m.GetWalletId()
		case *pb.TokenBalanceRecorded:
			s.last = m
		}
	}
	if s.walletID == "" {
		return tokenStream{}, twirp.InternalError(fmt.Sprintf(
			"token %q: stream starts with %s, want TokenMinted", tokenID, events[0].EventType,
		))
	}
	return s, nil
}

// distinct drops repeats, keeping first-seen order: a Transfer's legs can
// name the same Token more than once, and one Token must only be appended
// to once per call.
func distinct(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
