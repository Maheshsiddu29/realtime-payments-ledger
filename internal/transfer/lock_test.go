package transfer

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

// lockOrder is the whole of the deadlock-prevention strategy, so its
// properties are worth stating explicitly.
func TestLockOrderIsCanonical(t *testing.T) {
	t.Parallel()

	low := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	high := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

	t.Run("lower uuid first when given in order", func(t *testing.T) {
		t.Parallel()

		first, second := lockOrder(low, high)
		if first != low || second != high {
			t.Errorf("lockOrder(low, high) = (%s, %s), want (%s, %s)", first, second, low, high)
		}
	})

	t.Run("lower uuid first when given reversed", func(t *testing.T) {
		t.Parallel()

		first, second := lockOrder(high, low)
		if first != low || second != high {
			t.Errorf("lockOrder(high, low) = (%s, %s), want (%s, %s)", first, second, low, high)
		}
	})
}

// The property that actually prevents deadlocks: the order depends only on
// which two accounts are involved, never on which is the source. A to B and
// B to A must request the same rows in the same sequence.
func TestLockOrderIsIndependentOfTransferDirection(t *testing.T) {
	t.Parallel()

	for range 1000 {
		a, b := uuid.New(), uuid.New()

		forwardFirst, forwardSecond := lockOrder(a, b)
		reverseFirst, reverseSecond := lockOrder(b, a)

		if forwardFirst != reverseFirst || forwardSecond != reverseSecond {
			t.Fatalf("direction changed the lock order: (%s, %s) vs (%s, %s)",
				forwardFirst, forwardSecond, reverseFirst, reverseSecond)
		}
	}
}

// The order must be a genuine total order, or two accounts could still be
// requested in conflicting sequences.
func TestLockOrderIsATotalOrder(t *testing.T) {
	t.Parallel()

	for range 1000 {
		a, b := uuid.New(), uuid.New()

		first, second := lockOrder(a, b)

		if bytes.Compare(first[:], second[:]) > 0 {
			t.Fatalf("lockOrder returned %s before %s, which is out of byte order", first, second)
		}
		// The pair must be preserved, not invented.
		if !((first == a && second == b) || (first == b && second == a)) {
			t.Fatalf("lockOrder(%s, %s) returned (%s, %s)", a, b, first, second)
		}
	}
}

func TestLockOrderWithIdenticalIDs(t *testing.T) {
	t.Parallel()

	// Post rejects self-transfers before locking, but lockOrder must still be
	// well defined rather than depending on an unstable comparison.
	id := uuid.New()
	first, second := lockOrder(id, id)
	if first != id || second != id {
		t.Errorf("lockOrder(id, id) = (%s, %s), want both %s", first, second, id)
	}
}

// missingAccountError must attribute a missing row to the correct business
// role. Because rows are locked in UUID order, the first lock to fail is not
// necessarily the source.
func TestMissingAccountErrorAttributesTheCorrectRole(t *testing.T) {
	t.Parallel()

	source := uuid.New()
	destination := uuid.New()

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()

		err := missingAccountError(pgxErrNoRows(), source, source)
		if !isErr(err, ErrSourceAccountNotFound) {
			t.Errorf("err = %v, want ErrSourceAccountNotFound", err)
		}
	})

	t.Run("missing destination", func(t *testing.T) {
		t.Parallel()

		err := missingAccountError(pgxErrNoRows(), destination, source)
		if !isErr(err, ErrDestinationAccountNotFound) {
			t.Errorf("err = %v, want ErrDestinationAccountNotFound", err)
		}
	})

	t.Run("other errors pass through unchanged", func(t *testing.T) {
		t.Parallel()

		original := errString("connection reset")
		if got := missingAccountError(original, source, source); got != original {
			t.Errorf("err = %v, want the original error unchanged", got)
		}
	})
}
