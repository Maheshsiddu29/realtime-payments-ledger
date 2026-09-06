package money

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{"usd", "USD", true},
		{"eur", "EUR", true},
		{"jpy", "JPY", true},
		{"empty", "", false},
		{"too short", "US", false},
		{"too long", "USDD", false},
		{"lower case is not silently upper-cased", "usd", false},
		{"mixed case", "Usd", false},
		{"digits", "US1", false},
		{"padded", "US ", false},
		{"non-ascii", "€UR", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseCurrency(tt.input)
			if tt.valid {
				if err != nil {
					t.Fatalf("ParseCurrency(%q) = %v, want no error", tt.input, err)
				}
				if string(got) != tt.input {
					t.Errorf("ParseCurrency(%q) = %q, want %q", tt.input, got, tt.input)
				}
				return
			}

			if err == nil {
				t.Fatalf("ParseCurrency(%q) succeeded, want an error", tt.input)
			}
			if !errors.Is(err, ErrInvalidCurrency) {
				t.Errorf("error %v does not wrap ErrInvalidCurrency", err)
			}
			if got != "" {
				t.Errorf("ParseCurrency(%q) = %q, want the zero value on error", tt.input, got)
			}
		})
	}
}

// The error must name the offending value so a caller can see what was wrong.
func TestValidateErrorNamesTheValue(t *testing.T) {
	t.Parallel()

	err := Currency("us").Validate()
	if err == nil {
		t.Fatal("Validate() succeeded, want an error")
	}
	if !strings.Contains(err.Error(), `"us"`) {
		t.Errorf("error %q does not quote the offending value", err)
	}
}

func TestCurrencyString(t *testing.T) {
	t.Parallel()

	if got, want := Currency("GBP").String(), "GBP"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
