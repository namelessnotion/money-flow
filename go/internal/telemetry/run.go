package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/namelessnotion/money_flow/go/internal/eventstore"
	"github.com/namelessnotion/money_flow/go/internal/ledger"
	"github.com/namelessnotion/money_flow/go/internal/saga"
)

// flushTimeout bounds exporting what is still buffered when a binary stops.
const flushTimeout = 10 * time.Second

// Run is a binary's main: it configures telemetry for service from the
// environment and runs run until it returns or the process is asked to stop
// (SIGINT, SIGTERM), then flushes. It returns run's error, already logged, so
// main only has to exit non-zero on it.
//
// A configuration it cannot start with is written to stderr, since there is
// no logger yet to write it with.
func Run(service string, run func(ctx context.Context, t *Telemetry) error) error {
	cfg, err := ConfigFromEnv(service, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunWith(ctx, cfg, run)
}

// RunWith is Run with its configuration and context given.
//
// Whether run failed is logged through the process logger before the flush,
// so the line saying why a binary stopped is exported with everything else.
// The flush comes last, after run has closed whatever it opened, so nothing
// recorded on the way down is lost.
func RunWith(ctx context.Context, cfg Config, run func(ctx context.Context, t *Telemetry) error) error {
	t, err := Setup(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", cfg.ServiceName, err)
		return err
	}

	runErr := run(ctx, t)
	if runErr != nil {
		t.Logger.Error("stopped", slog.Any("err", runErr))
	} else {
		t.Logger.Info("stopped")
	}

	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()
	if err := t.Shutdown(flushCtx); err != nil {
		fmt.Fprintf(os.Stderr, "%s: flushing telemetry: %v\n", cfg.ServiceName, err)
	}
	return runErr
}

// Orchestrator is saga.Wire's orchestrator over an instrumented store and
// ledger: the one wiring every binary that drives sagas shares, so what a saga
// step does lands in its trace however it was woken.
func Orchestrator(p Providers, store eventstore.Store, lc ledger.Client) *saga.Orchestrator {
	return saga.Wire(NewStore(store, p), NewLedger(lc, p)).Orchestrator()
}
