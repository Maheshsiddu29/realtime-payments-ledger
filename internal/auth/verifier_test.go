package auth

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestVerifyAcceptsAValidToken(t *testing.T) {
	t.Parallel()

	keys := newTestKeypair(t)
	v := keys.verifier(t)

	raw := keys.signRS256(t, claimSet{
		subject: "client-42",
		scope:   "accounts:read transfers:write",
	})

	principal, err := v.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if principal.Subject != "client-42" {
		t.Errorf("Subject = %q, want %q", principal.Subject, "client-42")
	}
	if principal.Issuer != testIssuer {
		t.Errorf("Issuer = %q, want %q", principal.Issuer, testIssuer)
	}
	if principal.Audience != testAudience {
		t.Errorf("Audience = %q, want %q", principal.Audience, testAudience)
	}
	if !principal.HasScope(ScopeAccountsRead) || !principal.HasScope(ScopeTransfersWrite) {
		t.Errorf("Scopes = %v, want accounts:read and transfers:write", principal.Scopes)
	}
	if principal.HasScope(ScopeLedgerRead) {
		t.Errorf("Scopes = %v, want ledger:read absent", principal.Scopes)
	}
}

// The security cases. Each is a way an attacker gets in if the check is
// missing, so each is stated as its own scenario rather than folded together.
func TestVerifyRejectsBadTokens(t *testing.T) {
	t.Parallel()

	keys := newTestKeypair(t)
	v := keys.verifier(t)
	now := time.Now()

	expired := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	past := now.Add(-2 * time.Hour)

	tests := []struct {
		name  string
		token func(t *testing.T) string
		why   string
	}{
		{
			name: "unsigned token, alg none",
			token: func(t *testing.T) string {
				// jwt.UnsafeAllowNoneSignatureType is the library's explicit
				// opt-in for producing such a token; accepting one would let
				// anybody mint any identity.
				return sign(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, claimSet{})
			},
			why: "an unsigned token grants any identity to anyone",
		},
		{
			name: "signed with a different RSA key",
			token: func(t *testing.T) string {
				return sign(t, jwt.SigningMethodRS256, keys.otherSigner, claimSet{})
			},
			why: "a signature from an unknown key must not verify",
		},
		{
			name: "HS256 signed with the public key as the secret",
			token: func(t *testing.T) string {
				// The classic algorithm-confusion attack: the RSA public key is
				// public, so if HS256 were permitted anyone could use it as the
				// HMAC secret and forge tokens at will.
				return sign(t, jwt.SigningMethodHS256, []byte(keys.publicPEM), claimSet{})
			},
			why: "algorithm confusion turns a public key into a signing secret",
		},
		{
			name: "expired",
			token: func(t *testing.T) string {
				return keys.signRS256(t, claimSet{expiresAt: &expired})
			},
			why: "an expired token must not be honoured",
		},
		{
			name: "no expiry at all",
			token: func(t *testing.T) string {
				return keys.signRS256(t, claimSet{omitExp: true})
			},
			why: "a token without exp never stops working",
		},
		{
			name: "not yet valid",
			token: func(t *testing.T) string {
				return keys.signRS256(t, claimSet{notBefore: &future})
			},
			why: "nbf in the future means the token is not usable yet",
		},
		{
			name: "wrong issuer",
			token: func(t *testing.T) string {
				return keys.signRS256(t, claimSet{issuer: "https://attacker.example"})
			},
			why: "a token from another issuer must not be accepted",
		},
		{
			name: "wrong audience",
			token: func(t *testing.T) string {
				return keys.signRS256(t, claimSet{audience: "some-other-service"})
			},
			why: "a token minted for another service must not work here",
		},
		{
			name: "no subject",
			token: func(t *testing.T) string {
				return keys.signRS256(t, claimSet{subject: " "})
			},
			why: "an anonymous principal cannot be attributed in an audit trail",
		},
		{
			name:  "not a token at all",
			token: func(*testing.T) string { return "definitely-not-a-jwt" },
			why:   "garbage must be rejected, not parsed leniently",
		},
		{
			name: "tampered payload",
			token: func(t *testing.T) string {
				raw := keys.signRS256(t, claimSet{scope: "accounts:read"})
				parts := strings.Split(raw, ".")
				if len(parts) != 3 {
					t.Fatalf("unexpected token shape %q", raw)
				}
				// Flip a character in the payload; the signature no longer matches.
				payload := []byte(parts[1])
				payload[0] ^= 'A' ^ 'B'
				return parts[0] + "." + string(payload) + "." + parts[2]
			},
			why: "an edited payload invalidates the signature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := v.Verify(tt.token(t))
			if err == nil {
				t.Fatalf("Verify accepted a token that should be rejected: %s", tt.why)
			}
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error %v does not wrap ErrInvalidToken", err)
			}
		})
	}

	// A valid nbf in the past must still be accepted, or the check above would
	// be passing for the wrong reason.
	t.Run("nbf in the past is accepted", func(t *testing.T) {
		t.Parallel()

		if _, err := v.Verify(keys.signRS256(t, claimSet{notBefore: &past})); err != nil {
			t.Errorf("Verify with a past nbf = %v, want it accepted", err)
		}
	})
}

func TestVerifyRejectsAnEmptyToken(t *testing.T) {
	t.Parallel()

	if _, err := newTestKeypair(t).verifier(t).Verify(""); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("Verify(\"\") = %v, want ErrNoCredentials", err)
	}
}

// The scp array form is what some identity providers emit instead of the
// space-delimited scope string.
func TestVerifyReadsScopesFromEitherClaim(t *testing.T) {
	t.Parallel()

	keys := newTestKeypair(t)
	v := keys.verifier(t)

	claims := claimSet{scope: "accounts:read"}.build()
	claims["scp"] = []string{"transfers:write", "ledger:read"}

	raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(keys.private)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	principal, err := v.Verify(raw)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	for _, want := range []Scope{ScopeAccountsRead, ScopeTransfersWrite, ScopeLedgerRead} {
		if !principal.HasScope(want) {
			t.Errorf("scope %q missing from %v", want, principal.Scopes)
		}
	}
}

func TestNewVerifierRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	keys := newTestKeypair(t)

	tests := []struct {
		name string
		cfg  VerifierConfig
	}{
		{"no key", VerifierConfig{Issuer: testIssuer, Audience: testAudience}},
		{"no issuer", VerifierConfig{PublicKeyPEM: keys.publicPEM, Audience: testAudience}},
		{"no audience", VerifierConfig{PublicKeyPEM: keys.publicPEM, Issuer: testIssuer}},
		{"key is not PEM", VerifierConfig{PublicKeyPEM: "nonsense", Issuer: testIssuer, Audience: testAudience}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := NewVerifier(tt.cfg); err == nil {
				t.Error("NewVerifier accepted an incomplete configuration")
			}
		})
	}
}

// Configuring a private key where the public key belongs is a serious
// mistake, and the error must not echo the key back into a log.
func TestNewVerifierRejectsAPrivateKeyWithoutEchoingIt(t *testing.T) {
	t.Parallel()

	keys := newTestKeypair(t)
	privatePEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(keys.private),
	}))

	_, err := NewVerifier(VerifierConfig{
		PublicKeyPEM: privatePEM, Issuer: testIssuer, Audience: testAudience,
	})
	if err == nil {
		t.Fatal("NewVerifier accepted a private key")
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), privatePEM) {
		t.Errorf("error echoes key material: %v", err)
	}
}

func TestTokenFromAuthorization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		header  string
		want    string
		wantErr error
	}{
		{"bearer", "Bearer abc.def.ghi", "abc.def.ghi", nil},
		{"lower case scheme", "bearer abc.def.ghi", "abc.def.ghi", nil},
		{"upper case scheme", "BEARER abc.def.ghi", "abc.def.ghi", nil},
		{"extra spacing after scheme", "Bearer    abc.def.ghi", "abc.def.ghi", nil},
		{"empty", "", "", ErrNoCredentials},
		{"whitespace only", "   ", "", ErrNoCredentials},
		{"no scheme", "abc.def.ghi", "", ErrMalformedCredentials},
		{"wrong scheme", "Basic dXNlcjpwYXNz", "", ErrMalformedCredentials},
		{"scheme with no token", "Bearer ", "", ErrMalformedCredentials},
		{"token with embedded space", "Bearer abc def", "", ErrMalformedCredentials},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := TokenFromAuthorization(tt.header)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TokenFromAuthorization(%q) = %v", tt.header, err)
			}
			if got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
		})
	}
}

// A principal must only be placeable through this package, so a handler
// cannot be tricked into treating an unauthenticated request as authenticated.
func TestPrincipalContext(t *testing.T) {
	t.Parallel()

	if _, ok := PrincipalFrom(t.Context()); ok {
		t.Error("PrincipalFrom returned a principal for a bare context")
	}

	want := Principal{Subject: "s", Scopes: []Scope{ScopeLedgerRead}}
	got, ok := PrincipalFrom(WithPrincipal(t.Context(), want))
	if !ok {
		t.Fatal("PrincipalFrom did not return the stored principal")
	}
	if got.Subject != want.Subject || !got.HasScope(ScopeLedgerRead) {
		t.Errorf("principal = %+v, want %+v", got, want)
	}
}
