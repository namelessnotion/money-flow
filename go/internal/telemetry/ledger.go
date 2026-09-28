package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/namelessnotion/money_flow/go/internal/ledger"
)

// outcomeInvalidRequest is a batch the ledger refused as a whole
// (ledger.ErrInvalidRequest): nothing was applied and resubmitting cannot help.
const outcomeInvalidRequest = "invalid_request"

const (
	keyBatchSize      = attribute.Key("money_flow.ledger.batch_size")
	keyRejected       = attribute.Key("money_flow.ledger.rejected")
	keyAlreadyApplied = attribute.Key("money_flow.ledger.already_applied")
	keyLedgerResult   = attribute.Key("money_flow.ledger.result")
)

// Ledger is a ledger.Client that traces and times every call to TigerBeetle.
//
// It has to be a decorator: ledger.RealClient ignores its context, because the
// TigerBeetle client takes none, so there is nowhere inside it to hang a span.
//
// A TigerBeetle result other than ok is not a failed call. The batch was
// accepted and each entry answered, and the saga decides what an answer means.
// Refusals are counted by code, so a surge of exceeds_credits is visible
// without looking like a ledger outage; "exists", the answer to a retried
// entry, is not a refusal and is not counted as one.
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
	var answers batchAnswers
	for _, r := range results {
		switch r.Result {
		case ledger.AccountResultOK:
		case ledger.AccountResultExists:
			answers.alreadyApplied++
		default:
			answers.rejected = append(answers.rejected, r.Result.String())
		}
	}
	l.record(ctx, op, answers)
	done(err)
	return results, err
}

func (l *Ledger) CreateTransfers(ctx context.Context, transfers []ledger.Transfer) ([]ledger.TransferResult, error) {
	const op = "create_transfers"
	ctx, done := l.start(ctx, op, len(transfers))
	results, err := l.inner.CreateTransfers(ctx, transfers)
	var answers batchAnswers
	for _, r := range results {
		switch r.Result {
		case ledger.TransferResultOK:
		case ledger.TransferResultExists:
			answers.alreadyApplied++
		default:
			answers.rejected = append(answers.rejected, r.Result.String())
		}
	}
	l.record(ctx, op, answers)
	done(err)
	return results, err
}

func (l *Ledger) Balances(ctx context.Context, accountIDs []string) (map[string]ledger.Balance, error) {
	ctx, done := l.start(ctx, "balances", len(accountIDs))
	balances, err := l.inner.Balances(ctx, accountIDs)
	done(err)
	return balances, err
}

// batchAnswers is how TigerBeetle answered the entries of one batch, other
// than plainly applying them.
type batchAnswers struct {
	// rejected holds the result code of each entry refused on its merits.
	rejected []string
	// alreadyApplied counts entries answered "exists": an identical entry had
	// already landed. The saga treats that as success, because it is how an
	// at-least-once retry finds its work done, so it is not a rejection; a
	// redelivery after a restart would otherwise read as a surge of refusals.
	alreadyApplied int
}

// record counts each rejected entry under its code, and puts both totals on
// the span, where one particular batch's answers are one click away.
func (l *Ledger) record(ctx context.Context, op string, a batchAnswers) {
	trace.SpanFromContext(ctx).SetAttributes(keyRejected.Int(len(a.rejected)), keyAlreadyApplied.Int(a.alreadyApplied))
	for _, code := range a.rejected {
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
		finish(span, outcome, err)
		span.End()
		l.duration.Record(ctx, time.Since(began).Seconds(),
			metric.WithAttributes(keyOperation.String(op), keyOutcome.String(outcome)))
	}
}

func ledgerOutcome(err error) string {
	if errors.Is(err, ledger.ErrInvalidRequest) {
		return outcomeInvalidRequest
	}
	return baseOutcome(err)
}
