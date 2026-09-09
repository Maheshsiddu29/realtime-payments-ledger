// Package auth verifies JWT access tokens and models the authenticated caller.
//
// This service is a RESOURCE SERVER, not an authorization server. It validates
// access tokens; it does not issue them, it has no login flow, no user
// database and no password storage. In production the tokens are expected to
// come from an external OAuth2/OIDC identity provider. See
// docs/AUTHENTICATION.md.
package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// Scope is a permission carried by an access token, in the style OAuth2 uses.
type Scope string

// The scopes this service understands. They are deliberately coarse: they
// describe what an operation does, not which account it touches. Per-account
// ownership is not modelled — see docs/AUTHENTICATION.md.
const (
	ScopeAccountsRead   Scope = "accounts:read"
	ScopeAccountsWrite  Scope = "accounts:write"
	ScopeTransfersRead  Scope = "transfers:read"
	ScopeTransfersWrite Scope = "transfers:write"
	ScopeLedgerRead     Scope = "ledger:read"
)

// AllScopes is every scope the service recognises, for tooling and docs.
var AllScopes = []Scope{
	ScopeAccountsRead, ScopeAccountsWrite,
	ScopeTransfersRead, ScopeTransfersWrite,
	ScopeLedgerRead,
}

// Errors returned by verification. They are sentinels so the transport layer
// can map them to status codes without inspecting strings, and so that no
// library error text ever reaches a client.
var (
	// ErrNoCredentials means the request carried no bearer token.
	ErrNoCredentials = errors.New("auth: no credentials supplied")
	// ErrMalformedCredentials means the authorization header was not a
	// well-formed bearer token.
	ErrMalformedCredentials = errors.New("auth: malformed credentials")
	// ErrInvalidToken covers every reason a token failed verification:
	// signature, expiry, not-before, issuer, audience, algorithm. The reason is
	// logged, never returned — telling an attacker which check failed is free
	// help.
	ErrInvalidToken = errors.New("auth: token is not valid")
	// ErrMissingScope means the caller authenticated but lacks the permission
	// the RPC requires.
	ErrMissingScope = errors.New("auth: token does not carry the required scope")
)

// Principal is the authenticated caller.
//
// It is a deliberate abstraction over the token: no jwt.Token, no claims map
// and no raw token string travels beyond this package. Handlers see only this.
type Principal struct {
	// Subject is the "sub" claim: who the token was issued for.
	Subject string
	// Issuer is the "iss" claim, already validated against configuration.
	Issuer string
	// Audience is the "aud" value this service matched on.
	Audience string
	// Scopes are the permissions the token carries.
	Scopes []Scope
}

// HasScope reports whether the principal carries a scope.
func (p Principal) HasScope(s Scope) bool { return slices.Contains(p.Scopes, s) }

// ScopeStrings renders the scopes for logging.
func (p Principal) ScopeStrings() []string {
	out := make([]string, 0, len(p.Scopes))
	for _, s := range p.Scopes {
		out = append(out, string(s))
	}
	return out
}

// contextKey is unexported so nothing outside this package can plant a
// principal in a context and impersonate a caller.
type contextKey struct{}

// WithPrincipal returns a context carrying the authenticated caller.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// PrincipalFrom returns the authenticated caller, if the request was
// authenticated.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p, ok
}

// parseScopes splits an OAuth2 "scope" claim, which is a space-delimited
// string, into individual scopes. Empty entries are dropped.
func parseScopes(raw string) []Scope {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	scopes := make([]Scope, 0, len(fields))
	for _, f := range fields {
		scopes = append(scopes, Scope(f))
	}
	return scopes
}
