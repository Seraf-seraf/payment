package ports

import (
	"context"
	"errors"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/google/uuid"
)

type InvoiceUseCase interface {
	CreateInvoicePayment(ctx context.Context, req invoicedomain.Request, merchant merchantdomain.Merchant) (InvoiceCreateResult, error)
}

type InvoiceCreateResult struct {
	Invoice invoicedomain.Record
	Created bool
}

type InvoiceRepository interface {
	FindByMerchantAndRequestID(ctx context.Context, merchantID, requestID uuid.UUID) (invoicedomain.Record, error)
	Create(ctx context.Context, record invoicedomain.Record) (invoicedomain.Record, error)
	Lock(ctx context.Context, id uuid.UUID) (invoicedomain.Record, error)
	Save(ctx context.Context, record invoicedomain.Record) error
	LockPayment(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error)
	SavePayment(ctx context.Context, payment paymentdomain.Payment, now time.Time) error
	Match(ctx context.Context, connectionID, externalID, reference string) ([]uuid.UUID, error)
	Due(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)
	RecordEvidence(ctx context.Context, evidence invoicedomain.Evidence) (bool, bool, error)
}

type InvoiceCommand struct {
	ID             uuid.UUID
	CommandType    string
	PaymentID      uuid.UUID
	InboxID        uuid.UUID
	Attempts       int
	LeaseToken     uuid.UUID
	LeaseExpiresAt time.Time
	Status         string
	NextAttemptAt  time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type InvoiceCommandRepository interface {
	Create(ctx context.Context, command InvoiceCommand) error
	Claim(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]InvoiceCommand, error)
	Fence(ctx context.Context, command InvoiceCommand) error
	Finish(ctx context.Context, command InvoiceCommand, status string, next time.Time, errorCode string, now time.Time) error
}

type InvoiceProviderRegistry interface {
	ForMerchant(merchantID uuid.UUID) (invoicedomain.Connection, bool)
}

var ErrLeaseLost = errors.New("invoice command lease lost")

type InvoiceInboxRepository interface {
	Create(ctx context.Context, inbox invoicedomain.Inbox) (bool, error)
	Get(ctx context.Context, id uuid.UUID) (invoicedomain.Inbox, error)
	SetState(ctx context.Context, id uuid.UUID, state, errorCode string, next *time.Time, now time.Time) error
	Unmatched(ctx context.Context, connectionID, externalID, reference string, limit int) ([]invoicedomain.Inbox, error)
	DueUnmatched(ctx context.Context, now time.Time, limit int) ([]invoicedomain.Inbox, error)
}

type InvoiceProvider interface {
	Capabilities() invoicedomain.Capabilities
	Issue(ctx context.Context, invoice invoicedomain.Record) (invoicedomain.IssueResult, error)
	Recover(ctx context.Context, invoice invoicedomain.Record) (invoicedomain.Recovery, error)
	Reconcile(ctx context.Context, invoice invoicedomain.Record) ([]invoicedomain.Observation, error)
}

type InvoiceRuntimeRegistry interface {
	ForConnection(connectionID string) (invoicedomain.Connection, InvoiceProvider, bool)
}

type InvoiceNotificationUseCase interface {
	AcceptInvoiceNotification(ctx context.Context, connection invoicedomain.Connection, notification VerifiedInvoiceNotification) error
}
type VerifiedInvoiceNotification struct {
	EventKey         string
	RawBodyHash      [32]byte
	Observation      invoicedomain.Observation
	VerificationKind string
}
type InvoiceNotificationEnvelope struct {
	Body        []byte
	ContentType string
	Headers     map[string][]string
}
type InvoiceAcknowledgement struct {
	Status      int
	ContentType string
	Body        []byte
}
type InvoiceNotificationDecoder interface {
	VerifyAndDecode(envelope InvoiceNotificationEnvelope) (VerifiedInvoiceNotification, error)
	Acknowledgement() InvoiceAcknowledgement
}
type InvoiceNotificationDecoderRegistry interface {
	DecoderForConnection(connectionID string) (invoicedomain.Connection, InvoiceNotificationDecoder, bool)
}

var ErrInvoiceNotificationSignature = errors.New("invalid invoice notification signature")
var ErrInvoiceNotificationMalformed = errors.New("malformed invoice notification")
