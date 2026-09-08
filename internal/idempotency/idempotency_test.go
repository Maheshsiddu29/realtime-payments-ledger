package idempotency

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"application prefixed", "pay_01HQ8XZ4K9YN3TQF7WV2MC6RJD", true},
		{"uuid", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"single character", "a", true},
		{"max length", strings.Repeat("k", MaxKeyLength), true},
		{"punctuation is fine, keys are opaque", "order:2026-09-08/refund#1", true},
		{"empty", "", false},
		{"one over the maximum", strings.Repeat("k", MaxKeyLength+1), false},
		{"embedded space", "pay 123", false},
		{"leading space", " pay123", false},
		{"trailing newline", "pay123\n", false},
		{"tab", "pay\t123", false},
		{"null byte", "pay\x00123", false},
		{"delete character", "pay\x7f123", false},
		{"invalid utf-8", "pay\xff\xfe", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseKey(tt.input)
			if tt.valid {
				if err != nil {
					t.Fatalf("ParseKey(%q) = %v, want no error", tt.input, err)
				}
				if got.String() != tt.input {
					t.Errorf("ParseKey(%q) = %q", tt.input, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseKey(%q) succeeded, want an error", tt.input)
			}
			if !errors.Is(err, ErrInvalidKey) {
				t.Errorf("error %v does not wrap ErrInvalidKey", err)
			}
		})
	}
}

// Key naming is centralised so a second place cannot drift; a drifted prefix
// would silently disable deduplication rather than failing loudly.
func TestRedisKeyIsNamespaced(t *testing.T) {
	t.Parallel()

	key, err := ParseKey("pay_123")
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if got, want := key.RedisKey(), "idempotency:transfer:pay_123"; got != want {
		t.Errorf("RedisKey() = %q, want %q", got, want)
	}
}

func newRequest() Request {
	return Request{
		SourceAccountID:      uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		DestinationAccountID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		AmountMinor:          10000,
		Currency:             "USD",
	}
}

// The same logical payment must fingerprint identically every time, in any
// process. Nothing unstable may leak in.
func TestFingerprintIsStable(t *testing.T) {
	t.Parallel()

	first := newRequest().Fingerprint()
	for range 100 {
		if got := newRequest().Fingerprint(); got != first {
			t.Fatalf("fingerprint changed between identical requests: %s vs %s", got, first)
		}
	}
}

// Every field of the financial identity must change the fingerprint, or a key
// could be reused for a different payment undetected.
func TestFingerprintCoversEveryFinancialField(t *testing.T) {
	t.Parallel()

	base := newRequest()
	baseFingerprint := base.Fingerprint()

	tests := []struct {
		name   string
		mutate func(*Request)
	}{
		{"source account", func(r *Request) { r.SourceAccountID = uuid.New() }},
		{"destination account", func(r *Request) { r.DestinationAccountID = uuid.New() }},
		{"amount", func(r *Request) { r.AmountMinor = 10001 }},
		{"currency", func(r *Request) { r.Currency = "EUR" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mutated := base
			tt.mutate(&mutated)

			if mutated.Fingerprint() == baseFingerprint {
				t.Errorf("changing the %s did not change the fingerprint", tt.name)
			}
		})
	}
}

// Swapping source and destination is a different payment in the opposite
// direction, and must not collide.
func TestFingerprintDistinguishesDirection(t *testing.T) {
	t.Parallel()

	forward := newRequest()
	reverse := Request{
		SourceAccountID:      forward.DestinationAccountID,
		DestinationAccountID: forward.SourceAccountID,
		AmountMinor:          forward.AmountMinor,
		Currency:             forward.Currency,
	}

	if forward.Fingerprint() == reverse.Fingerprint() {
		t.Error("a transfer and its reverse produced the same fingerprint")
	}
}

// The canonical form must be unambiguous: no combination of field values may
// produce the same byte string as a different combination.
func TestCanonicalFormIsUnambiguous(t *testing.T) {
	t.Parallel()

	canonical := newRequest().Canonical()

	for _, want := range []string{
		"v1\n",
		"source=11111111-1111-1111-1111-111111111111\n",
		"destination=22222222-2222-2222-2222-222222222222\n",
		"amount_minor=10000\n",
		"currency=USD\n",
	} {
		if !strings.Contains(canonical, want) {
			t.Errorf("canonical form %q is missing %q", canonical, want)
		}
	}
	// Versioned, so a future change to the covered fields cannot be mistaken
	// for a conflicting payment.
	if !strings.HasPrefix(canonical, "v1\n") {
		t.Errorf("canonical form %q is not version prefixed", canonical)
	}
}

func TestFingerprintRoundTrips(t *testing.T) {
	t.Parallel()

	original := newRequest().Fingerprint()

	t.Run("through hex, as stored in redis", func(t *testing.T) {
		t.Parallel()

		got, err := ParseFingerprint(original.String())
		if err != nil {
			t.Fatalf("ParseFingerprint: %v", err)
		}
		if got != original {
			t.Errorf("round trip changed the fingerprint")
		}
	})

	t.Run("through bytes, as stored in postgres", func(t *testing.T) {
		t.Parallel()

		if got, want := len(original.Bytes()), 32; got != want {
			t.Fatalf("fingerprint is %d bytes, want %d to satisfy the CHECK constraint", got, want)
		}
		got, err := FingerprintFromBytes(original.Bytes())
		if err != nil {
			t.Fatalf("FingerprintFromBytes: %v", err)
		}
		if got != original {
			t.Errorf("round trip changed the fingerprint")
		}
	})
}

func TestFingerprintParsingRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	if _, err := ParseFingerprint("not hex"); err == nil {
		t.Error("ParseFingerprint accepted non-hex input")
	}
	if _, err := ParseFingerprint("abcd"); err == nil {
		t.Error("ParseFingerprint accepted a short digest")
	}
	if _, err := FingerprintFromBytes([]byte{1, 2, 3}); err == nil {
		t.Error("FingerprintFromBytes accepted a short digest")
	}
}

func TestRecordMatches(t *testing.T) {
	t.Parallel()

	fingerprint := newRequest().Fingerprint()
	record := Record{State: StateCompleted, Fingerprint: fingerprint.String()}

	if !record.Matches(fingerprint) {
		t.Error("Matches() = false for the fingerprint the record was built from")
	}

	other := newRequest()
	other.AmountMinor = 99999
	if record.Matches(other.Fingerprint()) {
		t.Error("Matches() = true for a different payment")
	}
}

// A nil store behaves as a permanently unavailable one, so the PostgreSQL
// fallback is exercised by ordinary unit tests rather than only by an outage.
func TestNilStoreReportsUnavailable(t *testing.T) {
	t.Parallel()

	var store *Store

	if store.Available() {
		t.Error("Available() = true for a nil store")
	}

	key := Key("pay_123")
	fingerprint := newRequest().Fingerprint()

	if _, err := store.Acquire(t.Context(), key, fingerprint); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Acquire = %v, want ErrUnavailable", err)
	}
	if _, _, err := store.Get(t.Context(), key); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get = %v, want ErrUnavailable", err)
	}
	if err := store.Complete(t.Context(), key, fingerprint, "t"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Complete = %v, want ErrUnavailable", err)
	}
	if err := store.Release(t.Context(), key); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Release = %v, want ErrUnavailable", err)
	}
	if err := store.Ping(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Ping = %v, want ErrUnavailable", err)
	}
}
