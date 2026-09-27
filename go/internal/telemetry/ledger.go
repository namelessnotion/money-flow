package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

// outcomeInvalidRequest is a batch the ledger refused as a whole
// (ledger.ErrInvalidRequest): nothing was applied and resubmitting cannot help.
const outcomeInvalidRequest = "invalid_request"

const (
	keyBatchSize    = attribute.Key("money_flow.ledger.batch_size")
	keyRejected     = attribute.Key("money_flow.ledger.rejected")
	keyLedgerResult = attribute.Key("money_flow.ledger.result")
)

// Ledger is a ledger.Client that traces and times every call to TigerBeetle.
//
// It has to be a decorator: ledger.RealClient ignores its context, because the
// TigerBeetle client takes none, so there is nowhere inside it to hang a span.
//
// A TigerBeetle result other than ok is not a failed call. The batch was
// accepted and each entry answered, and the saga decides what an answer means.
// Those answers are counted by code, so a surge of exceeds_credits is visible
// without looking like a ledger outage.
type Ledger struct {
	inner      ledger.Client
	tracer     trace.Tracer
	duration   metric.Float64Histogram
	rejections metric.Int64Counter
}

var _ ledger.Client = (*Ledger)(nil)

func NewLedger(inner ledger.Client, p Providers) *Ledger {
	b := instruments{meter: p.meter()}
	l := &Ledger{
		inner:  inner,
		tracer: p.tracer(),
		duration: b.duration("money_flow.ledger.operation.duration",
			"Time for one TigerBeetle round trip, by operation and outcome."),
		rejections: b.counter("money_flow.ledger.rejections", "{entry}",
			"Batch entries TigerBeetle answered with anything but ok, by operation and result code."),
	}
	b.mustBuild()
	return l
}

func (l *Ledger) CreateAccounts(ctx context.Context, accounts []ledger.Account) ([]ledger.AccountResult, error) {
	const op = "create_accounts"
	ctx, done := l.start(ctx, op, len(accounts))
	results, err := l.inner.CreateAccounts(ctx, accounts)
	rejected := make([]string, 0, len(results))
	for _, r := range results {
		if r.Result != ledger.AccountResultOK {
			rejected = append(rejected, r.Result.String())
		}
	}
	l.reject(ctx, op, rejected)
	done(err)
	return results, err
}

func (l *Ledger) CreateTransfers(ctx context.Context, transfers []ledger.Transfer) ([]ledger.TransferResult, error) {
	const op = "create_transfers"
	ctx, done := l.start(ctx, op, len(transfers))
	results, err := l.inner.CreateTransfers(ctx, transfers)
	rejected := make([]string, 0, len(results))
	for _, r := range results {
		if r.Result != ledger.TransferResultOK {
			rejected = append(rejected, r.Result.String())
		}
	}
	l.reject(ctx, op, rejected)
	done(err)
	return results, err
}

func (l *Ledger) Balances(ctx context.Context, accountIDs []string) (map[string]ledger.Balance, error) {
	ctx, done := l.start(ctx, "balances", len(accountIDs))
	balances, err := l.inner.Balances(ctx, accountIDs)
	done(err)
	return balances, err
}

// reject counts each non-ok entry under its code and puts the total on the
// span, where the codes of one particular batch are one click away.
func (l *Ledger) reject(ctx context.Context, op string, rejected []string) {
	trace.SpanFromContext(ctx).SetAttributes(keyRejected.Int(len(rejected)))
	for _, code := range rejected {
		l.rejections.Add(ctx, 1, metric.WithAttributes(keyOperation.String(op), keyLedgerResult.String(code)))
	}
}

func (l *Ledger) start(ctx context.Context, op string, batchSize int) (context.Context, func(error)) {
	began := time.Now()
	ctx, span := l.tracer.Start(ctx, "ledger."+op,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "tigerbeetle"), keyBatchSize.Int(batchSize)),
	)
	return ctx, func(err error) {
		outcome := ledgerOutcome(err)
		span.SetAttributes(keyOutcome.String(outcome))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
		l.duration.Record(ctx, time.Since(began).Seconds(),
			metric.WithAttributes(keyOperation.String(op), keyOutcome.String(outcome)))
	}
}

func ledgerOutcome(err error) string {
	switch {
	case err == nil:
		return outcomeOK
	case errors.Is(err, ledger.ErrInvalidRequest):
		return outcomeInvalidRequest
	default:
		return outcomeError
	}
}
