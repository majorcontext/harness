package turn

import "context"

type sessionIDKey struct{}

// WithSessionID returns a context that carries the session that owns the call.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, id)
}

// SessionID returns the session ID carried by ctx, or an empty string.
func SessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)
	return id
}
