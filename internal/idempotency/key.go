// Package idempotency provides the coordination layer that turns a repeated
// payment request into a single financial transfer.
//
// It owns key validation, request fingerprinting, and the Redis record that
// tracks a request's state. It deliberately contains **no financial logic**:
// it never posts a transfer, never touches balances and never writes a ledger
// entry. The transfer service orchestrates this package and the posting
// mechanism; that direction of dependency is what keeps the two separable.
//
// Redis is not the correctness boundary. It makes duplicate detection fast and
// lets a replay be answered without touching the database, but it can be
// flushed, can expire a key, and can be unavailable at exactly the wrong
// moment. The UNIQUE constraint on transfers.idempotency_key is what actually
// guarantees at most one transfer per key. See docs/IDEMPOTENCY.md.
package idempotency

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// Key length bounds.
//
// Keys are opaque: this system never parses one or assumes a format, so a
// UUID, a ULID or an application-specific string such as "pay_01H..." are all
// acceptable. Only the bounds are enforced, because unbounded input in a
// unique index is a denial-of-service surface. The same limits are enforced
// again by a CHECK constraint in PostgreSQL.
const (
	MinKeyLength = 1
	MaxKeyLength = 255
)

// ErrInvalidKey is returned for a key that is empty, too long, or contains
// characters that would make it unsafe to use in a Redis key.
var ErrInvalidKey = errors.New("idempotency: invalid key")

// Key is a validated, opaque client-supplied idempotency key.
//
// The zero Key is invalid. Construct one with ParseKey.
type Key string

// ParseKey validates a client-supplied key.
//
// Control characters and whitespace are rejected rather than escaped. Redis
// keys are binary-safe so they would technically be accepted, but a key
// containing a newline or a space is almost always a client bug — a trimmed
// header, a concatenation gone wrong — and silently accepting it would make
// two requests that look identical in a log resolve to different keys.
func ParseKey(s string) (Key, error) {
	if len(s) < MinKeyLength {
		return "", fmt.Errorf("%w: key must not be empty", ErrInvalidKey)
	}
	if len(s) > MaxKeyLength {
		return "", fmt.Errorf("%w: key is %d bytes, the maximum is %d",
			ErrInvalidKey, len(s), MaxKeyLength)
	}
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: key is not valid UTF-8", ErrInvalidKey)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f {
			return "", fmt.Errorf("%w: key must not contain whitespace or control characters", ErrInvalidKey)
		}
	}
	return Key(s), nil
}

func (k Key) String() string { return string(k) }

// redisKeyPrefix namespaces every idempotency record.
//
// Key naming lives here and nowhere else: a second place that builds these
// strings is a second place that can drift, and a drifted prefix silently
// disables deduplication rather than failing loudly.
const redisKeyPrefix = "idempotency:transfer:"

// RedisKey returns the namespaced Redis key for this idempotency key.
func (k Key) RedisKey() string { return redisKeyPrefix + string(k) }
