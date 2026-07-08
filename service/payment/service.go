package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

var (
	ErrAlreadyExists = errors.New("payment already exists")
	ErrNotFound      = errors.New("payment not found")
)

type Service struct {
	payments  ports.PaymentRepository
	providers ports.ProviderRegistry
	now       func() time.Time
}

var _ ports.PaymentUseCase = (*Service)(nil)

func NewService(payments ports.PaymentRepository, providers ports.ProviderRegistry, now func() time.Time) *Service {
	return &Service{
		payments:  payments,
		providers: providers,
		now:       now,
	}
}

func (s *Service) CreatePayment(ctx context.Context, req ports.CreatePaymentRequest) (ports.CreatePaymentResult, error) {
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("%s:%d:%s", req.OrderID, req.AmountMinor, req.Currency)
	}

	existing, err := s.payments.FindByMerchantAndIdempotencyKey(ctx, req.Merchant.ID, idempotencyKey)
	if err == nil {
		return ports.CreatePaymentResult{Payment: existing, Created: false}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return ports.CreatePaymentResult{}, err
	}

	provider, ok := s.providers.Get(req.Merchant.ProviderName)
	if !ok {
		return ports.CreatePaymentResult{}, fmt.Errorf("provider %q not registered", req.Merchant.ProviderName)
	}

	now := s.now()
	created := paymentdomain.Payment{
		ID:              uuid.New(),
		MerchantID:      req.Merchant.ID,
		ProviderName:    req.Merchant.ProviderName,
		MerchantOrderID: req.OrderID,
		IdempotencyKey:  idempotencyKey,
		AmountMinor:     req.AmountMinor,
		Currency:        req.Currency,
		Description:     req.Description,
		Status:          paymentdomain.StatusPending,
		Metadata:        map[string]any{},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.payments.Create(ctx, created); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			existing, findErr := s.payments.FindByMerchantAndIdempotencyKey(ctx, req.Merchant.ID, idempotencyKey)
			if findErr != nil {
				return ports.CreatePaymentResult{}, findErr
			}
			return ports.CreatePaymentResult{Payment: existing, Created: false}, nil
		}
		return ports.CreatePaymentResult{}, err
	}

	providerResult, err := provider.CreatePayment(ctx, providerdomain.CreatePaymentRequest{
		PaymentID:       created.ID.String(),
		OrderID:         req.OrderID,
		AmountMinor:     req.AmountMinor,
		Currency:        req.Currency,
		Description:     req.Description,
		PaymentMethod:   req.PaymentMethod,
		CustomerEmail:   req.CustomerEmail,
		IdempotencyKey:  idempotencyKey,
		MerchantID:      req.Merchant.ID.String(),
		MerchantSuccess: req.Merchant.SuccessURL,
		MerchantFail:    req.Merchant.FailURL,
	})
	if err != nil {
		return ports.CreatePaymentResult{}, err
	}

	if err := s.payments.UpdateProviderData(ctx, created.ID, providerResult.ProviderPaymentID, providerResult.PaymentURL); err != nil {
		return ports.CreatePaymentResult{}, err
	}
	created.ProviderPaymentID = providerResult.ProviderPaymentID
	created.PaymentURL = providerResult.PaymentURL

	return ports.CreatePaymentResult{Payment: created, Created: true}, nil
}

func (s *Service) GetPayment(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error) {
	return s.payments.FindByID(ctx, id)
}
