package grpcapi

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/ledger"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// PaymentsService is the gRPC transport for the ledger.
//
// It is deliberately thin. Every handler validates its input, calls exactly
// one existing service method, and converts the result to protobuf. There is
// no transfer logic here, no locking, no retry, no idempotency handling and no
// SQL — reimplementing any of that in the transport would be a second,
// divergent copy of the rules that keep the ledger correct.
type PaymentsService struct {
	paymentsv1.UnimplementedPaymentsServiceServer

	accounts  *account.Repository
	transfers *transfer.Service
	entries   *ledger.Repository
	log       *slog.Logger
}

// NewPaymentsService wires the transport to the existing service layer.
func NewPaymentsService(
	accounts *account.Repository,
	transfers *transfer.Service,
	entries *ledger.Repository,
	log *slog.Logger,
) *PaymentsService {
	return &PaymentsService{accounts: accounts, transfers: transfers, entries: entries, log: log}
}

// CreateAccount opens an account with a zero balance.
//
// There is deliberately no starting-balance field in the request. An account
// created already holding money would be a credit with no matching debit, and
// the books would not balance from the first row. Funding is a transfer.
func (s *PaymentsService) CreateAccount(ctx context.Context, req *paymentsv1.CreateAccountRequest) (*paymentsv1.CreateAccountResponse, error) {
	const method = "CreateAccount"

	currency, err := parseCurrency(req.GetCurrency())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	acct, err := s.accounts.Create(ctx, currency)
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}
	return &paymentsv1.CreateAccountResponse{Account: accountToProto(acct)}, nil
}

func (s *PaymentsService) GetAccount(ctx context.Context, req *paymentsv1.GetAccountRequest) (*paymentsv1.GetAccountResponse, error) {
	const method = "GetAccount"

	id, err := parseUUID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	acct, err := s.accounts.Get(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}
	return &paymentsv1.GetAccountResponse{Account: accountToProto(acct)}, nil
}

// CreateTransfer posts a transfer through the existing idempotent service.
//
// Everything that makes a transfer safe happens below this call and is
// untouched by it: the idempotency claim and its PostgreSQL uniqueness
// barrier, canonical-order row locking, SERIALIZABLE isolation, bounded
// serialization retry, and the ledger invariants enforced by deferred
// constraint triggers. The transport's only jobs are to validate the request
// and to convert the answer.
func (s *PaymentsService) CreateTransfer(ctx context.Context, req *paymentsv1.CreateTransferRequest) (*paymentsv1.CreateTransferResponse, error) {
	const method = "CreateTransfer"

	// An idempotency key is required. Accepting a transfer without one would
	// mean a retried payment could post twice, which is the failure this whole
	// mechanism exists to prevent.
	key, err := idempotency.ParseKey(req.GetIdempotencyKey())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	source, err := parseUUID("source_account_id", req.GetSourceAccountId())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}
	destination, err := parseUUID("destination_account_id", req.GetDestinationAccountId())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}
	currency, err := parseCurrency(req.GetCurrency())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	// protobuf guarantees this is an int64; it guarantees nothing about it
	// being a sensible amount. Zero and negative values are rejected here and
	// again by Command.Validate and a CHECK constraint.
	if req.GetAmountMinor() <= 0 {
		return nil, toStatus(ctx, s.log, method,
			fmt.Errorf("%w: amount_minor must be greater than zero, got %d",
				transfer.ErrInvalidAmount, req.GetAmountMinor()))
	}

	result, err := s.transfers.PostIdempotent(ctx, key, transfer.Command{
		SourceAccountID:      source,
		DestinationAccountID: destination,
		AmountMinor:          req.GetAmountMinor(),
		Currency:             currency,
	})
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	return &paymentsv1.CreateTransferResponse{
		Transfer:         transferToProto(result.Transfer),
		IdempotentReplay: result.Replayed,
	}, nil
}

func (s *PaymentsService) GetTransfer(ctx context.Context, req *paymentsv1.GetTransferRequest) (*paymentsv1.GetTransferResponse, error) {
	const method = "GetTransfer"

	id, err := parseUUID("id", req.GetId())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	t, err := s.transfers.Get(ctx, id)
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}
	return &paymentsv1.GetTransferResponse{Transfer: transferToProto(t)}, nil
}

// ListLedgerEntriesForTransfer returns the entries a transfer posted.
//
// Read-only by construction: internal/ledger exposes no way to create, modify
// or delete an entry, so there is no write RPC that could be added here by
// accident.
//
// The response is inherently bounded — a transfer posts exactly two entries —
// so there is no pagination. Adding it now would be an abstraction with no
// caller. An account statement endpoint, which is genuinely unbounded, would
// need it.
func (s *PaymentsService) ListLedgerEntriesForTransfer(ctx context.Context, req *paymentsv1.ListLedgerEntriesForTransferRequest) (*paymentsv1.ListLedgerEntriesForTransferResponse, error) {
	const method = "ListLedgerEntriesForTransfer"

	transferID, err := parseUUID("transfer_id", req.GetTransferId())
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	// A transfer that does not exist is NOT_FOUND rather than an empty list:
	// "no entries" and "no such transfer" are different answers, and a client
	// reconciling its own records needs to tell them apart.
	if _, err := s.transfers.Get(ctx, transferID); err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	found, err := s.entries.EntriesForTransfer(ctx, transferID)
	if err != nil {
		return nil, toStatus(ctx, s.log, method, err)
	}

	out := make([]*paymentsv1.LedgerEntry, 0, len(found))
	for _, e := range found {
		out = append(out, ledgerEntryToProto(e))
	}
	return &paymentsv1.ListLedgerEntriesForTransferResponse{Entries: out}, nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// parseUUID validates an identifier supplied by a client.
//
// The field name is included so a caller with several id fields learns which
// one was wrong, which is information about their own request and safe to
// return.
func parseUUID(field, raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, fmt.Errorf("%w: %s is required", ErrInvalidRequest, field)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %s is not a valid UUID", ErrInvalidRequest, field)
	}
	if id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: %s must not be the zero UUID", ErrInvalidRequest, field)
	}
	return id, nil
}

func parseCurrency(raw string) (money.Currency, error) {
	currency, err := money.ParseCurrency(raw)
	if err != nil {
		return "", err
	}
	return currency, nil
}

// ---------------------------------------------------------------------------
// Protobuf conversion
// ---------------------------------------------------------------------------

func accountToProto(a account.Account) *paymentsv1.Account {
	return &paymentsv1.Account{
		Id:           a.ID.String(),
		Currency:     a.Currency.String(),
		BalanceMinor: a.BalanceMinor,
		CreatedAt:    timestampToProto(a.CreatedAt),
		UpdatedAt:    timestampToProto(a.UpdatedAt),
	}
}

func transferToProto(t transfer.Transfer) *paymentsv1.Transfer {
	out := &paymentsv1.Transfer{
		Id:                   t.ID.String(),
		SourceAccountId:      t.SourceAccountID.String(),
		DestinationAccountId: t.DestinationAccountID.String(),
		AmountMinor:          t.AmountMinor,
		Currency:             t.Currency.String(),
		Status:               transferStatusToProto(t.Status),
		CreatedAt:            timestampToProto(t.CreatedAt),
	}
	if t.CompletedAt != nil {
		out.CompletedAt = timestampToProto(*t.CompletedAt)
	}
	return out
}

func ledgerEntryToProto(e ledger.Entry) *paymentsv1.LedgerEntry {
	return &paymentsv1.LedgerEntry{
		Id:          e.ID.String(),
		TransferId:  e.TransferID.String(),
		AccountId:   e.AccountID.String(),
		AmountMinor: e.AmountMinor,
		Currency:    e.Currency.String(),
		CreatedAt:   timestampToProto(e.CreatedAt),
	}
}

func transferStatusToProto(s transfer.Status) paymentsv1.TransferStatus {
	switch s {
	case transfer.StatusPending:
		return paymentsv1.TransferStatus_TRANSFER_STATUS_PENDING
	case transfer.StatusCompleted:
		return paymentsv1.TransferStatus_TRANSFER_STATUS_COMPLETED
	case transfer.StatusFailed:
		return paymentsv1.TransferStatus_TRANSFER_STATUS_FAILED
	default:
		return paymentsv1.TransferStatus_TRANSFER_STATUS_UNSPECIFIED
	}
}

// timestampToProto converts a Go time, treating the zero value as absent
// rather than as the year 1. A nil Timestamp is how protobuf says "not set".
func timestampToProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
