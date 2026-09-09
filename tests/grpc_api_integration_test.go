//go:build integration

package tests

import (
	"testing"

	"google.golang.org/grpc/codes"

	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
)

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

func TestGRPCCreateAndGetAccount(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	created, err := g.client.CreateAccount(authed, &paymentsv1.CreateAccountRequest{Currency: "USD"})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	acct := created.GetAccount()
	if acct.GetId() == "" {
		t.Error("no account id returned")
	}
	if acct.GetCurrency() != "USD" {
		t.Errorf("currency = %q, want USD", acct.GetCurrency())
	}
	// The API offers no way to open an account holding money, and must not
	// invent one: that would be a credit with no matching debit.
	if acct.GetBalanceMinor() != 0 {
		t.Errorf("balance_minor = %d, want 0 for a new account", acct.GetBalanceMinor())
	}
	if acct.GetCreatedAt() == nil || acct.GetUpdatedAt() == nil {
		t.Error("timestamps were not populated")
	}

	fetched, err := g.client.GetAccount(authed, &paymentsv1.GetAccountRequest{Id: acct.GetId()})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if fetched.GetAccount().GetId() != acct.GetId() {
		t.Errorf("GetAccount returned %s, want %s", fetched.GetAccount().GetId(), acct.GetId())
	}
}

func TestGRPCAccountValidation(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	t.Run("invalid currencies are rejected", func(t *testing.T) {
		for _, currency := range []string{"", "US", "USDD", "usd", "U5D"} {
			_, err := g.client.CreateAccount(authed, &paymentsv1.CreateAccountRequest{Currency: currency})
			assertCode(t, err, codes.InvalidArgument)
		}
	})

	t.Run("malformed account id", func(t *testing.T) {
		for _, id := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
			_, err := g.client.GetAccount(authed, &paymentsv1.GetAccountRequest{Id: id})
			assertCode(t, err, codes.InvalidArgument)
		}
	})

	t.Run("unknown account is NOT_FOUND", func(t *testing.T) {
		_, err := g.client.GetAccount(authed, &paymentsv1.GetAccountRequest{Id: newUUIDString()})
		assertCode(t, err, codes.NotFound)
	})
}

// ---------------------------------------------------------------------------
// Transfers
// ---------------------------------------------------------------------------

func TestGRPCCreateTransfer(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	source := g.fundedAccountRPC(t, ctx, "USD", 100_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	resp, err := g.client.CreateTransfer(authed, &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 25_000, Currency: "USD", IdempotencyKey: "grpc-transfer-1",
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	tr := resp.GetTransfer()
	if tr.GetId() == "" {
		t.Error("no transfer id returned")
	}
	if tr.GetSourceAccountId() != source || tr.GetDestinationAccountId() != destination {
		t.Errorf("accounts = %s -> %s, want %s -> %s",
			tr.GetSourceAccountId(), tr.GetDestinationAccountId(), source, destination)
	}
	if tr.GetAmountMinor() != 25_000 {
		t.Errorf("amount_minor = %d, want 25000", tr.GetAmountMinor())
	}
	if tr.GetStatus() != paymentsv1.TransferStatus_TRANSFER_STATUS_COMPLETED {
		t.Errorf("status = %s, want COMPLETED", tr.GetStatus())
	}
	if tr.GetCompletedAt() == nil {
		t.Error("completed_at is nil on a completed transfer")
	}
	if resp.GetIdempotentReplay() {
		t.Error("a first request was marked as a replay")
	}

	// The money actually moved.
	if got, want := balanceOf(t, g.env, uuidMust(t, source)), int64(75_000); got != want {
		t.Errorf("source balance = %d, want %d", got, want)
	}
	if got, want := balanceOf(t, g.env, uuidMust(t, destination)), int64(25_000); got != want {
		t.Errorf("destination balance = %d, want %d", got, want)
	}
	assertReconciled(t, g.env)
}

// Every rejection path, through the transport, with the status code a client
// would actually see.
func TestGRPCTransferRejections(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	source := g.fundedAccountRPC(t, ctx, "USD", 10_000)
	destination := g.newAccountRPC(t, ctx, "USD")
	euro := g.newAccountRPC(t, ctx, "EUR")

	base := func() *paymentsv1.CreateTransferRequest {
		return &paymentsv1.CreateTransferRequest{
			SourceAccountId: source, DestinationAccountId: destination,
			AmountMinor: 100, Currency: "USD", IdempotencyKey: "rejection-" + newUUIDString(),
		}
	}

	tests := []struct {
		name   string
		mutate func(*paymentsv1.CreateTransferRequest)
		want   codes.Code
	}{
		{"insufficient funds", func(r *paymentsv1.CreateTransferRequest) { r.AmountMinor = 99_999_999 }, codes.FailedPrecondition},
		{"zero amount", func(r *paymentsv1.CreateTransferRequest) { r.AmountMinor = 0 }, codes.InvalidArgument},
		{"negative amount", func(r *paymentsv1.CreateTransferRequest) { r.AmountMinor = -500 }, codes.InvalidArgument},
		{"currency mismatch", func(r *paymentsv1.CreateTransferRequest) { r.DestinationAccountId = euro }, codes.FailedPrecondition},
		{"invalid currency code", func(r *paymentsv1.CreateTransferRequest) { r.Currency = "usd" }, codes.InvalidArgument},
		{"missing source account", func(r *paymentsv1.CreateTransferRequest) { r.SourceAccountId = newUUIDString() }, codes.NotFound},
		{"missing destination account", func(r *paymentsv1.CreateTransferRequest) { r.DestinationAccountId = newUUIDString() }, codes.NotFound},
		{"malformed source id", func(r *paymentsv1.CreateTransferRequest) { r.SourceAccountId = "nope" }, codes.InvalidArgument},
		{"source equals destination", func(r *paymentsv1.CreateTransferRequest) { r.DestinationAccountId = r.SourceAccountId }, codes.InvalidArgument},
		{"missing idempotency key", func(r *paymentsv1.CreateTransferRequest) { r.IdempotencyKey = "" }, codes.InvalidArgument},
		{"idempotency key with whitespace", func(r *paymentsv1.CreateTransferRequest) { r.IdempotencyKey = "has space" }, codes.InvalidArgument},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base()
			tt.mutate(req)

			_, err := g.client.CreateTransfer(authed, req)
			assertCode(t, err, tt.want)
		})
	}

	// Nothing may have been created by any rejected request.
	if n := countRows(t, g.env, "transfers"); n != 0 {
		t.Errorf("transfers table has %d rows after only rejections, want 0", n)
	}
	if got := balanceOf(t, g.env, uuidMust(t, source)); got != 10_000 {
		t.Errorf("source balance = %d, want 10000 unchanged", got)
	}
	assertReconciled(t, g.env)
}

// The scenario idempotency exists for, through the public API.
func TestGRPCDuplicateTransferReturnsTheOriginal(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	source := g.fundedAccountRPC(t, ctx, "USD", 50_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	req := &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 3_000, Currency: "USD", IdempotencyKey: "grpc-duplicate",
	}

	first, err := g.client.CreateTransfer(authed, req)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}

	for attempt := range 3 {
		replay, err := g.client.CreateTransfer(authed, req)
		if err != nil {
			t.Fatalf("replay %d: %v", attempt, err)
		}
		if replay.GetTransfer().GetId() != first.GetTransfer().GetId() {
			t.Errorf("replay %d returned %s, want %s",
				attempt, replay.GetTransfer().GetId(), first.GetTransfer().GetId())
		}
		if !replay.GetIdempotentReplay() {
			t.Errorf("replay %d was not flagged as a replay", attempt)
		}
	}

	if got, want := balanceOf(t, g.env, uuidMust(t, source)), int64(47_000); got != want {
		t.Errorf("source balance = %d, want %d: the payment was applied more than once", got, want)
	}
	if n := countTransfersForKey(t, g.env, key(t, "grpc-duplicate")); n != 1 {
		t.Errorf("transfer rows = %d, want 1", n)
	}
	assertReconciled(t, g.env)
}

// Reusing a key for a different payment must be refused and must not execute.
func TestGRPCIdempotencyConflict(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	source := g.fundedAccountRPC(t, ctx, "USD", 50_000)
	destination := g.newAccountRPC(t, ctx, "USD")
	other := g.newAccountRPC(t, ctx, "USD")

	original := &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 1_000, Currency: "USD", IdempotencyKey: "grpc-conflict",
	}
	if _, err := g.client.CreateTransfer(authed, original); err != nil {
		t.Fatalf("first request: %v", err)
	}
	balanceAfterFirst := balanceOf(t, g.env, uuidMust(t, source))

	conflicts := []struct {
		name   string
		mutate func(*paymentsv1.CreateTransferRequest)
	}{
		{"different amount", func(r *paymentsv1.CreateTransferRequest) { r.AmountMinor = 2_000 }},
		{"different destination", func(r *paymentsv1.CreateTransferRequest) { r.DestinationAccountId = other }},
	}

	for _, tt := range conflicts {
		t.Run(tt.name, func(t *testing.T) {
			req := &paymentsv1.CreateTransferRequest{
				SourceAccountId: original.SourceAccountId, DestinationAccountId: original.DestinationAccountId,
				AmountMinor: original.AmountMinor, Currency: original.Currency,
				IdempotencyKey: original.IdempotencyKey,
			}
			tt.mutate(req)

			// ALREADY_EXISTS: the key already names a different payment, and
			// no amount of retrying will change that. The client needs a new
			// key.
			_, err := g.client.CreateTransfer(authed, req)
			assertCode(t, err, codes.AlreadyExists)
		})
	}

	if got := balanceOf(t, g.env, uuidMust(t, source)); got != balanceAfterFirst {
		t.Errorf("source balance = %d, want %d: a conflicting request executed", got, balanceAfterFirst)
	}
	if n := countTransfersForKey(t, g.env, key(t, "grpc-conflict")); n != 1 {
		t.Errorf("transfer rows = %d, want 1", n)
	}
	assertReconciled(t, g.env)
}

func TestGRPCGetTransfer(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	source := g.fundedAccountRPC(t, ctx, "USD", 10_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	posted, err := g.client.CreateTransfer(authed, &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 500, Currency: "USD", IdempotencyKey: "grpc-get-transfer",
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}

	fetched, err := g.client.GetTransfer(authed, &paymentsv1.GetTransferRequest{Id: posted.GetTransfer().GetId()})
	if err != nil {
		t.Fatalf("GetTransfer: %v", err)
	}
	if fetched.GetTransfer().GetId() != posted.GetTransfer().GetId() {
		t.Errorf("GetTransfer returned the wrong transfer")
	}
	if fetched.GetTransfer().GetAmountMinor() != 500 {
		t.Errorf("amount_minor = %d, want 500", fetched.GetTransfer().GetAmountMinor())
	}

	t.Run("unknown transfer", func(t *testing.T) {
		_, err := g.client.GetTransfer(authed, &paymentsv1.GetTransferRequest{Id: newUUIDString()})
		assertCode(t, err, codes.NotFound)
	})

	t.Run("malformed id", func(t *testing.T) {
		_, err := g.client.GetTransfer(authed, &paymentsv1.GetTransferRequest{Id: "not-a-uuid"})
		assertCode(t, err, codes.InvalidArgument)
	})
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

func TestGRPCListLedgerEntriesForTransfer(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	source := g.fundedAccountRPC(t, ctx, "USD", 10_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	posted, err := g.client.CreateTransfer(authed, &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 2_500, Currency: "USD", IdempotencyKey: "grpc-ledger",
	})
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	transferID := posted.GetTransfer().GetId()

	resp, err := g.client.ListLedgerEntriesForTransfer(authed,
		&paymentsv1.ListLedgerEntriesForTransferRequest{TransferId: transferID})
	if err != nil {
		t.Fatalf("ListLedgerEntriesForTransfer: %v", err)
	}

	entries := resp.GetEntries()
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want exactly 2", len(entries))
	}

	// Debits first, so entry 0 is the debit against the source.
	debit, credit := entries[0], entries[1]
	if debit.GetAmountMinor() != -2_500 {
		t.Errorf("debit = %d, want -2500", debit.GetAmountMinor())
	}
	if debit.GetAccountId() != source {
		t.Errorf("debit is against %s, want the source %s", debit.GetAccountId(), source)
	}
	if credit.GetAmountMinor() != 2_500 {
		t.Errorf("credit = %d, want 2500", credit.GetAmountMinor())
	}
	if credit.GetAccountId() != destination {
		t.Errorf("credit is against %s, want the destination %s", credit.GetAccountId(), destination)
	}
	if debit.GetAmountMinor()+credit.GetAmountMinor() != 0 {
		t.Error("the entries do not sum to zero")
	}
	for _, e := range entries {
		if e.GetTransferId() != transferID {
			t.Errorf("entry belongs to transfer %s, want %s", e.GetTransferId(), transferID)
		}
		if e.GetCurrency() != "USD" || e.GetCreatedAt() == nil {
			t.Errorf("entry is missing currency or timestamp: %+v", e)
		}
	}

	t.Run("unknown transfer is NOT_FOUND, not an empty list", func(t *testing.T) {
		_, err := g.client.ListLedgerEntriesForTransfer(authed,
			&paymentsv1.ListLedgerEntriesForTransferRequest{TransferId: newUUIDString()})
		assertCode(t, err, codes.NotFound)
	})

	t.Run("malformed id", func(t *testing.T) {
		_, err := g.client.ListLedgerEntriesForTransfer(authed,
			&paymentsv1.ListLedgerEntriesForTransferRequest{TransferId: "nope"})
		assertCode(t, err, codes.InvalidArgument)
	})
}

// The API must offer no way to write to the ledger. This asserts it against
// the generated descriptor, so adding such an RPC fails the build's tests.
func TestGRPCExposesNoLedgerWriteRPC(t *testing.T) {
	forbidden := []string{"CreateLedgerEntry", "UpdateLedgerEntry", "DeleteLedgerEntry",
		"SetBalance", "UpdateAccountBalance", "DeleteTransfer", "UpdateTransfer"}

	for _, method := range paymentsv1.PaymentsService_ServiceDesc.Methods {
		for _, bad := range forbidden {
			if method.MethodName == bad {
				t.Errorf("the service exposes %s; the ledger is append-only and balances move only through CreateTransfer",
					bad)
			}
		}
	}
}
