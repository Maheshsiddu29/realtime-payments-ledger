// Package money defines how monetary values are represented across the
// ledger.
//
// Two rules hold everywhere in this system:
//
//  1. Amounts are integers in minor units — cents for USD, so $10.25 is 1025.
//     Floating point is never used for money. Binary floating point cannot
//     represent most decimal fractions exactly, so 0.1 + 0.2 != 0.3, and the
//     error compounds over a sequence of operations. In a ledger that error
//     eventually shows up as money that does not exist.
//
//  2. An amount is meaningless without a currency. Currencies are never
//     converted implicitly; a transfer between accounts of different
//     currencies is rejected rather than guessed at.
package money

import (
	"errors"
	"fmt"
)

// ErrInvalidCurrency is returned for anything that is not a three-letter
// upper-case currency code.
var ErrInvalidCurrency = errors.New("money: invalid currency code")

// Currency is an ISO 4217 alphabetic code such as USD or EUR.
//
// The database column is VARCHAR(3) with a matching CHECK constraint, so an
// invalid code cannot be stored even if it bypasses this type.
type Currency string

// ParseCurrency validates a currency code and returns it.
//
// Codes are compared exactly: lower-case input is rejected rather than
// silently upper-cased, because "usd" in a request usually means the caller is
// sending something other than a currency code.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(s)
	if err := c.Validate(); err != nil {
		return "", err
	}
	return c, nil
}

// Validate reports whether the currency is a well-formed code.
func (c Currency) Validate() error {
	if len(c) != 3 {
		return fmt.Errorf("%w: %q must be exactly 3 characters", ErrInvalidCurrency, string(c))
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return fmt.Errorf("%w: %q must be upper-case ASCII letters", ErrInvalidCurrency, string(c))
		}
	}
	return nil
}

func (c Currency) String() string { return string(c) }
