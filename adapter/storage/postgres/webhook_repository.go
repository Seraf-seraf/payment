package postgres

import (
	"context"
	"encoding/json"
	"time"

	webhookdomain "github.com/Seraf-seraf/payment/domain/webhook"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebhookRepository реализует хранилище webhook-событий в PostgreSQL.
type WebhookRepository struct {
	db queryer
}

var _ ports.WebhookEventRepository = (*WebhookRepository)(nil)

// NewWebhookRepository создает PostgreSQL-репозиторий webhook-событий.
func NewWebhookRepository(pool *pgxpool.Pool) *WebhookRepository {
	return &WebhookRepository{db: pool}
}

func newWebhookRepository(db queryer) *WebhookRepository {
	return &WebhookRepository{db: db}
}

// Create сохраняет webhook-событие и возвращает false при дубле.
func (r *WebhookRepository) Create(ctx context.Context, event webhookdomain.Event) (bool, error) {
	rawPayload := event.RawPayload
	if len(rawPayload) == 0 {
		rawPayload = json.RawMessage(`{}`)
	}

	const query = `
INSERT INTO webhook_events (
    id,
    provider_name,
    provider_event_id,
    provider_payment_id,
    event_type,
    raw_payload,
    signature_valid,
    created_at
) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8)
ON CONFLICT (provider_name, provider_event_id) DO NOTHING`

	tag, err := r.db.Exec(ctx, query,
		event.ID,
		event.ProviderName,
		event.ProviderEventID,
		event.ProviderPaymentID,
		event.EventType,
		rawPayload,
		event.SignatureValid,
		event.CreatedAt,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkProcessed отмечает webhook-событие как обработанное.
func (r *WebhookRepository) MarkProcessed(ctx context.Context, providerName, providerEventID string, processedAt time.Time) error {
	const query = `
UPDATE webhook_events
SET processed_at = $3
WHERE provider_name = $1 AND provider_event_id = $2`
	_, err := r.db.Exec(ctx, query, providerName, providerEventID, processedAt)
	return err
}
