package merchant

import (
	"context"
	"errors"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
)

var (
	// ErrNotFound означает, что мерчант не найден.
	ErrNotFound = errors.New("merchant not found")
	// ErrInactive означает, что мерчант отключен и не может создавать платежи.
	ErrInactive = errors.New("merchant is inactive")
)

// Service реализует аутентификацию мерчантов.
type Service struct {
	merchants ports.MerchantRepository
}

var _ ports.MerchantAuthenticator = (*Service)(nil)

// NewService создает сервис аутентификации мерчантов.
func NewService(merchants ports.MerchantRepository) *Service {
	return &Service{merchants: merchants}
}

// AuthenticateAPIKey находит активного мерчанта по hash(API key).
func (s *Service) AuthenticateAPIKey(ctx context.Context, apiKey string) (merchantdomain.Merchant, error) {
	merchant, err := s.merchants.FindByAPIKeyHash(ctx, crypto.SHA256Hex(apiKey))
	if err != nil {
		return merchantdomain.Merchant{}, err
	}
	if !merchant.IsActive {
		return merchantdomain.Merchant{}, ErrInactive
	}
	return merchant, nil
}
