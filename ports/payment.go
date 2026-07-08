package ports

import (
	"context"
	"net/http"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	webhookdomain "github.com/Seraf-seraf/payment/domain/webhook"
	"github.com/google/uuid"
)

// PaymentUseCase описывает сценарии работы с платежами.
type PaymentUseCase interface {
	CreatePayment(ctx context.Context, req CreatePaymentRequest) (CreatePaymentResult, error)
	GetPayment(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error)
}

// CreatePaymentRequest содержит данные для создания платежа от имени мерчанта.
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

// CreatePaymentResult содержит результат создания или идемпотентного получения платежа.
type CreatePaymentResult struct {
	Payment paymentdomain.Payment
	Created bool
}

// PaymentRepository описывает хранилище платежей.
type PaymentRepository interface {
	Create(ctx context.Context, payment paymentdomain.Payment) error
	FindByID(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error)
	FindByMerchantAndIdempotencyKey(ctx context.Context, merchantID uuid.UUID, key string) (paymentdomain.Payment, error)
	FindByProviderPaymentID(ctx context.Context, providerName, providerPaymentID string) (paymentdomain.Payment, error)
	UpdateProviderData(ctx context.Context, id uuid.UUID, providerPaymentID, paymentURL string) error
	UpdateProviderDataAndStatus(ctx context.Context, id uuid.UUID, providerPaymentID, paymentURL string, status paymentdomain.Status, allowedCurrent []paymentdomain.Status) error
	UpdateStatus(ctx context.Context, id uuid.UUID, status paymentdomain.Status) error
	UpdateStatusIfAllowed(ctx context.Context, id uuid.UUID, status paymentdomain.Status, allowedCurrent []paymentdomain.Status) error
}

// OutboxRepository описывает хранилище событий outbox.
type OutboxRepository interface {
	Create(ctx context.Context, event outboxdomain.Event) error
	FetchPending(ctx context.Context, limit int, now time.Time) ([]outboxdomain.Event, error)
	MarkSent(ctx context.Context, id uuid.UUID) error
	MarkRetry(ctx context.Context, id uuid.UUID, attempts int, nextAttemptAt time.Time, lastError string) error
	MarkFailed(ctx context.Context, id uuid.UUID, attempts int, lastError string) error
}

// Repositories объединяет tx-scoped репозитории, доступные внутри транзакции.
type Repositories struct {
	Payments      PaymentRepository
	WebhookEvents WebhookEventRepository
	Outbox        OutboxRepository
}

// TransactionManager выполняет набор операций внутри транзакции БД.
type TransactionManager interface {
	WithinTx(ctx context.Context, fn func(context.Context, Repositories) error) error
}

// CallbackSender доставляет уведомление о платеже на callback_url мерчанта.
type CallbackSender interface {
	Send(ctx context.Context, merchant merchantdomain.Merchant, payment paymentdomain.Payment, event outboxdomain.Event) error
}

// OutboxWorker обрабатывает pending outbox-события.
type OutboxWorker interface {
	Start(ctx context.Context)
	Stop(ctx context.Context) error
}

// WebhookEventRepository описывает хранилище webhook-событий провайдера.
type WebhookEventRepository interface {
	Create(ctx context.Context, event webhookdomain.Event) (bool, error)
	MarkProcessed(ctx context.Context, providerName, providerEventID string, processedAt time.Time) error
}

// ProviderRegistry возвращает платежного провайдера по имени.
type ProviderRegistry interface {
	Get(name string) (PaymentProvider, bool)
}

// PaymentProvider описывает интеграцию с платежным провайдером.
type PaymentProvider interface {
	Name() string
	CreatePayment(ctx context.Context, req providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error)
	VerifyWebhook(ctx context.Context, headers http.Header, rawBody []byte) error
	ParseWebhook(ctx context.Context, rawBody []byte) (providerdomain.WebhookEvent, error)
}
