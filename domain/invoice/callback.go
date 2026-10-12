package invoice

import (
	"github.com/google/uuid"
	"time"
)

type CallbackEnvelope struct {
	SchemaVersion          string     `json:"schema_version"`
	EventID                uuid.UUID  `json:"event_id"`
	EventType              string     `json:"event_type"`
	MerchantID             uuid.UUID  `json:"merchant_id"`
	PaymentID              uuid.UUID  `json:"payment_id"`
	OrderID                string     `json:"order_id"`
	PaymentMethod          string     `json:"payment_method"`
	Revision               int64      `json:"revision"`
	Reason                 string     `json:"reason"`
	OccurredAt             time.Time  `json:"occurred_at"`
	Status                 string     `json:"status"`
	IssuanceState          string     `json:"issuance_state"`
	FundingState           string     `json:"funding_state"`
	AmountMinor            int64      `json:"amount_minor"`
	CreditedMinor          int64      `json:"credited_minor"`
	Currency               string     `json:"currency"`
	ConfirmedPaid          bool       `json:"confirmed_paid"`
	RequiresReview         bool       `json:"requires_review"`
	ReviewReasons          []string   `json:"review_reasons"`
	AcceptUntil            time.Time  `json:"accept_until"`
	PaymentConfirmedAt     *time.Time `json:"payment_confirmed_at"`
	ConfirmationRecordedAt *time.Time `json:"confirmation_recorded_at"`
}
