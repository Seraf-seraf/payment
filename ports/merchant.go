package ports

import (
	"context"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
)

type MerchantAuthenticator interface {
	AuthenticateAPIKey(ctx context.Context, apiKey string) (merchantdomain.Merchant, error)
}

type MerchantRepository interface {
	FindByAPIKeyHash(ctx context.Context, apiKeyHash string) (merchantdomain.Merchant, error)
}
