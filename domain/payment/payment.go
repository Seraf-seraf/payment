package payment

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
	StatusRefunded  Status = "refunded"
)

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
