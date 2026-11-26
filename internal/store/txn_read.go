package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TxnStatus returns the status of the transaction with this id in agent.transactions, and ok false
// when finstore holds no such transaction.
func TxnStatus(ctx context.Context, q DBTX, id uuid.UUID) (status string, ok bool, err error) {
	err = q.QueryRow(ctx, `SELECT status FROM agent.transactions WHERE id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return status, true, nil
}

// TxnFlowClass returns the flow class and reason agent.cash_flows gives the transaction with this
// id, and ok false when the view has no row for it (a declined or unknown transaction).
func TxnFlowClass(ctx context.Context, q DBTX, id uuid.UUID) (class, reason string, ok bool, err error) {
	err = q.QueryRow(ctx, `SELECT flow_class, flow_reason FROM agent.cash_flows WHERE id = $1`, id).Scan(&class, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return class, reason, true, nil
}
