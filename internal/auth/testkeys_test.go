package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Test key material is GENERATED AT RUN TIME, never committed. Nothing in this
// repository is a private key, so there is nothing to leak and nothing that
// could be mistaken for production material.

const (
	testIssuer   = "https://issuer.test"
	testAudience = "payments-api"
)

type testKeypair struct {
	private     *rsa.PrivateKey
	publicPEM   string
	otherSigner *rsa.PrivateKey // a different key, for signature-mismatch tests
}

func newTestKeypair(t *testing.T) testKeypair {
	t.Helper()

	// 2048 is the smallest size worth using; larger keys make tests slow for
	// no security value in a test.
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating second test key: %v", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		t.Fatalf("marshalling public key: %v", err)
	}
	publicPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	return testKeypair{private: private, publicPEM: publicPEM, otherSigner: other}
}

func (k testKeypair) verifier(t *testing.T) *Verifier {
	t.Helper()

	v, err := NewVerifier(VerifierConfig{
		PublicKeyPEM: k.publicPEM,
		Issuer:       testIssuer,
		Audience:     testAudience,
		Leeway:       0,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// claimSet describes a token to mint.
type claimSet struct {
	subject   string
	issuer    string
	audience  string
	scope     string
	issuedAt  time.Time
	notBefore *time.Time
	expiresAt *time.Time
	omitExp   bool
}

func (c claimSet) withDefaults() claimSet {
	now := time.Now()
	if c.subject == "" {
		c.subject = "test-subject"
	}
	if c.issuer == "" {
		c.issuer = testIssuer
	}
	if c.audience == "" {
		c.audience = testAudience
	}
	if c.issuedAt.IsZero() {
		c.issuedAt = now
	}
	if c.expiresAt == nil && !c.omitExp {
		exp := now.Add(time.Hour)
		c.expiresAt = &exp
	}
	return c
}

func (c claimSet) build() jwt.MapClaims {
	c = c.withDefaults()

	claims := jwt.MapClaims{
		"sub": c.subject,
		"iss": c.issuer,
		"aud": c.audience,
		"iat": c.issuedAt.Unix(),
	}
	if c.scope != "" {
		claims["scope"] = c.scope
	}
	if c.expiresAt != nil {
		claims["exp"] = c.expiresAt.Unix()
	}
	if c.notBefore != nil {
		claims["nbf"] = c.notBefore.Unix()
	}
	return claims
}

// sign mints a token with the given method and key.
func sign(t *testing.T, method jwt.SigningMethod, key any, c claimSet) string {
	t.Helper()

	raw, err := jwt.NewWithClaims(method, c.build()).SignedString(key)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	return raw
}

// signRS256 mints a correctly signed token.
func (k testKeypair) signRS256(t *testing.T, c claimSet) string {
	t.Helper()
	return sign(t, jwt.SigningMethodRS256, k.private, c)
}
