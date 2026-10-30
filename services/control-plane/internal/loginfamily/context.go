package loginfamily

import "context"

type accessIdentityKey struct{}

// WithAccessIdentity carries the middleware-verified signed access identity on
// the request context. It is an input claim, never authority: every consumer
// must revalidate it under its own transaction's user and family locks.
func WithAccessIdentity(ctx context.Context, identity AccessIdentity) context.Context {
	return context.WithValue(ctx, accessIdentityKey{}, identity)
}

// AccessIdentityFromContext returns the identity WithAccessIdentity attached.
func AccessIdentityFromContext(ctx context.Context) (AccessIdentity, bool) {
	identity, ok := ctx.Value(accessIdentityKey{}).(AccessIdentity)
	return identity, ok
}
