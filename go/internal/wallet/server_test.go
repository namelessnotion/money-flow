package wallet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"

	sharedpb "github.com/namelessnotion/money_flow/go/gen/proto/shared/v1"
	pb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/testutil"
)

func openRequest() *pb.OpenRequest {
	return &pb.OpenRequest{
		Id:       testutil.ID("w1"),
		HolderId: testutil.ID("h1"),
		Name:     "bank",
		Allows:   sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP,
	}
}

func TestOpenRecordsWalletOpened(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	server := NewServer(store)
	ctx := context.Background()

	resp, err := server.Open(ctx, openRequest())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	opened := resp.GetWalletOpened()
	if opened == nil {
		t.Fatal("response result = nil, want WalletOpened")
	}
	if opened.GetId() != testutil.ID("w1") || opened.GetHolderId() != testutil.ID("h1") {
		t.Errorf("WalletOpened = (%q, %q), want (%s, %s)",
			opened.GetId(), opened.GetHolderId(), testutil.ID("w1"), testutil.ID("h1"))
	}
	if opened.GetAllows() != sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP {
		t.Errorf("WalletOpened.Allows = %v, want ONRAMP_AND_OFFRAMP", opened.GetAllows())
	}

	// The event must actually be in the log, not merely echoed back.
	events, err := store.Load(ctx, AggregateType, testutil.ID("w1"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("log holds %d events, want 1", len(events))
	}
	if got, want := events[0].EventType, "wallet.v1.WalletOpened"; got != want {
		t.Errorf("recorded event type = %q, want %q", got, want)
	}

	decoded, err := events[0].Decode()
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !proto.Equal(opened, decoded) {
		t.Errorf("recorded event = %v, want %v", decoded, opened)
	}
}

func TestOpenValidatesRequest(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		req  *pb.OpenRequest
	}{
		{"missing id", &pb.OpenRequest{HolderId: testutil.ID("h1"), Allows: sharedpb.Allows_ALLOWS_NONE}},
		{"missing holder_id", &pb.OpenRequest{Id: testutil.ID("w1"), Allows: sharedpb.Allows_ALLOWS_NONE}},
		{"malformed id", &pb.OpenRequest{
			Id: "not-a-uuid", HolderId: testutil.ID("h1"), Allows: sharedpb.Allows_ALLOWS_NONE,
		}},
		{"malformed holder_id", &pb.OpenRequest{
			Id: testutil.ID("w1"), HolderId: "not-a-uuid", Allows: sharedpb.Allows_ALLOWS_NONE,
		}},
		// Unset allows is a bug, not a default — ALLOWS_NONE exists for
		// deliberately permitting neither direction.
		{"unspecified allows", &pb.OpenRequest{Id: testutil.ID("w1"), HolderId: testutil.ID("h1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := NewServer(eventstore.NewMemoryStore())

			_, err := server.Open(context.Background(), tc.req)
			if err == nil {
				t.Fatal("Open() error = nil, want a rejection")
			}
			var twerr twirp.Error
			if !errors.As(err, &twerr) {
				t.Fatalf("Open() error = %T, want twirp.Error", err)
			}
			if twerr.Code() != twirp.InvalidArgument {
				t.Errorf("error code = %q, want %q", twerr.Code(), twirp.InvalidArgument)
			}
		})
	}
}

// ValidateWallet is the piece of ValidateOpen that AddWallet and Provision
// reuse once they've already checked the holder id themselves — it must not
// also require one.
func TestValidateWalletChecksOnlyWalletIDAndAllows(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		walletID string
		allows   sharedpb.Allows
		wantErr  bool
	}{
		{"valid", testutil.ID("w1"), sharedpb.Allows_ALLOWS_NONE, false},
		{"missing id", "", sharedpb.Allows_ALLOWS_NONE, true},
		{"malformed id", "not-a-uuid", sharedpb.Allows_ALLOWS_NONE, true},
		{"unspecified allows", testutil.ID("w1"), sharedpb.Allows_ALLOWS_UNSPECIFIED, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateWallet(tc.walletID, tc.allows)
			if tc.wantErr && err == nil {
				t.Fatal("ValidateWallet() error = nil, want a rejection")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateWallet() error = %v, want nil", err)
			}
		})
	}
}

// Opening is idempotent so a retry after an ambiguous failure converges on the
// one Wallet rather than opening a second.
func TestOpenIsIdempotent(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	server := NewServer(store)
	ctx := context.Background()

	first, err := server.Open(ctx, openRequest())
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	second, err := server.Open(ctx, openRequest())
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	if !proto.Equal(first, second) {
		t.Errorf("second Open() = %v, want identical to first %v", second, first)
	}

	events, _ := store.Load(ctx, AggregateType, testutil.ID("w1"))
	if len(events) != 1 {
		t.Fatalf("log holds %d events, want 1 — reopening must not append again", len(events))
	}
}

// The recorded event is immutable: a replay carrying different attributes gets
// the original back rather than rewriting history.
func TestOpenIgnoresChangedAttributesOnReplay(t *testing.T) {
	t.Parallel()

	server := NewServer(eventstore.NewMemoryStore())
	ctx := context.Background()

	if _, err := server.Open(ctx, openRequest()); err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	changed := openRequest()
	changed.Name = "changed"
	changed.Allows = sharedpb.Allows_ALLOWS_NONE

	resp, err := server.Open(ctx, changed)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	if got := resp.GetWalletOpened().GetName(); got != "bank" {
		t.Errorf("name = %q, want %q — the recorded event is immutable", got, "bank")
	}
	if got := resp.GetWalletOpened().GetAllows(); got != sharedpb.Allows_ALLOWS_ONRAMP_AND_OFFRAMP {
		t.Errorf("allows = %v, want the originally recorded ONRAMP_AND_OFFRAMP", got)
	}
}

// Idempotency must not hand a caller someone else's Wallet.
func TestOpenRejectsReplayUnderADifferentHolder(t *testing.T) {
	t.Parallel()

	server := NewServer(eventstore.NewMemoryStore())
	ctx := context.Background()

	if _, err := server.Open(ctx, openRequest()); err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	poached := openRequest()
	poached.HolderId = testutil.ID("h2")

	resp, err := server.Open(ctx, poached)
	if err != nil {
		t.Fatalf("Open() error = %v, want a domain rejection rather than an error", err)
	}
	rejected := resp.GetWalletRejected()
	if rejected == nil {
		t.Fatal("response result = nil, want WalletRejected")
	}
	if rejected.GetHolderId() != testutil.ID("h2") {
		t.Errorf("WalletRejected.HolderId = %q, want the requesting holder %s", rejected.GetHolderId(), testutil.ID("h2"))
	}
}

func TestOpenResolvesConcurrencyConflict(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	ctx := context.Background()

	winner := OpenedEvent(testutil.ID("w1"), testutil.ID("h1"), "winner", sharedpb.Allows_ALLOWS_ONRAMP)
	server := NewServer(&conflictOnceStore{Store: store, onConflict: func() {
		if err := store.Append(ctx, AggregateType, testutil.ID("w1"), 0, winner); err != nil {
			t.Errorf("seeding winner: %v", err)
		}
	}})

	resp, err := server.Open(ctx, openRequest())
	if err != nil {
		t.Fatalf("Open() error = %v, want the winner's wallet", err)
	}
	if got := resp.GetWalletOpened().GetName(); got != "winner" {
		t.Errorf("name = %q, want %q", got, "winner")
	}
}

func TestOpenRejectsStreamNotStartingWithWalletOpened(t *testing.T) {
	t.Parallel()

	store := eventstore.NewMemoryStore()
	ctx := context.Background()

	// A WalletRejected is a real wallet.v1 message, so this exercises the
	// wrong-type branch rather than a decode failure.
	if err := store.Append(ctx, AggregateType, testutil.ID("w1"), 0, &pb.WalletRejected{Id: testutil.ID("w1")}); err != nil {
		t.Fatalf("seeding stream: %v", err)
	}

	_, err := NewServer(store).Open(ctx, openRequest())
	if err == nil {
		t.Fatal("Open() error = nil, want a refusal for a malformed stream")
	}
	var twerr twirp.Error
	if !errors.As(err, &twerr) || twerr.Code() != twirp.Internal {
		t.Errorf("Open() error = %v, want twirp internal", err)
	}
}

func TestOpenSurfacesStoreFailure(t *testing.T) {
	t.Parallel()

	server := NewServer(&failingStore{err: errors.New("database is on fire")})

	_, err := server.Open(context.Background(), openRequest())
	if err == nil {
		t.Fatal("Open() error = nil, want the store failure surfaced")
	}
	var twerr twirp.Error
	if !errors.As(err, &twerr) || twerr.Code() != twirp.Internal {
		t.Errorf("Open() error = %v, want twirp internal", err)
	}
}

type conflictOnceStore struct {
	eventstore.Store
	onConflict func()
	conflicted bool
}

func (s *conflictOnceStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedSeq int64, events ...proto.Message) error {
	return s.AppendAtomic(ctx, eventstore.StreamWrite{
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		ExpectedSeq:   expectedSeq,
		Events:        events,
	})
}

func (s *conflictOnceStore) AppendAtomic(ctx context.Context, writes ...eventstore.StreamWrite) error {
	if !s.conflicted {
		s.conflicted = true
		s.onConflict()
		return eventstore.ErrConcurrencyConflict
	}
	return s.Store.AppendAtomic(ctx, writes...)
}

type failingStore struct{ err error }

func (s *failingStore) Append(context.Context, string, string, int64, ...proto.Message) error {
	return s.err
}

func (s *failingStore) AppendAtomic(context.Context, ...eventstore.StreamWrite) error {
	return s.err
}

func (s *failingStore) Load(context.Context, string, string) ([]eventstore.Event, error) {
	return nil, s.err
}

func (s *failingStore) Now(context.Context) (time.Time, error) {
	return time.Time{}, s.err
}
