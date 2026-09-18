package token

import (
	"context"
	"errors"
	"fmt"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	pb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

// maxRecordAttempts bounds how many optimistic-concurrency races one Token's
// balance recording may lose before giving up with an error. The caller (a
// saga step) is retried as a whole, so giving up is safe.
const maxRecordAttempts = 5

// tokenStream is one Token's stream as RecordBalances needs it: where to
// append next, which Wallet the Token belongs to, and the last balance
// already published.
type tokenStream struct {
	seq      int64
	walletID string
	last     *pb.TokenBalanceRecorded
}

// RecordBalances publishes a TokenBalanceRecorded for each Token in
// tokenIDs, read from its TigerBeetle account. Call it after every ledger
// write, passing every Token the write touched.
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
	ids := distinct(tokenIDs)
	streams := make(map[string]tokenStream, len(ids))
	for _, id := range ids {
		s, err := loadTokenStream(ctx, store, id)
		if err != nil {
			return err
		}
		streams[id] = s
	}
	balances, err := lc.Balances(ctx, ids)
	if err != nil {
		return twirp.InternalErrorWith(fmt.Errorf("ledger: Balances: %w", err))
	}

	for _, id := range ids {
		if err := recordBalance(ctx, store, lc, id, streams[id], balances); err != nil {
			return err
		}
	}
	return nil
}

// recordBalance appends one Token's observation, starting from a stream and
// ledger read already made, and re-reading both on each lost race.
func recordBalance(
	ctx context.Context, store eventstore.Store, lc ledger.Client,
	tokenID string, stream tokenStream, balances map[string]ledger.Balance,
) error {
	for attempt := 1; ; attempt++ {
		event, err := balanceRecorded(tokenID, stream.walletID, balances)
		if err != nil {
			return err
		}
		if proto.Equal(event, stream.last) {
			return nil
		}
		err = store.Append(ctx, AggregateType, tokenID, stream.seq, event)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, eventstore.ErrConcurrencyConflict):
			return twirp.InternalErrorWith(err)
		case attempt == maxRecordAttempts:
			return twirp.InternalErrorWith(fmt.Errorf("token %q: recording balance: %w", tokenID, err))
		}

		if stream, err = loadTokenStream(ctx, store, tokenID); err != nil {
			return err
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
