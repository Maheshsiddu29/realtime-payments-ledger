package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// Request is the financial identity of a transfer request: the parts that
// decide where money moves and how much.
//
// This type exists here, rather than the transfer command being reused, so
// that idempotency does not import transfer. The dependency runs one way —
// the transfer service orchestrates idempotency — and a shared type in the
// other direction would make that a cycle.
type Request struct {
	SourceAccountID      uuid.UUID
	DestinationAccountID uuid.UUID
	AmountMinor          int64
	Currency             string
}

// Fingerprint is a SHA-256 over the canonical form of a request.
type Fingerprint [sha256.Size]byte

// fingerprintVersion prefixes the canonical form.
//
// If the fields covered by a fingerprint ever change, this changes with them.
// Without it, an old record and a new one could hash differently for reasons
// that have nothing to do with the payment, and the mismatch would surface as
// a spurious idempotency conflict rather than as the deployment change it is.
const fingerprintVersion = "v1"

// Canonical returns the exact bytes that are hashed.
//
// The form is deliberately explicit and fully ordered: fixed field order,
// named fields, newline separated, every value rendered in a single
// unambiguous way. Nothing unstable is included — no timestamps, no request
// ids, no map iteration — so the same logical payment always produces the same
// fingerprint, in this process and in any other.
func (r Request) Canonical() string {
	return fingerprintVersion + "\n" +
		"source=" + r.SourceAccountID.String() + "\n" +
		"destination=" + r.DestinationAccountID.String() + "\n" +
		"amount_minor=" + strconv.FormatInt(r.AmountMinor, 10) + "\n" +
		"currency=" + r.Currency + "\n"
}

// Fingerprint hashes the canonical form.
//
// The point is to notice a key being reused for a *different* payment. Two
// requests that agree on source, destination, amount and currency are the same
// payment for deduplication purposes; anything else is a conflict, not a
// retry.
func (r Request) Fingerprint() Fingerprint {
	return sha256.Sum256([]byte(r.Canonical()))
}

// String renders the fingerprint as lower-case hex, which is how it is stored
// in Redis and how it appears in logs and errors.
func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }

// Bytes returns the raw digest, which is how it is stored in PostgreSQL —
// BYTEA, exactly 32 bytes, enforced by a CHECK constraint.
func (f Fingerprint) Bytes() []byte { return f[:] }

// ParseFingerprint reads a hex-encoded fingerprint, as stored in Redis.
func ParseFingerprint(s string) (Fingerprint, error) {
	var f Fingerprint

	raw, err := hex.DecodeString(s)
	if err != nil {
		return f, fmt.Errorf("idempotency: fingerprint is not hex: %w", err)
	}
	if len(raw) != len(f) {
		return f, fmt.Errorf("idempotency: fingerprint is %d bytes, want %d", len(raw), len(f))
	}
	copy(f[:], raw)
	return f, nil
}

// FingerprintFromBytes reads a raw digest, as stored in PostgreSQL.
func FingerprintFromBytes(raw []byte) (Fingerprint, error) {
	var f Fingerprint

	if len(raw) != len(f) {
		return f, fmt.Errorf("idempotency: fingerprint is %d bytes, want %d", len(raw), len(f))
	}
	copy(f[:], raw)
	return f, nil
}
