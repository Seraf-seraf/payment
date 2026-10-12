package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

var (
	ErrInvalidRequest        = invoicedomain.ErrInvalidRequest
	ErrIdempotencyConflict   = errors.New("invoice idempotency conflict")
	ErrUnsupportedTerms      = errors.New("unsupported invoice terms")
	ErrConnectionUnavailable = errors.New("invoice connection unavailable")
	ErrNotFound              = paymentservice.ErrNotFound
)

const (
	issuanceQueued = "queued"
	fundingNone    = "none"
	eventType      = "payment.invoice.updated"
)

type Service struct {
	invoices ports.InvoiceRepository
	registry ports.InvoiceProviderRegistry
	tx       ports.TransactionManager
	now      func() time.Time
}

var _ ports.InvoiceUseCase = (*Service)(nil)

func NewService(invoices ports.InvoiceRepository, registry ports.InvoiceProviderRegistry, tx ports.TransactionManager, now func() time.Time) *Service {
	return &Service{invoices: invoices, registry: registry, tx: tx, now: now}
}

func (s *Service) CreateInvoicePayment(ctx context.Context, req invoicedomain.Request, merchant merchantdomain.Merchant) (ports.InvoiceCreateResult, error) {
	if s.invoices == nil || s.registry == nil || s.tx == nil || s.now == nil {
		return ports.InvoiceCreateResult{}, errors.New("invoice service is not configured")
	}
	req = invoicedomain.NormalizeRequest(req)
	if err := invoicedomain.ValidateRequest(req); err != nil {
		return ports.InvoiceCreateResult{}, err
	}
	hash, err := invoicedomain.RequestHash(req)
	if err != nil {
		return ports.InvoiceCreateResult{}, err
	}
	if existing, err := s.invoices.FindByMerchantAndRequestID(ctx, merchant.ID, req.RequestID); err == nil {
		return s.replay(ctx, existing, hash, merchant.ID)
	} else if !errors.Is(err, paymentservice.ErrNotFound) {
		return ports.InvoiceCreateResult{}, err
	}

	now := s.now().UTC()
	if err := invoicedomain.ValidateNewDeadline(req.AcceptUntil, now); err != nil {
		return ports.InvoiceCreateResult{}, err
	}
	connection, ok := s.registry.ForMerchant(merchant.ID)
	if !ok || connection.MerchantID != merchant.ID || connection.ID == "" || merchant.CallbackURL == "" {
		return ports.InvoiceCreateResult{}, ErrConnectionUnavailable
	}
	if connection.ProviderKind != "mock_invoice" || connection.EvidenceMode != "invoice_confirmation" && connection.EvidenceMode != "bank_credits" || req.Currency != "RUB" {
		return ports.InvoiceCreateResult{}, ErrUnsupportedTerms
	}
	if !supportsAcceptDeadline(now, req.AcceptUntil, connection.Capabilities.MaxAcceptDuration) {
		return ports.InvoiceCreateResult{}, ErrUnsupportedTerms
	}
	if !supportsTerms(connection.Capabilities, req.Lines) {
		return ports.InvoiceCreateResult{}, ErrUnsupportedTerms
	}
	if err := validateCallbackURL(merchant.CallbackURL); err != nil {
		return ports.InvoiceCreateResult{}, ErrConnectionUnavailable
	}

	paymentID := uuid.New()
	reference := "INV-" + strings.ToUpper(strings.ReplaceAll(paymentID.String(), "-", ""))
	key := "invoice:" + req.RequestID.String()
	parent := paymentdomain.Payment{
		ID: paymentID, MerchantID: merchant.ID,
		ProviderName:    "invoice:" + connection.ID,
		MerchantOrderID: req.OrderID, IdempotencyKey: key,
		AmountMinor: req.AmountMinor, Currency: req.Currency,
		Status: paymentdomain.StatusCreating, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}
	record := invoicedomain.Record{
		PaymentID: paymentID, MerchantID: merchant.ID, CreatedAt: now, Request: req, RequestHash: hash,
		Connection: connection.Snapshot(), Reference: reference,
		EvidenceMode: connection.EvidenceMode, IssuanceState: issuanceQueued,
		FundingState: fundingNone, ReviewReasons: []string{}, Revision: 1,
	}
	eventID := uuid.New()
	var created invoicedomain.Record
	err = s.tx.WithinTx(ctx, func(txCtx context.Context, repos ports.Repositories) error {
		if repos.Payments == nil || repos.Invoices == nil || repos.InvoiceCommands == nil || repos.Outbox == nil {
			return errors.New("invoice transaction repositories are incomplete")
		}
		if err := repos.Payments.Create(txCtx, parent); err != nil {
			return err
		}
		created, err = repos.Invoices.Create(txCtx, record)
		if err != nil {
			return err
		}
		if err := repos.InvoiceCommands.Create(txCtx, ports.InvoiceCommand{
			ID: uuid.New(), CommandType: "invoice.issue", PaymentID: paymentID,
			Status: "pending", NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		payload, err := createdEnvelope(eventID, parent, created, now)
		if err != nil {
			return err
		}
		return repos.Outbox.Create(txCtx, outboxdomain.Event{
			ID: eventID, AggregateType: "payment", AggregateID: paymentID,
			EventType: eventType, Payload: payload, Status: outboxdomain.StatusPending,
			NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		if errors.Is(err, paymentservice.ErrAlreadyExists) {
			existing, readErr := s.invoices.FindByMerchantAndRequestID(ctx, merchant.ID, req.RequestID)
			if readErr == nil {
				return s.replay(ctx, existing, hash, merchant.ID)
			}
			if !errors.Is(readErr, paymentservice.ErrNotFound) {
				return ports.InvoiceCreateResult{}, errors.Join(err, readErr)
			}
			return ports.InvoiceCreateResult{}, ErrIdempotencyConflict
		}
		return ports.InvoiceCreateResult{}, err
	}
	return ports.InvoiceCreateResult{Invoice: visibleRecord(created, now), Status: parent.Status, Created: true}, nil
}

func (s *Service) GetInvoicePayment(ctx context.Context, id uuid.UUID, merchant merchantdomain.Merchant) (ports.InvoiceGetResult, error) {
	if s.invoices == nil || s.tx == nil || s.now == nil {
		return ports.InvoiceGetResult{}, errors.New("invoice service is not configured")
	}
	var result ports.InvoiceGetResult
	err := s.tx.WithinTx(ctx, func(txCtx context.Context, repos ports.Repositories) error {
		if repos.Invoices == nil {
			return errors.New("invoice transaction repository is incomplete")
		}
		record, err := repos.Invoices.LockByMerchantAndPaymentID(txCtx, merchant.ID, id)
		if err != nil {
			return err
		}
		payment, err := repos.Invoices.LockPayment(txCtx, id)
		if err != nil {
			return err
		}
		if payment.MerchantID != merchant.ID {
			return ErrNotFound
		}
		result = ports.InvoiceGetResult{Invoice: visibleRecord(record, s.now().UTC()), Status: payment.Status}
		return nil
	})
	if err != nil {
		return ports.InvoiceGetResult{}, err
	}
	return result, nil
}

func (s *Service) replay(ctx context.Context, existing invoicedomain.Record, hash [32]byte, merchantID uuid.UUID) (ports.InvoiceCreateResult, error) {
	if existing.RequestHash != hash {
		return ports.InvoiceCreateResult{}, ErrIdempotencyConflict
	}
	projection, err := s.GetInvoicePayment(ctx, existing.PaymentID, merchantdomain.Merchant{ID: merchantID})
	if err != nil {
		return ports.InvoiceCreateResult{}, err
	}
	return ports.InvoiceCreateResult{Invoice: projection.Invoice, Status: projection.Status}, nil
}

func visibleRecord(record invoicedomain.Record, now time.Time) invoicedomain.Record {
	if record.Artifact == nil {
		return record
	}
	artifact := *record.Artifact
	if artifact.NextAction != nil {
		action := *artifact.NextAction
		if (action.ValidUntil != nil && !action.ValidUntil.After(now)) || (action.Type == "redirect" && action.ValidUntil == nil) {
			artifact.NextAction = nil
		} else {
			artifact.NextAction = &action
		}
	}
	artifact.Documents = make([]invoicedomain.Document, 0, len(record.Artifact.Documents))
	for _, document := range record.Artifact.Documents {
		if document.ValidUntil != nil && !document.ValidUntil.After(now) {
			continue
		}
		artifact.Documents = append(artifact.Documents, document)
	}
	record.Artifact = &artifact
	return record
}

func supportsAcceptDeadline(now, acceptUntil time.Time, maxDuration time.Duration) bool {
	return !acceptUntil.After(now.Add(maxDuration))
}

func supportsTerms(capabilities invoicedomain.Capabilities, lines []invoicedomain.Line) bool {
	for _, line := range lines {
		unitSupported := false
		for _, unit := range capabilities.SupportedUnits {
			if unit == line.Unit {
				unitSupported = true
				break
			}
		}
		if !unitSupported {
			return false
		}
		if line.VAT.Kind == "none" {
			if !capabilities.SupportsVATNone {
				return false
			}
			continue
		}
		rateSupported := false
		for _, rate := range capabilities.SupportedVATRatesBPS {
			if line.VAT.RateBasisPoints != nil && rate == *line.VAT.RateBasisPoints {
				rateSupported = true
				break
			}
		}
		if !rateSupported {
			return false
		}
	}
	return true
}

func validateCallbackURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return ErrInvalidRequest
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme != "http" {
		return ErrInvalidRequest
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	return ErrInvalidRequest
}

func createdEnvelope(eventID uuid.UUID, payment paymentdomain.Payment, record invoicedomain.Record, now time.Time) (json.RawMessage, error) {
	payload, err := json.Marshal(invoicedomain.CallbackEnvelope{
		SchemaVersion: "invoice-integration/1", EventID: eventID, EventType: eventType,
		MerchantID: payment.MerchantID, PaymentID: payment.ID, OrderID: payment.MerchantOrderID,
		PaymentMethod: "legal_entity_invoice", Revision: 1, Reason: "created", OccurredAt: now.UTC(),
		Status: string(paymentdomain.StatusCreating), IssuanceState: record.IssuanceState,
		FundingState: record.FundingState, AmountMinor: payment.AmountMinor, CreditedMinor: 0,
		Currency: payment.Currency, ConfirmedPaid: false, RequiresReview: false,
		ReviewReasons: []string{}, AcceptUntil: record.Request.AcceptUntil.UTC(),
	})
	return payload, err
}
