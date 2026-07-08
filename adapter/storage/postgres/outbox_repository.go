package postgres

import (
	"context"
	"time"

	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OutboxRepository реализует хранилище outbox-событий в PostgreSQL.
type OutboxRepository struct {
	db queryer
}

var _ ports.OutboxRepository = (*OutboxRepository)(nil)

// NewOutboxRepository создает PostgreSQL-репозиторий outbox-событий.
func NewOutboxRepository(pool *pgxpool.Pool) *OutboxRepository {
	return &OutboxRepository{db: pool}
}

func newOutboxRepository(db queryer) *OutboxRepository {
	return &OutboxRepository{db: db}
}

// Create сохраняет новое outbox-событие.
func (r *OutboxRepository) Create(ctx context.Context, event outboxdomain.Event) error {
	const query = `
INSERT INTO outbox_events (
    id,
    aggregate_type,
    aggregate_id,
    event_type,
    payload,
    status,
    attempts,
    next_attempt_at,
    last_error,
    created_at,
    updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11)`

	_, err := r.db.Exec(ctx, query,
		event.ID,
		event.AggregateType,
		event.AggregateID,
		event.EventType,
		event.Payload,
		event.Status,
		event.Attempts,
		event.NextAttemptAt,
		event.LastError,
		event.CreatedAt,
		event.UpdatedAt,
	)
	return err
}

// FetchPending выбирает pending-события с блокировкой для конкурентных workers.
func (r *OutboxRepository) FetchPending(ctx context.Context, limit int, now time.Time) ([]outboxdomain.Event, error) {
	const query = `
SELECT id,
       aggregate_type,
       aggregate_id,
       event_type,
       payload,
       status,
       attempts,
       next_attempt_at,
       COALESCE(last_error, ''),
       created_at,
       updated_at
FROM outbox_events
WHERE status = 'pending'
  AND next_attempt_at <= $1
ORDER BY created_at
LIMIT $2
FOR UPDATE SKIP LOCKED`

	rows, err := r.db.Query(ctx, query, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []outboxdomain.Event
	for rows.Next() {
		var event outboxdomain.Event
		if err := rows.Scan(
			&event.ID,
			&event.AggregateType,
			&event.AggregateID,
			&event.EventType,
			&event.Payload,
			&event.Status,
			&event.Attempts,
			&event.NextAttemptAt,
			&event.LastError,
			&event.CreatedAt,
			&event.UpdatedAt,
		); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// MarkSent отмечает outbox-событие как успешно доставленное.
func (r *OutboxRepository) MarkSent(ctx context.Context, id uuid.UUID) error {
	const query = `
UPDATE outbox_events
SET status = 'sent',
    updated_at = now(),
    last_error = NULL
WHERE id = $1`
	_, err := r.db.Exec(ctx, query, id)
	return err
}

// MarkRetry планирует повторную доставку outbox-события.
func (r *OutboxRepository) MarkRetry(ctx context.Context, id uuid.UUID, attempts int, nextAttemptAt time.Time, lastError string) error {
	const query = `
UPDATE outbox_events
SET status = 'pending',
    attempts = $2,
    next_attempt_at = $3,
    last_error = $4,
    updated_at = now()
WHERE id = $1`
	_, err := r.db.Exec(ctx, query, id, attempts, nextAttemptAt, lastError)
	return err
}

// MarkFailed отмечает outbox-событие как окончательно не доставленное.
func (r *OutboxRepository) MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastError string) error {
	const query = `
UPDATE outbox_events
SET status = 'failed',
    attempts = $2,
    last_error = $3,
    updated_at = now()
WHERE id = $1`
	_, err := r.db.Exec(ctx, query, id, attempts, lastError)
	return err
}
