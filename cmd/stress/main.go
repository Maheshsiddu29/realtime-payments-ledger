// Command stress is DEVELOPMENT AND TESTING TOOLING. It is not part of the
// service and is never deployed.
//
// It drives internal/transfer directly, with no network layer, to measure how
// the ledger behaves under concurrent posting: how many transfers succeed, how
// much contention they cost in retries, and — the part that matters — whether
// every accounting invariant still holds afterwards.
//
// It calls the transfer service in-process because there is no transfer API
// yet; gRPC arrives in a later phase. Adding an HTTP endpoint purely to give
// this tool something to call would be scope no one asked for.
//
// Usage:
//
//	stress -scenario oneway   -attempts 1000 -concurrency 1000
//	stress -scenario opposing -attempts 500  -concurrency 500
//	stress -scenario ring     -attempts 600  -concurrency 200 -accounts 4
//	stress -attempts 1000 -json > docs/results/run.json
//
// By default the tool creates and funds its own accounts, which writes
// balances directly. THAT CREATES MONEY FROM NOTHING and has no production
// equivalent — it is the same fixture exception the integration tests use, and
// it exists only so a run can start from a funded state. Pass -source and
// -destination to use existing accounts instead.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/database"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/reconcile"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

type options struct {
	scenario    string
	attempts    int
	concurrency int
	amount      int64
	currency    string
	fund        int64
	accounts    int
	source      string
	destination string
	timeout     time.Duration
	asJSON      bool
	verbose     bool
}

func main() {
	var opt options

	flag.StringVar(&opt.scenario, "scenario", "oneway", "oneway | opposing | ring")
	flag.IntVar(&opt.attempts, "attempts", 100, "total transfer attempts")
	flag.IntVar(&opt.concurrency, "concurrency", 0, "maximum attempts in flight (0 = all at once)")
	flag.Int64Var(&opt.amount, "amount", 100, "amount to transfer, in minor units")
	flag.StringVar(&opt.currency, "currency", "USD", "currency code")
	flag.Int64Var(&opt.fund, "fund", 0, "minor units to fund each created account with (0 = enough for every attempt)")
	flag.IntVar(&opt.accounts, "accounts", 4, "number of accounts for the ring scenario")
	flag.StringVar(&opt.source, "source", "", "existing source account UUID (default: create one)")
	flag.StringVar(&opt.destination, "destination", "", "existing destination account UUID (default: create one)")
	flag.DurationVar(&opt.timeout, "timeout", 5*time.Minute, "overall timeout for the run")
	flag.BoolVar(&opt.asJSON, "json", false, "emit the result as JSON")
	flag.BoolVar(&opt.verbose, "v", false, "log transfer activity to stderr")
	flag.Parse()

	if err := run(opt); err != nil {
		fmt.Fprintf(os.Stderr, "stress: %v\n", err)
		os.Exit(1)
	}
}

// Result is the machine-readable outcome of a run.
type Result struct {
	Scenario    string `json:"scenario"`
	Attempts    int    `json:"attempts"`
	Concurrency int    `json:"concurrency"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`

	Successful         int `json:"successful"`
	InsufficientFunds  int `json:"insufficient_funds"`
	ExhaustedRetries   int `json:"exhausted_retries"`
	UnexpectedFailures int `json:"unexpected_failures"`

	SerializationRetries int `json:"serialization_retries"`
	DeadlockRetries      int `json:"deadlock_retries"`
	TotalAttempts        int `json:"total_attempts"`
	MaxAttemptsForOne    int `json:"max_attempts_for_one_transfer"`
	RetryBudget          int `json:"retry_budget"`

	NegativeBalances          int  `json:"negative_balances"`
	LedgerInvariantViolations int  `json:"ledger_invariant_violations"`
	ConservationHeld          bool `json:"conservation_held"`
	ReconciliationOK          bool `json:"reconciliation_ok"`

	TotalBalanceBefore int64            `json:"total_balance_minor_before"`
	TotalBalanceAfter  int64            `json:"total_balance_minor_after"`
	BalancesBefore     map[string]int64 `json:"balances_before"`
	BalancesAfter      map[string]int64 `json:"balances_after"`

	CompletedTransfers int `json:"completed_transfers"`
	PendingTransfers   int `json:"pending_transfers"`
	LedgerEntries      int `json:"ledger_entries"`

	DurationMS  int64    `json:"duration_ms"`
	Problems    []string `json:"problems,omitempty"`
	Environment Env      `json:"environment"`
}

// Env records where the numbers came from, so a result file cannot be mistaken
// for a general claim about all hardware.
type Env struct {
	GoVersion         string `json:"go_version"`
	OSArch            string `json:"os_arch"`
	CPUs              int    `json:"cpus"`
	PostgresVersion   string `json:"postgres_version"`
	StartedAt         string `json:"started_at"`
	PoolMaxConns      int    `json:"pool_max_conns"`
	IsolationLevel    string `json:"isolation_level"`
	LockOrderStrategy string `json:"lock_order_strategy"`
}

// OK reports whether every invariant held. It is the only line of the output
// that matters for correctness; everything else is throughput detail.
func (r Result) OK() bool {
	return r.NegativeBalances == 0 &&
		r.LedgerInvariantViolations == 0 &&
		r.ConservationHeld &&
		r.ReconciliationOK &&
		r.UnexpectedFailures == 0
}

func run(opt options) error {
	if opt.attempts < 1 {
		return errors.New("-attempts must be at least 1")
	}
	if opt.concurrency <= 0 || opt.concurrency > opt.attempts {
		opt.concurrency = opt.attempts
	}
	currency, err := money.ParseCurrency(opt.currency)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logOut := io.Discard
	if opt.verbose {
		logOut = os.Stderr
	}
	log := slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := database.New(cfg, log)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()

	if err := db.Verify(ctx); err != nil {
		return fmt.Errorf("%w\n\nStart PostgreSQL with: make infra-up && make migrate-up", err)
	}

	pool := db.Pool()
	svc := transfer.NewService(pool, log)

	participants, err := setupAccounts(ctx, opt, currency, pool)
	if err != nil {
		return err
	}

	cmds := buildCommands(opt, currency, participants)

	// The baseline is taken before the workload, so reconciliation afterwards
	// checks that this run explained every movement it made.
	baseline, err := reconcile.SnapshotBaseline(ctx, pool)
	if err != nil {
		return err
	}

	before, totalBefore, err := readBalances(ctx, pool, participants)
	if err != nil {
		return err
	}

	result := Result{
		Scenario:    opt.scenario,
		Attempts:    len(cmds),
		Concurrency: opt.concurrency,
		AmountMinor: opt.amount,
		Currency:    currency.String(),
		RetryBudget: svc.RetryPolicy().MaxAttempts,
		Environment: describeEnv(ctx, pool, cfg),
	}

	execute(ctx, svc, cmds, opt.concurrency, &result)

	after, totalAfter, err := readBalances(ctx, pool, participants)
	if err != nil {
		return err
	}
	result.BalancesBefore, result.TotalBalanceBefore = before, totalBefore
	result.BalancesAfter, result.TotalBalanceAfter = after, totalAfter
	result.ConservationHeld = totalBefore == totalAfter

	report, err := reconcile.Check(ctx, pool, baseline)
	if err != nil {
		return err
	}
	result.NegativeBalances = len(report.NegativeBalances)
	result.LedgerInvariantViolations = len(report.UnbalancedTransfers) + len(report.BalanceMismatches)
	if report.TotalLedgerMinor != 0 {
		result.LedgerInvariantViolations++
	}
	result.ReconciliationOK = report.OK()
	result.CompletedTransfers = report.CompletedTransfers
	result.PendingTransfers = report.PendingTransfers
	result.LedgerEntries = report.LedgerEntries
	result.Problems = report.Problems()
	if !result.ConservationHeld {
		result.Problems = append(result.Problems,
			fmt.Sprintf("total balances changed from %d to %d during a transfer-only workload",
				totalBefore, totalAfter))
	}

	if err := emit(result, opt.asJSON); err != nil {
		return err
	}
	if !result.OK() {
		return errors.New("invariant violations were detected; see the problems above")
	}
	return nil
}

// execute runs every command, at most `concurrency` at a time, and classifies
// the outcomes.
//
// When concurrency equals the attempt count — the default — every goroutine
// waits on a barrier and they are released together, so the contention is real
// rather than an artefact of staggered starts.
func execute(ctx context.Context, svc *transfer.Service, cmds []transfer.Command, concurrency int, result *Result) {
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	slots := make(chan struct{}, concurrency)
	start := make(chan struct{})

	began := time.Now()
	for _, cmd := range cmds {
		wg.Add(1)
		go func(cmd transfer.Command) {
			defer wg.Done()

			<-start
			slots <- struct{}{}
			defer func() { <-slots }()

			_, attempts, err := svc.PostWithAttempts(ctx, cmd)

			mu.Lock()
			defer mu.Unlock()

			result.TotalAttempts += attempts.Total
			result.SerializationRetries += attempts.SerializationFailures
			result.DeadlockRetries += attempts.Deadlocks
			result.MaxAttemptsForOne = max(result.MaxAttemptsForOne, attempts.Total)

			switch {
			case err == nil:
				result.Successful++
			case errors.Is(err, transfer.ErrInsufficientFunds):
				result.InsufficientFunds++
			case errors.Is(err, transfer.ErrRetriesExhausted):
				result.ExhaustedRetries++
			default:
				result.UnexpectedFailures++
				if len(result.Problems) < 10 {
					result.Problems = append(result.Problems, "unexpected failure: "+err.Error())
				}
			}
		}(cmd)
	}

	close(start)
	wg.Wait()
	result.DurationMS = time.Since(began).Milliseconds()
}

// setupAccounts returns the accounts the scenario will move money between,
// creating and funding them unless existing ids were supplied.
func setupAccounts(ctx context.Context, opt options, currency money.Currency, pool *pgxpool.Pool) ([]uuid.UUID, error) {
	if opt.source != "" || opt.destination != "" {
		src, err := uuid.Parse(opt.source)
		if err != nil {
			return nil, fmt.Errorf("-source: %w", err)
		}
		dst, err := uuid.Parse(opt.destination)
		if err != nil {
			return nil, fmt.Errorf("-destination: %w", err)
		}
		return []uuid.UUID{src, dst}, nil
	}

	count := 2
	if opt.scenario == "ring" {
		count = opt.accounts
		if count < 2 {
			return nil, errors.New("-accounts must be at least 2 for the ring scenario")
		}
	}

	// Fund every account for the whole workload by default, so a run measures
	// contention rather than how quickly the source runs dry.
	funding := opt.fund
	if funding == 0 {
		funding = int64(opt.attempts) * opt.amount
	}

	repo := account.NewRepository(pool)
	ids := make([]uuid.UUID, 0, count)
	for range count {
		acct, err := repo.Create(ctx, currency)
		if err != nil {
			return nil, fmt.Errorf("creating account: %w", err)
		}
		// DEVELOPMENT TOOLING ONLY: a direct balance write, with no ledger
		// entry explaining it. See the package comment.
		if funding > 0 {
			if _, err := pool.Exec(ctx,
				`UPDATE accounts SET balance_minor = balance_minor + $2 WHERE id = $1`,
				acct.ID, funding); err != nil {
				return nil, fmt.Errorf("funding account %s: %w", acct.ID, err)
			}
		}
		ids = append(ids, acct.ID)
	}
	return ids, nil
}

// buildCommands turns a scenario into the exact list of transfers to attempt.
// Every scenario is deterministic: the same flags always produce the same
// workload, so a failure can be reproduced.
func buildCommands(opt options, currency money.Currency, accounts []uuid.UUID) []transfer.Command {
	cmds := make([]transfer.Command, 0, opt.attempts)

	newCmd := func(from, to uuid.UUID) transfer.Command {
		return transfer.Command{
			SourceAccountID: from, DestinationAccountID: to,
			AmountMinor: opt.amount, Currency: currency,
		}
	}

	switch opt.scenario {
	case "opposing":
		// Alternating directions over one pair: the deadlock scenario.
		for i := range opt.attempts {
			if i%2 == 0 {
				cmds = append(cmds, newCmd(accounts[0], accounts[1]))
			} else {
				cmds = append(cmds, newCmd(accounts[1], accounts[0]))
			}
		}
	case "ring":
		// Each attempt moves money one step around the ring, so every pair of
		// neighbours is contended from both sides across the run.
		n := len(accounts)
		for i := range opt.attempts {
			from := accounts[i%n]
			to := accounts[(i+1)%n]
			cmds = append(cmds, newCmd(from, to))
		}
	default: // "oneway"
		for range opt.attempts {
			cmds = append(cmds, newCmd(accounts[0], accounts[1]))
		}
	}
	return cmds
}

func readBalances(ctx context.Context, pool *pgxpool.Pool, ids []uuid.UUID) (map[string]int64, int64, error) {
	balances := make(map[string]int64, len(ids))
	repo := account.NewRepository(pool)

	for _, id := range ids {
		acct, err := repo.Get(ctx, id)
		if err != nil {
			return nil, 0, fmt.Errorf("reading account %s: %w", id, err)
		}
		balances[id.String()] = acct.BalanceMinor
	}

	var total int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(balance_minor), 0) FROM accounts`).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("summing balances: %w", err)
	}
	return balances, total, nil
}

func describeEnv(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) Env {
	var version string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		version = "unknown"
	}
	return Env{
		GoVersion:         runtimeVersion(),
		OSArch:            runtimeOSArch(),
		CPUs:              runtimeCPUs(),
		PostgresVersion:   version,
		StartedAt:         time.Now().UTC().Format(time.RFC3339),
		PoolMaxConns:      cfg.Postgres.MaxOpenConns,
		IsolationLevel:    "SERIALIZABLE",
		LockOrderStrategy: "canonical UUID order, lowest first",
	}
}

func emit(result Result, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	fmt.Printf("scenario           %s\n", result.Scenario)
	fmt.Printf("attempts           %d (concurrency %d, amount %d %s)\n",
		result.Attempts, result.Concurrency, result.AmountMinor, result.Currency)
	fmt.Printf("duration           %d ms\n\n", result.DurationMS)

	fmt.Printf("successful         %d\n", result.Successful)
	fmt.Printf("insufficient funds %d\n", result.InsufficientFunds)
	fmt.Printf("exhausted retries  %d\n", result.ExhaustedRetries)
	fmt.Printf("unexpected         %d\n\n", result.UnexpectedFailures)

	fmt.Printf("serialization retries %d\n", result.SerializationRetries)
	fmt.Printf("deadlock retries      %d\n", result.DeadlockRetries)
	fmt.Printf("total attempts        %d (budget %d, worst single transfer %d)\n\n",
		result.TotalAttempts, result.RetryBudget, result.MaxAttemptsForOne)

	fmt.Printf("total balance      %d -> %d\n", result.TotalBalanceBefore, result.TotalBalanceAfter)
	for id, before := range result.BalancesBefore {
		fmt.Printf("  %s  %d -> %d\n", id, before, result.BalancesAfter[id])
	}
	fmt.Println()

	fmt.Printf("negative balances           %d\n", result.NegativeBalances)
	fmt.Printf("ledger invariant violations %d\n", result.LedgerInvariantViolations)
	fmt.Printf("conservation held           %t\n", result.ConservationHeld)
	fmt.Printf("reconciliation ok           %t\n", result.ReconciliationOK)
	fmt.Printf("completed transfers         %d (pending %d, ledger entries %d)\n",
		result.CompletedTransfers, result.PendingTransfers, result.LedgerEntries)

	for _, p := range result.Problems {
		fmt.Printf("\nPROBLEM: %s\n", p)
	}
	if result.OK() {
		fmt.Println("\nall invariants held for this run")
	}
	return nil
}
