package ports

import (
	"context"
	"net/http"
	"time"

	webhookdomain "github.com/Seraf-seraf/payment/domain/webhook"
)

type WebhookHandler interface {
	Handle(ctx context.Context, providerName string, headers http.Header, rawBody []byte) error
}

type WebhookEventRepository interface {
	Create(ctx context.Context, event webhookdomain.Event) (bool, error)
	MarkProcessed(ctx context.Context, providerName, providerEventID string, processedAt time.Time) error
}
