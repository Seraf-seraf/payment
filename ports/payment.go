package ports

import (
	"context"
	"net/http"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/google/uuid"
)

type PaymentUseCase interface {
	CreatePayment(ctx context.Context, req CreatePaymentRequest) (CreatePaymentResult, error)
	GetPayment(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error)
}

type CreatePaymentRequest struct {
	Merchant       merchantdomain.Merchant
	OrderID        string
	AmountMinor    int64
	Currency       string
	Description    string
	PaymentMethod  string
	CustomerEmail  string
	IdempotencyKey string
}

type CreatePaymentResult struct {
	Payment paymentdomain.Payment
	Created bool
}

type PaymentRepository interface {
	Create(ctx context.Context, payment paymentdomain.Payment) error
	FindByID(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error)
	FindByMerchantAndIdempotencyKey(ctx context.Context, merchantID uuid.UUID, key string) (paymentdomain.Payment, error)
	FindByProviderPaymentID(ctx context.Context, providerName, providerPaymentID string) (paymentdomain.Payment, error)
	UpdateProviderData(ctx context.Context, id uuid.UUID, providerPaymentID, paymentURL string) error
	UpdateStatus(ctx context.Context, id uuid.UUID, status paymentdomain.Status) error
}

type ProviderRegistry interface {
	Get(name string) (PaymentProvider, bool)
}

type PaymentProvider interface {
	Name() string
	CreatePayment(ctx context.Context, req providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error)
	VerifyWebhook(ctx context.Context, headers http.Header, rawBody []byte) error
	ParseWebhook(ctx context.Context, rawBody []byte) (providerdomain.WebhookEvent, error)
}
