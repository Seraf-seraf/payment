package merchant

import (
	"context"
	"errors"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
)

var (
	ErrNotFound = errors.New("merchant not found")
	ErrInactive = errors.New("merchant is inactive")
)

type Service struct {
	merchants ports.MerchantRepository
}

var _ ports.MerchantAuthenticator = (*Service)(nil)

func NewService(merchants ports.MerchantRepository) *Service {
	return &Service{merchants: merchants}
}

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
