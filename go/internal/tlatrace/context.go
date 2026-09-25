package tlatrace

import "context"

type requestKey struct{}

// WithRequest tags ctx with the traced Request whose store calls it carries.
func WithRequest(ctx context.Context, req string) context.Context {
	return context.WithValue(ctx, requestKey{}, req)
}

// RequestFrom returns the traced Request ctx carries, if any.
func RequestFrom(ctx context.Context) (string, bool) {
	req, ok := ctx.Value(requestKey{}).(string)
	return req, ok
}
