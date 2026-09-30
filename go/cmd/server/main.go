// Serves the Twirp APIs backed by the Postgres event store and TigerBeetle.
//
//	DATABASE_URL=postgres://... DATABASE_MAX_CONNS=20 LISTEN_ADDR=:8080 \
//	TIGERBEETLE_ADDRESS=127.0.0.1:3000 TIGERBEETLE_CLUSTER_ID=0 \
//	go run ./cmd/server
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twitchtv/twirp"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/holder"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
	"github.com/namelessnotion/money_flow/go/internal/telemetry"
	"github.com/namelessnotion/money_flow/go/internal/token"
	"github.com/namelessnotion/money_flow/go/internal/wallet"
)

const (
	defaultDatabaseURL = "postgres://money_flow:money_flow@localhost:5432/money_flow_dev?sslmode=disable"
	// defaultDatabaseMaxConns is deliberately an explicit, environment-
	// independent number rather than pgxpool's own default (max(4,
	// runtime.NumCPU())): that default ties this server's throughput ceiling
	// to whatever core count the host or container happens to expose, not to
	// anything chosen for this workload — see go/cmd/simulate's -mode=transfer
	// vs -mode=transaction throughput comparison, which found it capping
	// RPC-driven saga throughput well below what Postgres could otherwise
	// sustain. 20 leaves headroom under docker-compose.yml's
	// max_connections=200 for the orchestrator, ruby, ruby-consumer and the
	// resque workers to share the same instance; raise it per environment via
	// DATABASE_MAX_CONNS rather than editing this default.
	defaultDatabaseMaxConns     = "20"
	defaultListenAddr           = ":8080"
	defaultTigerBeetleAddress   = "127.0.0.1:3000"
	defaultTigerBeetleClusterID = "0"
	shutdownGrace               = 10 * time.Second
)

// poolConfig parses databaseURL into a pgxpool.Config with MaxConns
// overridden to maxConns. Kept as a separate override from databaseURL
// itself — never a "?pool_max_conns=" query parameter appended to
// DATABASE_URL — because docker/go/entrypoint.sh runs cmd/migrate against
// the same DATABASE_URL first, over plain pgx rather than pgxpool; plain pgx
// does not recognize that parameter and forwards it straight to Postgres as
// a runtime GUC, which Postgres then rejects outright.
func poolConfig(databaseURL string, maxConns int32) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	config.MaxConns = maxConns
	return config, nil
}

func main() {
	if err := telemetry.Run("money-flow-server", serve); err != nil {
		os.Exit(1)
	}
}

// serve runs the API until ctx is cancelled, returning an error only if it
// could not start or stopped for any other reason.
func serve(ctx context.Context, tel *telemetry.Telemetry) error {
	maxConns, err := strconv.ParseInt(env("DATABASE_MAX_CONNS", defaultDatabaseMaxConns), 10, 32)
	if err != nil {
		return fmt.Errorf("DATABASE_MAX_CONNS: %w", err)
	}
	cfg, err := poolConfig(env("DATABASE_URL", defaultDatabaseURL), int32(maxConns))
	if err != nil {
		return fmt.Errorf("database url: %w", err)
	}
	telemetry.InstrumentPool(cfg, tel.Providers)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if err := telemetry.RecordPoolStats(pool, tel.Providers); err != nil {
		return fmt.Errorf("pool stats: %w", err)
	}

	clusterID, err := strconv.ParseUint(env("TIGERBEETLE_CLUSTER_ID", defaultTigerBeetleClusterID), 10, 64)
	if err != nil {
		return fmt.Errorf("TIGERBEETLE_CLUSTER_ID: %w", err)
	}
	addresses := strings.Split(env("TIGERBEETLE_ADDRESS", defaultTigerBeetleAddress), ",")
	tb, err := ledger.NewRealClient(clusterID, addresses)
	if err != nil {
		return fmt.Errorf("tigerbeetle: %w", err)
	}
	defer tb.Close()

	addr := env("LISTEN_ADDR", defaultListenAddr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(eventstore.NewPostgresStore(pool), pool, tb, tel.Providers),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(tel.Logger.Handler(), slog.LevelWarn),
	}

	listening := make(chan error, 1)
	go func() {
		tel.Logger.Info("server: listening", slog.String("addr", addr))
		listening <- srv.ListenAndServe()
	}()

	select {
	case err := <-listening:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	tel.Logger.Info("server: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// pinger is the part of the pool the health check needs, so tests can supply
// something that isn't a real database.
type pinger interface {
	Ping(context.Context) error
}

// newMux wires the services onto their generated Twirp paths. Each service
// mounts at its own PathPrefix() — "/twirp/<package>.<Service>/" — which is
// what clients must post to; a client pointed at the bare host will 404.
//
// Telemetry goes on at the ports only: the store and ledger are wrapped
// before any service sees them, every Twirp server gets the same interceptor,
// and the whole mux sits behind the HTTP handler that continues a caller's
// trace (go/docs/adr/0017).
func newMux(store eventstore.Store, health pinger, tb ledger.Client, p telemetry.Providers) http.Handler {
	mux := http.NewServeMux()
	store = telemetry.NewStore(store, p)
	tb = telemetry.NewLedger(tb, p)
	traced := twirp.WithServerInterceptors(telemetry.Interceptor(p))

	holderServer := holderpb.NewHolderServiceServer(holder.NewServer(store), traced)
	walletServer := walletpb.NewWalletServiceServer(wallet.NewServer(store), traced)
	tokenServer := tokenpb.NewTokenServiceServer(token.NewServer(store, tb), traced)

	// saga.Wire ties transfer and transaction to each other — transfer needs
	// transaction's IsOpen/Exists checkers, transaction needs the transfer
	// server to dispatch through. It is done there rather than here so this
	// process and the orchestrator cannot drift into wiring the same two
	// services differently.
	sagaServers := saga.Wire(store, tb)
	transferServer := transferpb.NewTransferServiceServer(sagaServers.Transfer, traced)
	transactionServer := transactionpb.NewTransactionServiceServer(sagaServers.Transaction, traced)

	mux.Handle(holderServer.PathPrefix(), holderServer)
	mux.Handle(walletServer.PathPrefix(), walletServer)
	mux.Handle(tokenServer.PathPrefix(), tokenServer)
	mux.Handle(transactionServer.PathPrefix(), transactionServer)
	mux.Handle(transferServer.PathPrefix(), transferServer)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := health.Ping(r.Context()); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	return telemetry.Handler(p, mux)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
