package ports

import (
	"context"
	"net/http"
)

// WebhookHandler обрабатывает raw webhook от платежного провайдера.
type WebhookHandler interface {
	Handle(ctx context.Context, providerName string, headers http.Header, rawBody []byte) error
}
