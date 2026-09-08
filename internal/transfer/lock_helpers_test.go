package transfer

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// Small helpers shared by the locking tests.

func pgxErrNoRows() error { return pgx.ErrNoRows }

func isErr(err, target error) bool { return errors.Is(err, target) }

type errString string

func (e errString) Error() string { return string(e) }
