package auth

import (
	"fmt"
	"os"
	"strings"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
)

// VerifierFromConfig builds a verifier from application configuration,
// reading the key from a file when one is configured.
//
// It returns (nil, nil) when no verification key is configured at all. That is
// only reachable outside production — configuration validation rejects it for
// APP_ENV=production — and the caller must treat a nil verifier as "refuse
// every authenticated RPC", never as "allow everything".
func VerifierFromConfig(cfg config.Config) (*Verifier, error) {
	pem := cfg.JWT.PublicKeyPEM

	if cfg.JWT.PublicKeyFile != "" {
		raw, err := os.ReadFile(cfg.JWT.PublicKeyFile)
		if err != nil {
			// The path is safe to name; the contents are not, and are not read
			// into the error.
			return nil, fmt.Errorf("auth: reading JWT_PUBLIC_KEY_FILE %s: %w", cfg.JWT.PublicKeyFile, err)
		}
		pem = string(raw)
	}

	if strings.TrimSpace(pem) == "" && cfg.JWT.Issuer == "" && cfg.JWT.Audience == "" {
		return nil, nil
	}

	return NewVerifier(VerifierConfig{
		PublicKeyPEM: pem,
		Issuer:       cfg.JWT.Issuer,
		Audience:     cfg.JWT.Audience,
		Leeway:       cfg.JWT.Leeway,
	})
}
