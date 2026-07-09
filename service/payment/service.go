package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/pkg/metrics"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

var (
	// ErrAlreadyExists означает конфликт уникального ключа при создании платежа.
	ErrAlreadyExists = errors.New("payment already exists")
	// ErrNotFound означает, что платеж не найден.
	ErrNotFound = errors.New("payment not found")
	// ErrPaymentInProgress означает, что идемпотентный платеж еще создается.
	ErrPaymentInProgress = errors.New("payment creation is in progress")
	// ErrPaymentCreationFailed означает, что предыдущая попытка создания платежа завершилась ошибкой.
	ErrPaymentCreationFailed = errors.New("payment creation failed")
	// ErrInvalidStatusTransition означает запрещенный переход статуса платежа.
	ErrInvalidStatusTransition = errors.New("invalid payment status transition")
)

// Service реализует сценарии создания и чтения платежей.
type Service struct {
	payments  ports.PaymentRepository
	providers ports.ProviderRegistry
	now       func() time.Time
}

var _ ports.PaymentUseCase = (*Service)(nil)

// NewService создает платежный сервис.
func NewService(payments ports.PaymentRepository, providers ports.ProviderRegistry, now func() time.Time) *Service {
	return &Service{
		payments:  payments,
		providers: providers,
		now:       now,
	}
}

// CreatePayment создает платеж или возвращает результат существующего идемпотентного платежа.
func (s *Service) CreatePayment(ctx context.Context, req ports.CreatePaymentRequest) (ports.CreatePaymentResult, error) {
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("%s:%d:%s", req.OrderID, req.AmountMinor, req.Currency)
	}

	existing, err := s.payments.FindByMerchantAndIdempotencyKey(ctx, req.Merchant.ID, idempotencyKey)
	if err == nil {
		return idempotentResult(existing)
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
		Status:          paymentdomain.StatusCreating,
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
			return idempotentResult(existing)
		}
		return ports.CreatePaymentResult{}, err
	}

	startedAt := time.Now()
	providerResult, err := provider.CreatePayment(ctx, providerdomain.CreatePaymentRequest{
		PaymentID:       created.ID.String(),
		OrderID:         req.OrderID,
		AmountMinor:     req.AmountMinor,
		Currency:        req.Currency,
		Description:     req.Description,
		PaymentMethod:   req.PaymentMethod,
		CustomerEmail:   req.CustomerEmail,
		Receipt:         req.Receipt,
		IdempotencyKey:  idempotencyKey,
		MerchantID:      req.Merchant.ID.String(),
		MerchantSuccess: req.Merchant.SuccessURL,
		MerchantFail:    req.Merchant.FailURL,
	})
	metrics.ObserveProviderRequest(req.Merchant.ProviderName, "create_payment", err == nil, time.Since(startedAt).Seconds())
	if err != nil {
		if markErr := s.payments.UpdateStatusIfAllowed(ctx, created.ID, paymentdomain.StatusFailed, []paymentdomain.Status{paymentdomain.StatusCreating}); markErr != nil && !errors.Is(markErr, ErrInvalidStatusTransition) {
			return ports.CreatePaymentResult{}, errors.Join(err, markErr)
		}
		return ports.CreatePaymentResult{}, err
	}

	if err := s.payments.UpdateProviderDataAndStatus(
		ctx,
		created.ID,
		providerResult.ProviderPaymentID,
		providerResult.PaymentURL,
		paymentdomain.StatusPending,
		[]paymentdomain.Status{paymentdomain.StatusCreating},
	); err != nil {
		return ports.CreatePaymentResult{}, err
	}
	created.ProviderPaymentID = providerResult.ProviderPaymentID
	created.PaymentURL = providerResult.PaymentURL
	created.Status = paymentdomain.StatusPending
	metrics.PaymentsCreatedTotal.Inc()

	return ports.CreatePaymentResult{Payment: created, Created: true}, nil
}

// GetPayment возвращает платеж по идентификатору.
func (s *Service) GetPayment(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error) {
	return s.payments.FindByID(ctx, id)
}

func idempotentResult(existing paymentdomain.Payment) (ports.CreatePaymentResult, error) {
	switch existing.Status {
	case paymentdomain.StatusPending, paymentdomain.StatusSucceeded, paymentdomain.StatusCanceled, paymentdomain.StatusRefunded:
		if existing.PaymentURL == "" || existing.ProviderPaymentID == "" {
			return ports.CreatePaymentResult{}, ErrPaymentInProgress
		}
		return ports.CreatePaymentResult{Payment: existing, Created: false}, nil
	case paymentdomain.StatusCreating:
		return ports.CreatePaymentResult{}, ErrPaymentInProgress
	case paymentdomain.StatusFailed:
		return ports.CreatePaymentResult{}, ErrPaymentCreationFailed
	default:
		return ports.CreatePaymentResult{}, ErrInvalidStatusTransition
	}
}
