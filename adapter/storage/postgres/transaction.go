package postgres

import (
	"context"

	"github.com/Seraf-seraf/payment/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type queryer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TransactionManager управляет PostgreSQL-транзакциями для нескольких репозиториев.
type TransactionManager struct {
	pool          *pgxpool.Pool
	encryptionKey string
}

var _ ports.TransactionManager = (*TransactionManager)(nil)

// NewTransactionManager создает transaction manager для PostgreSQL.
func NewTransactionManager(pool *pgxpool.Pool, encryptionKey string) *TransactionManager {
	return &TransactionManager{pool: pool, encryptionKey: encryptionKey}
}

// WithinTx выполняет функцию внутри транзакции и передает tx-scoped репозитории.
func (m *TransactionManager) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	repos := ports.Repositories{
		Payments:      newPaymentRepository(tx),
		WebhookEvents: newWebhookRepository(tx),
		Outbox:        newOutboxRepository(tx),
	}
	if err := fn(ctx, repos); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
