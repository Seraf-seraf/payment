package invoice

import (
	"github.com/google/uuid"
	"time"
)

type Beneficiary struct {
	Name                 string `json:"name"`
	INN                  string `json:"inn"`
	KPP                  string `json:"kpp"`
	Account              string `json:"account"`
	BankBIK              string `json:"bank_bik"`
	BankName             string `json:"bank_name"`
	CorrespondentAccount string `json:"correspondent_account"`
}

type Artifact struct {
	NextAction *NextAction `json:"next_action"`
	Documents  []Document  `json:"documents"`
}
type NextAction struct {
	Type        string       `json:"type"`
	Purpose     string       `json:"purpose,omitempty"`
	Beneficiary *Beneficiary `json:"beneficiary,omitempty"`
	URL         string       `json:"url,omitempty"`
	ValidUntil  *time.Time   `json:"valid_until,omitempty"`
}
type Document struct {
	Type       string     `json:"type"`
	URL        string     `json:"url"`
	ValidUntil *time.Time `json:"valid_until"`
}

type IssueOutcome string

const (
	Issued             IssueOutcome = "issued"
	DefinitelyRejected IssueOutcome = "definitely_rejected"
	OutcomeUnknown     IssueOutcome = "outcome_unknown"
)

type IssueResult struct {
	Outcome           IssueOutcome
	ExternalInvoiceID string
	Artifact          *Artifact
	ProviderExpiresAt *time.Time
	ErrorCode         string
}
type Recovery struct {
	Found        bool
	Invoice      IssueResult
	Observations []Observation
}

// Observation contains verified financial fields, never an HTTP envelope or credentials.
type Observation struct {
	OperationChannel      string     `json:"operation_channel,omitempty"`
	CanonicalFinancialKey bool       `json:"canonical_financial_key"`
	ExternalOperationID   string     `json:"external_operation_id,omitempty"`
	Kind                  string     `json:"kind"`
	ExternalInvoiceID     string     `json:"external_invoice_id,omitempty"`
	Reference             string     `json:"reference,omitempty"`
	FinancialKey          string     `json:"financial_key,omitempty"`
	EvidenceMode          string     `json:"evidence_mode,omitempty"`
	AmountMinor           int64      `json:"amount_minor,omitempty"`
	Currency              string     `json:"currency,omitempty"`
	RecipientAccount      string     `json:"recipient_account,omitempty"`
	PayerINN              string     `json:"payer_inn,omitempty"`
	FinalIncoming         bool       `json:"final_incoming,omitempty"`
	ProviderOccurredAt    *time.Time `json:"provider_occurred_at,omitempty"`
}
type Inbox struct {
	ID               uuid.UUID
	ConnectionID     string
	EventKey         string
	RawBodyHash      [32]byte
	Observation      Observation
	VerificationKind string
	State            string
	ReceivedAt       time.Time
	ProcessedAt      *time.Time
	ErrorCode        string
	NextMatchAt      *time.Time
}
type Evidence struct {
	VerificationKind   string
	ID                 uuid.UUID
	PaymentID          uuid.UUID
	ConnectionID       string
	FinancialKey       string
	NormalizedHash     [32]byte
	EvidenceMode       string
	AmountMinor        int64
	Currency           string
	Eligible           bool
	RecordedAt         time.Time
	ProviderOccurredAt *time.Time
}
