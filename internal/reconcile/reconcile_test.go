package reconcile

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestReportOK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report Report
		wantOK bool
	}{
		{"clean", Report{}, true},
		{
			name:   "ledger does not sum to zero",
			report: Report{TotalLedgerMinor: 1},
			wantOK: false,
		},
		{
			name:   "negative balance",
			report: Report{NegativeBalances: []NegativeBalance{{AccountID: uuid.New(), BalanceMinor: -1}}},
			wantOK: false,
		},
		{
			name:   "balance mismatch",
			report: Report{BalanceMismatches: []BalanceMismatch{{AccountID: uuid.New()}}},
			wantOK: false,
		},
		{
			name:   "unbalanced transfer",
			report: Report{UnbalancedTransfers: []TransferProblem{{TransferID: uuid.New()}}},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.report.OK(); got != tt.wantOK {
				t.Errorf("OK() = %v, want %v", got, tt.wantOK)
			}
			if wantProblems := !tt.wantOK; wantProblems && len(tt.report.Problems()) == 0 {
				t.Error("Problems() is empty for a failing report")
			}
		})
	}
}

func TestReportViolationsCountsEveryProblem(t *testing.T) {
	t.Parallel()

	report := Report{
		TotalLedgerMinor:    5,
		NegativeBalances:    []NegativeBalance{{}, {}},
		BalanceMismatches:   []BalanceMismatch{{}},
		UnbalancedTransfers: []TransferProblem{{}, {}, {}},
	}

	if got, want := report.Violations(), 7; got != want {
		t.Errorf("Violations() = %d, want %d", got, want)
	}
	if got, want := len(report.Problems()), 7; got != want {
		t.Errorf("len(Problems()) = %d, want %d", got, want)
	}
}

// A clean report must produce no noise, so a passing stress run reports
// nothing rather than an empty-looking problem.
func TestCleanReportHasNoProblems(t *testing.T) {
	t.Parallel()

	report := Report{Accounts: 4, CompletedTransfers: 100, LedgerEntries: 200}
	if !report.OK() {
		t.Fatal("OK() = false for a clean report")
	}
	if got := report.Problems(); len(got) != 0 {
		t.Errorf("Problems() = %v, want none", got)
	}
	if got := report.Violations(); got != 0 {
		t.Errorf("Violations() = %d, want 0", got)
	}
}

func TestBalanceMismatchDifference(t *testing.T) {
	t.Parallel()

	m := BalanceMismatch{StoredMinor: 900, BaselineMinor: 1000, LedgerMinor: -50, ExpectedMinor: 950}
	if got, want := m.Difference(), int64(-50); got != want {
		t.Errorf("Difference() = %d, want %d", got, want)
	}
}

// The problem text must name the account and both sides of the discrepancy, or
// an operator cannot act on it.
func TestProblemTextIsActionable(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	report := Report{
		BalanceMismatches: []BalanceMismatch{{
			AccountID: id, StoredMinor: 900, BaselineMinor: 1000, LedgerMinor: -50, ExpectedMinor: 950,
		}},
	}

	problems := report.Problems()
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1", len(problems))
	}
	for _, want := range []string{id.String(), "900", "1000", "950"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("problem %q does not mention %q", problems[0], want)
		}
	}
}

func TestTransferProblemString(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	got := TransferProblem{TransferID: id, EntryCount: 1, EntrySumMinor: -500, DebitCount: 1}.String()

	for _, want := range []string{id.String(), "entries=1", "sum=-500"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, does not mention %q", got, want)
		}
	}
}
