package invoice

import (
	"time"

	"github.com/google/uuid"
)

const MaxAmountMinor int64 = 9_007_199_254_740_991

type Request struct {
	RequestID   uuid.UUID `json:"request_id"`
	OrderID     string    `json:"order_id"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	Description string    `json:"description"`
	AcceptUntil time.Time `json:"accept_until"`
	Payer       Payer     `json:"payer"`
	Lines       []Line    `json:"lines"`
}

type Payer struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	INN          string `json:"inn"`
	KPP          string `json:"kpp"`
	LegalAddress string `json:"legal_address"`
	Email        string `json:"email,omitempty"`
}

type Line struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Quantity       int64  `json:"quantity"`
	Unit           string `json:"unit"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	AmountMinor    int64  `json:"amount_minor"`
	VAT            VAT    `json:"vat"`
}

type VAT struct {
	Kind            string `json:"kind"`
	RateBasisPoints *int32 `json:"rate_basis_points"`
	AmountMinor     int64  `json:"amount_minor"`
}

type Connection struct {
	ID           string
	MerchantID   uuid.UUID
	ProviderKind string
	EvidenceMode string
	Capabilities Capabilities
	Beneficiary  Beneficiary
}

type Capabilities struct {
	SupportedUnits       []string
	SupportsVATNone      bool
	SupportedVATRatesBPS []int32
	MaxAcceptDuration    time.Duration
	CreationReplay       string
}

type ConnectionSnapshot struct {
	ID           string       `json:"id"`
	ProviderKind string       `json:"provider_kind"`
	EvidenceMode string       `json:"evidence_mode"`
	Capabilities Capabilities `json:"capabilities"`
	Beneficiary  Beneficiary  `json:"beneficiary"`
}

type Record struct {
	PaymentID              uuid.UUID
	MerchantID             uuid.UUID
	CreatedAt              time.Time
	Request                Request
	RequestHash            [32]byte
	Connection             ConnectionSnapshot
	ExternalInvoiceID      string
	Artifact               *Artifact
	ProviderExpiresAt      *time.Time
	DispatchedAt           *time.Time
	NextReconcileAt        *time.Time
	UpdatedAt              time.Time
	InvoiceNumber          int64
	Reference              string
	EvidenceMode           string
	IssuanceState          string
	FundingState           string
	CreditedMinor          int64
	ReviewReasons          []string
	Revision               int64
	PaymentConfirmedAt     *time.Time
	ConfirmationRecordedAt *time.Time
}

func (c Connection) Snapshot() ConnectionSnapshot {
	return ConnectionSnapshot{ID: c.ID, ProviderKind: c.ProviderKind, EvidenceMode: c.EvidenceMode, Capabilities: c.Capabilities, Beneficiary: c.Beneficiary}
}
