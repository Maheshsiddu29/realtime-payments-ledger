package auth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// bearerPrefix is the only credential scheme accepted.
const bearerPrefix = "bearer "

// Verifier validates JWT access tokens against a configured public key,
// issuer and audience.
//
// Signing is asymmetric (RS256): this service holds only the PUBLIC key and
// can therefore verify tokens but never mint one. A symmetric scheme such as
// HS256 would require the verification secret to be present here, which means
// any compromise of the payments service becomes the ability to forge tokens
// for the whole estate. Verification-only is the property worth having.
type Verifier struct {
	parser   *jwt.Parser
	key      *rsa.PublicKey
	issuer   string
	audience string
}

// VerifierConfig is what a Verifier needs. Every field is required.
type VerifierConfig struct {
	// PublicKeyPEM is a PEM-encoded RSA public key. Its contents are never
	// logged.
	PublicKeyPEM string
	Issuer       string
	Audience     string
	// Leeway tolerates small clock differences between this service and the
	// issuer when checking exp and nbf. Keep it small; it is a window in which
	// an expired token still works.
	Leeway time.Duration
}

// NewVerifier builds a verifier, failing loudly on anything missing or
// malformed. Authentication that silently degrades is worse than none.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if strings.TrimSpace(cfg.PublicKeyPEM) == "" {
		return nil, errors.New("auth: no JWT verification key configured")
	}
	if cfg.Issuer == "" {
		return nil, errors.New("auth: no JWT issuer configured")
	}
	if cfg.Audience == "" {
		return nil, errors.New("auth: no JWT audience configured")
	}

	key, err := parseRSAPublicKey(cfg.PublicKeyPEM)
	if err != nil {
		return nil, err
	}

	// Every validation below is enabled explicitly rather than relying on
	// library defaults, so a library upgrade cannot quietly relax them.
	parser := jwt.NewParser(
		// The single most important line here. Without an explicit algorithm
		// allow-list, a token can nominate its own algorithm in the header —
		// including "none", or HS256 signed with the RSA public key as the
		// HMAC secret, which is public. Restricting to RS256 makes both
		// attacks impossible.
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(cfg.Leeway),
	)

	return &Verifier{
		parser:   parser,
		key:      key,
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
	}, nil
}

// parseRSAPublicKey reads a PEM-encoded RSA public key.
func parseRSAPublicKey(pemData string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		// Deliberately does not echo the input: it could be a misconfigured
		// private key, and that must never reach a log.
		return nil, errors.New("auth: JWT verification key is not valid PEM")
	}

	if strings.Contains(block.Type, "PRIVATE") {
		return nil, errors.New("auth: JWT verification key is a private key; configure the public key")
	}

	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		// Fall back to the older PKCS#1 "RSA PUBLIC KEY" encoding.
		if pkcs1, pkcs1Err := x509.ParsePKCS1PublicKey(block.Bytes); pkcs1Err == nil {
			return pkcs1, nil
		}
		return nil, fmt.Errorf("auth: parsing JWT verification key: %w", err)
	}

	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("auth: JWT verification key is %T, want an RSA public key", parsed)
	}
	return key, nil
}

// registeredClaims carries the standard claims plus the OAuth2 scope claim.
//
// Both spellings are accepted: "scope" is the space-delimited string from
// RFC 8693 and most OAuth2 servers, "scp" is the array form Microsoft Entra
// and some others emit.
type registeredClaims struct {
	jwt.RegisteredClaims
	Scope  string   `json:"scope,omitempty"`
	Scopes []string `json:"scp,omitempty"`
}

// Verify parses and validates a raw token, returning the authenticated caller.
//
// Every failure returns ErrInvalidToken wrapping the underlying reason. The
// wrapped reason is for logs; the transport layer must not pass it to the
// client, because "expired" versus "wrong audience" versus "bad signature"
// tells an attacker which knob to turn next.
func (v *Verifier) Verify(raw string) (Principal, error) {
	if raw == "" {
		return Principal{}, ErrNoCredentials
	}

	var claims registeredClaims
	token, err := v.parser.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
		// The key is chosen by configuration, never by the token. Returning a
		// key based on a "kid" the token supplies would be the same class of
		// mistake as trusting its "alg".
		return v.key, nil
	})
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if !token.Valid {
		return Principal{}, fmt.Errorf("%w: token reported invalid", ErrInvalidToken)
	}

	// Trimmed, so a whitespace-only subject cannot pass as an identity: every
	// action has to be attributable to someone in the audit trail.
	subject := strings.TrimSpace(claims.Subject)
	if subject == "" {
		return Principal{}, fmt.Errorf("%w: token has no subject", ErrInvalidToken)
	}

	scopes := parseScopes(claims.Scope)
	for _, s := range claims.Scopes {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, Scope(s))
		}
	}

	return Principal{
		Subject:  subject,
		Issuer:   v.issuer,
		Audience: v.audience,
		Scopes:   scopes,
	}, nil
}

// TokenFromAuthorization extracts a bearer token from an Authorization header
// value.
//
// The scheme match is case-insensitive because RFC 7235 says it is, but
// nothing else is forgiven: no other scheme, no missing token, no extra
// fields.
func TokenFromAuthorization(header string) (string, error) {
	if strings.TrimSpace(header) == "" {
		return "", ErrNoCredentials
	}
	if len(header) < len(bearerPrefix) || !strings.EqualFold(header[:len(bearerPrefix)], bearerPrefix) {
		return "", fmt.Errorf("%w: expected a Bearer token", ErrMalformedCredentials)
	}

	token := strings.TrimSpace(header[len(bearerPrefix):])
	if token == "" {
		return "", fmt.Errorf("%w: Bearer scheme with no token", ErrMalformedCredentials)
	}
	if strings.ContainsAny(token, " \t") {
		return "", fmt.Errorf("%w: token contains whitespace", ErrMalformedCredentials)
	}
	return token, nil
}
