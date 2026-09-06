package ledger

import "testing"

func TestEntryDirection(t *testing.T) {
	t.Parallel()

	debit := Entry{AmountMinor: -10000}
	credit := Entry{AmountMinor: 10000}

	if !debit.IsDebit() || debit.IsCredit() {
		t.Errorf("entry with amount %d should be a debit only", debit.AmountMinor)
	}
	if !credit.IsCredit() || credit.IsDebit() {
		t.Errorf("entry with amount %d should be a credit only", credit.AmountMinor)
	}
}

// The double-entry invariant in its simplest form: a matched debit and credit
// sum to zero.
func TestSumMinor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []Entry
		want    int64
	}{
		{"no entries", nil, 0},
		{
			name:    "balanced pair",
			entries: []Entry{{AmountMinor: -10000}, {AmountMinor: 10000}},
			want:    0,
		},
		{
			name:    "unbalanced pair",
			entries: []Entry{{AmountMinor: -10000}, {AmountMinor: 9999}},
			want:    -1,
		},
		{
			name:    "balanced multi-leg",
			entries: []Entry{{AmountMinor: -10000}, {AmountMinor: 7500}, {AmountMinor: 2500}},
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := SumMinor(tt.entries); got != tt.want {
				t.Errorf("SumMinor() = %d, want %d", got, tt.want)
			}
		})
	}
}
