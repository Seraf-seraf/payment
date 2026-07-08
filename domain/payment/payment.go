package payment

import (
	"time"

	"github.com/google/uuid"
)

// Status описывает состояние платежа.
type Status string

const (
	// StatusCreating означает, что платеж создан локально, но провайдер еще не вернул ссылку оплаты.
	StatusCreating Status = "creating"
	// StatusPending означает, что платеж ожидает оплаты пользователем.
	StatusPending Status = "pending"
	// StatusSucceeded означает, что платеж успешно оплачен.
	StatusSucceeded Status = "succeeded"
	// StatusFailed означает, что платеж завершился ошибкой.
	StatusFailed Status = "failed"
	// StatusCanceled означает, что платеж отменен.
	StatusCanceled Status = "canceled"
	// StatusRefunded означает, что платеж возвращен.
	StatusRefunded Status = "refunded"
)

// Payment описывает платеж и его состояние в системе.
type Payment struct {
	ID                uuid.UUID
	MerchantID        uuid.UUID
	ProviderName      string
	ProviderPaymentID string
	MerchantOrderID   string
	IdempotencyKey    string
	AmountMinor       int64
	Currency          string
	Description       string
	Status            Status
	PaymentURL        string
	Metadata          map[string]any
	CreatedAt         time.Time
	UpdatedAt         time.Time
	PaidAt            *time.Time
	CanceledAt        *time.Time
	RefundedAt        *time.Time
}
