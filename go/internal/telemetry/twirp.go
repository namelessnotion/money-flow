package telemetry

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/twitchtv/twirp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const keyTwirpErrorCode = attribute.Key("twirp.error_code")

// healthPath is excluded from tracing: it is polled by infrastructure every
// few seconds and a trace of it answers no question anyone asks.
const healthPath = "/healthz"

// Handler wraps the whole HTTP surface: it continues a caller's W3C trace
// context, opens the server span every Twirp call runs inside, and records
// the standard HTTP server metrics.
//
// The span is named for the route the mux matched, one per Twirp service,
// never for the raw path: a request to a garbage path must not become a span
// name of its own. Interceptor opens the per-method span beneath it.
func Handler(p Providers, h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http",
		otelhttp.WithTracerProvider(p.Tracer),
		otelhttp.WithMeterProvider(p.Meter),
		otelhttp.WithPropagators(p.Propagator),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern == "" {
				return r.Method
			}
			return r.Method + " " + r.Pattern
		}),
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != healthPath }),
	)
}

// Interceptor opens a span for the Twirp method a call carries, beneath the
// HTTP span Handler opened, tags it with the aggregate the command addressed,
// and times the call. It is an internal span: the HTTP span is the request's
// one server span, which is what backends count requests by.
//
// It records a twirp.Error's code on every answer but marks the span as failed
// only for a server fault (a 5xx code). Aborted, NotFound, FailedPrecondition
// and InvalidArgument are the API answering as designed, and a trace full of
// red for a lost optimistic-concurrency race would hide the real faults.
func Interceptor(p Providers) twirp.Interceptor {
	b := instruments{meter: p.meter()}
	duration := b.duration("rpc.server.call.duration", "Time to answer one Twirp call, by method and Twirp error code.")
	b.mustBuild()
	tracer := p.tracer()

	return func(next twirp.Method) twirp.Method {
		return func(ctx context.Context, req any) (any, error) {
			began := time.Now()
			pkg, _ := twirp.PackageName(ctx)
			service, _ := twirp.ServiceName(ctx)
			method, _ := twirp.MethodName(ctx)
			fullService := pkg + "." + service

			ctx, span := tracer.Start(ctx, fullService+"/"+method,
				trace.WithSpanKind(trace.SpanKindInternal),
				trace.WithAttributes(
					attribute.String("rpc.system", "twirp"),
					attribute.String("rpc.service", fullService),
					attribute.String("rpc.method", method),
				),
			)
			defer span.End()
			// Every command in the API names its aggregate as Id; tlatrace
			// relies on the same convention.
			if r, ok := req.(interface{ GetId() string }); ok && r.GetId() != "" {
				span.SetAttributes(keyAggregateID.String(r.GetId()))
			}

			resp, err := next(ctx, req)

			code := twirpCode(err)
			span.SetAttributes(keyTwirpErrorCode.String(code))
			if err != nil && twirp.ServerHTTPStatusFromErrorCode(twirp.ErrorCode(code)) >= http.StatusInternalServerError {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			duration.Record(ctx, time.Since(began).Seconds(), metric.WithAttributes(
				attribute.String("rpc.service", fullService),
				attribute.String("rpc.method", method),
				keyTwirpErrorCode.String(code),
			))
			return resp, err
		}
	}
}

// twirpCode is the code the caller will see: "ok", the code of a twirp.Error,
// or internal for any other error, which is what Twirp turns it into.
func twirpCode(err error) string {
	if err == nil {
		return outcomeOK
	}
	var terr twirp.Error
	if errors.As(err, &terr) {
		return string(terr.Code())
	}
	return string(twirp.Internal)
}
