package transfer

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
)

// Command.Validate covers the checks that need no database, so a malformed
// request never reaches PostgreSQL at all.
func TestCommandValidate(t *testing.T) {
	t.Parallel()

	src := uuid.New()
	dst := uuid.New()

	valid := Command{
		SourceAccountID:      src,
		DestinationAccountID: dst,
		AmountMinor:          1025,
		Currency:             money.Currency("USD"),
	}

	tests := []struct {
		name    string
		mutate  func(*Command)
		wantErr error
	}{
		{"valid command", func(*Command) {}, nil},
		{"zero amount", func(c *Command) { c.AmountMinor = 0 }, ErrInvalidAmount},
		{"negative amount", func(c *Command) { c.AmountMinor = -1 }, ErrInvalidAmount},
		{"same account", func(c *Command) { c.DestinationAccountID = c.SourceAccountID }, ErrSameAccount},
		{"empty currency", func(c *Command) { c.Currency = "" }, money.ErrInvalidCurrency},
		{"lower-case currency", func(c *Command) { c.Currency = "usd" }, money.ErrInvalidCurrency},
		{"nil source", func(c *Command) { c.SourceAccountID = uuid.Nil }, ErrSourceAccountNotFound},
		{"nil destination", func(c *Command) { c.DestinationAccountID = uuid.Nil }, ErrSourceAccountNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := valid
			tt.mutate(&cmd)

			err := cmd.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

// A one-cent transfer is valid: the minimum movement is one minor unit, not
// some rounded-off fraction.
func TestCommandValidateAcceptsSmallestUnit(t *testing.T) {
	t.Parallel()

	cmd := Command{
		SourceAccountID:      uuid.New(),
		DestinationAccountID: uuid.New(),
		AmountMinor:          1,
		Currency:             money.Currency("USD"),
	}
	if err := cmd.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil for a one-minor-unit transfer", err)
	}
}

// The status constants must match the values the transfers CHECK constraint
// permits, or every insert would fail at runtime.
func TestStatusValues(t *testing.T) {
	t.Parallel()

	for status, want := range map[Status]string{
		StatusPending:   "pending",
		StatusCompleted: "completed",
		StatusFailed:    "failed",
	} {
		if string(status) != want {
			t.Errorf("status = %q, want %q", status, want)
		}
	}
}
