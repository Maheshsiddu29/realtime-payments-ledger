// Command devtoken mints JWT access tokens for LOCAL DEVELOPMENT AND TESTING.
//
// IT IS NOT AN AUTHORIZATION SERVER. It has no users, no login, no consent, no
// refresh tokens and no revocation. It signs a token with a key it generated
// moments earlier so a developer can call the API with grpcurl. In production
// tokens come from a real OAuth2/OIDC identity provider and this tool has no
// part in the system at all.
//
// It is never built into the service image and never deployed.
//
// Typical use:
//
//	# generate a keypair and a token in one step
//	go run ./cmd/devtoken -out ./.devkeys -scopes 'accounts:write accounts:read transfers:write transfers:read ledger:read'
//
//	# mint another token from the same key
//	go run ./cmd/devtoken -key ./.devkeys/private.pem -scopes 'accounts:read'
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	var (
		issuer   = flag.String("issuer", "https://dev-issuer.local", "value for the iss claim")
		audience = flag.String("audience", "payments-api", "value for the aud claim")
		subject  = flag.String("subject", "dev-client", "value for the sub claim")
		scopes   = flag.String("scopes", "accounts:read accounts:write transfers:read transfers:write ledger:read",
			"space-delimited scopes for the scope claim")
		ttl     = flag.Duration("ttl", time.Hour, "how long the token is valid")
		keyPath = flag.String("key", "", "existing PEM private key to sign with (default: generate one)")
		outDir  = flag.String("out", "", "directory to write a generated keypair into")
		quiet   = flag.Bool("quiet", false, "print only the token, for scripting")
	)
	flag.Parse()

	if err := run(*issuer, *audience, *subject, *scopes, *ttl, *keyPath, *outDir, *quiet); err != nil {
		fmt.Fprintf(os.Stderr, "devtoken: %v\n", err)
		os.Exit(1)
	}
}

func run(issuer, audience, subject, scopes string, ttl time.Duration, keyPath, outDir string, quiet bool) error {
	key, generated, err := loadOrGenerateKey(keyPath)
	if err != nil {
		return err
	}

	publicPEM, err := encodePublicKey(&key.PublicKey)
	if err != nil {
		return err
	}

	if generated && outDir != "" {
		if err := writeKeypair(outDir, key, publicPEM); err != nil {
			return err
		}
	}

	token, err := mint(key, issuer, audience, subject, scopes, ttl)
	if err != nil {
		return err
	}

	if quiet {
		fmt.Println(token)
		return nil
	}

	fmt.Println("# DEVELOPMENT TOKEN — not production authentication infrastructure.")
	fmt.Println("#")
	fmt.Printf("# issuer   %s\n", issuer)
	fmt.Printf("# audience %s\n", audience)
	fmt.Printf("# subject  %s\n", subject)
	fmt.Printf("# scopes   %s\n", scopes)
	fmt.Printf("# expires  %s\n", time.Now().Add(ttl).UTC().Format(time.RFC3339))
	fmt.Println()
	fmt.Println("# Configure the service with the matching public key:")
	fmt.Printf("export JWT_ISSUER=%q\n", issuer)
	fmt.Printf("export JWT_AUDIENCE=%q\n", audience)
	fmt.Printf("export JWT_PUBLIC_KEY=%q\n", publicPEM)
	fmt.Println()
	fmt.Println("# Then call the API:")
	fmt.Printf("export TOKEN=%q\n", token)
	fmt.Println(`grpcurl -plaintext -H "authorization: Bearer $TOKEN" \`)
	fmt.Println(`  -d '{"currency":"USD"}' \`)
	fmt.Println("  localhost:9090 payments.v1.PaymentsService/CreateAccount")

	if generated && outDir != "" {
		fmt.Printf("\n# keypair written to %s (development only, do not commit)\n", outDir)
	}
	return nil
}

func loadOrGenerateKey(path string) (*rsa.PrivateKey, bool, error) {
	if path == "" {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, false, fmt.Errorf("generating key: %w", err)
		}
		return key, true, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, false, fmt.Errorf("%s is not valid PEM", path)
	}

	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, false, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, false, errors.New("key is not an RSA private key")
	}
	return key, false, nil
}

func encodePublicKey(key *rsa.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", fmt.Errorf("encoding public key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

func writeKeypair(dir string, key *rsa.PrivateKey, publicPEM string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	privatePEM := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	// 0600: it is a development key, but a private key on disk is still a
	// private key.
	if err := os.WriteFile(filepath.Join(dir, "private.pem"), privatePEM, 0o600); err != nil {
		return fmt.Errorf("writing private key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public.pem"), []byte(publicPEM), 0o644); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}
	return nil
}

func mint(key *rsa.PrivateKey, issuer, audience, subject, scopes string, ttl time.Duration) (string, error) {
	now := time.Now()

	claims := jwt.MapClaims{
		"iss":   issuer,
		"aud":   audience,
		"sub":   subject,
		"scope": scopes,
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(ttl).Unix(),
	}

	// RS256 to match what the service verifies. There is no option to sign
	// with "none" or a symmetric key: this tool cannot produce a token the
	// service would be wrong to accept.
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("signing token: %w", err)
	}
	return token, nil
}
