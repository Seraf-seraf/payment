package ports

import (
	"context"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/google/uuid"
)

// MerchantAuthenticator аутентифицирует мерчанта по исходному API key.
type MerchantAuthenticator interface {
	AuthenticateAPIKey(ctx context.Context, apiKey string) (merchantdomain.Merchant, error)
}

// MerchantRepository описывает хранилище мерчантов.
type MerchantRepository interface {
	FindByAPIKeyHash(ctx context.Context, apiKeyHash string) (merchantdomain.Merchant, error)
	FindByID(ctx context.Context, id uuid.UUID) (merchantdomain.Merchant, error)
}
