// Serves the Twirp APIs backed by the Postgres event store and TigerBeetle.
//
//	DATABASE_URL=postgres://... DATABASE_MAX_CONNS=20 LISTEN_ADDR=:8080 \
//	TIGERBEETLE_ADDRESS=127.0.0.1:3000 TIGERBEETLE_CLUSTER_ID=0 \
//	go run ./cmd/server
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	holderpb "github.com/namelessnotion/money_flow/go/gen/proto/holder/v1"
	tokenpb "github.com/namelessnotion/money_flow/go/gen/proto/token/v1"
	transactionpb "github.com/namelessnotion/money_flow/go/gen/proto/transaction/v1"
	transferpb "github.com/namelessnotion/money_flow/go/gen/proto/transfer/v1"
	walletpb "github.com/namelessnotion/money_flow/go/gen/proto/wallet/v1"
	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/holder"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
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
	// sustain. 20 leaves headroom under Postgres's own default
	// max_connections=100 for the orchestrator, ruby, ruby-consumer and the
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	maxConns, err := strconv.ParseInt(env("DATABASE_MAX_CONNS", defaultDatabaseMaxConns), 10, 32)
	if err != nil {
		log.Fatalf("server: DATABASE_MAX_CONNS: %v", err)
	}
	cfg, err := poolConfig(env("DATABASE_URL", defaultDatabaseURL), int32(maxConns))
	if err != nil {
		log.Fatalf("server: database url: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatalf("server: pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("server: ping: %v", err)
	}

	clusterID, err := strconv.ParseUint(env("TIGERBEETLE_CLUSTER_ID", defaultTigerBeetleClusterID), 10, 64)
	if err != nil {
		log.Fatalf("server: TIGERBEETLE_CLUSTER_ID: %v", err)
	}
	addresses := strings.Split(env("TIGERBEETLE_ADDRESS", defaultTigerBeetleAddress), ",")
	tb, err := ledger.NewRealClient(clusterID, addresses)
	if err != nil {
		log.Fatalf("server: tigerbeetle: %v", err)
	}
	defer tb.Close()

	addr := env("LISTEN_ADDR", defaultListenAddr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(eventstore.NewPostgresStore(pool), pool, tb),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("server: listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("server: shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server: shutdown: %v", err)
	}
}

// pinger is the part of the pool the health check needs, so tests can supply
// something that isn't a real database.
type pinger interface {
	Ping(context.Context) error
}

// newMux wires the services onto their generated Twirp paths. Each service
// mounts at its own PathPrefix() — "/twirp/<package>.<Service>/" — which is
// what clients must post to; a client pointed at the bare host will 404.
func newMux(store eventstore.Store, health pinger, tb ledger.Client) *http.ServeMux {
	mux := http.NewServeMux()

	holderServer := holderpb.NewHolderServiceServer(holder.NewServer(store))
	walletServer := walletpb.NewWalletServiceServer(wallet.NewServer(store))
	tokenServer := tokenpb.NewTokenServiceServer(token.NewServer(store, tb))

	// saga.Wire ties transfer and transaction to each other — transfer needs
	// transaction's IsOpen/Exists checkers, transaction needs the transfer
	// server to dispatch through. It is done there rather than here so this
	// process and the orchestrator cannot drift into wiring the same two
	// services differently.
	sagaServers := saga.Wire(store, tb)
	transferServer := transferpb.NewTransferServiceServer(sagaServers.Transfer)
	transactionServer := transactionpb.NewTransactionServiceServer(sagaServers.Transaction)

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

	return mux
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
