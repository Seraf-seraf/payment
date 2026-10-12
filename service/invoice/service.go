package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

var (
	ErrInvalidRequest        = errors.New("invalid invoice request")
	ErrIdempotencyConflict   = errors.New("invoice idempotency conflict")
	ErrUnsupportedTerms      = errors.New("unsupported invoice terms")
	ErrConnectionUnavailable = errors.New("invoice connection unavailable")
	innPattern               = regexp.MustCompile(`^[0-9]{10}$`)
	kppPattern               = regexp.MustCompile(`^[0-9]{9}$`)
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
	if err := normalizeAndValidate(&req); err != nil {
		return ports.InvoiceCreateResult{}, err
	}
	hash, err := requestHash(req)
	if err != nil {
		return ports.InvoiceCreateResult{}, err
	}
	if existing, err := s.invoices.FindByMerchantAndRequestID(ctx, merchant.ID, req.RequestID); err == nil {
		return replay(existing, hash)
	} else if !errors.Is(err, paymentservice.ErrNotFound) {
		return ports.InvoiceCreateResult{}, err
	}

	now := s.now().UTC()
	if !req.AcceptUntil.After(now) {
		return ports.InvoiceCreateResult{}, ErrInvalidRequest
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
				return replay(existing, hash)
			}
			if !errors.Is(readErr, paymentservice.ErrNotFound) {
				return ports.InvoiceCreateResult{}, errors.Join(err, readErr)
			}
			return ports.InvoiceCreateResult{}, ErrIdempotencyConflict
		}
		return ports.InvoiceCreateResult{}, err
	}
	return ports.InvoiceCreateResult{Invoice: created, Created: true}, nil
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

func replay(existing invoicedomain.Record, hash [32]byte) (ports.InvoiceCreateResult, error) {
	if existing.RequestHash != hash {
		return ports.InvoiceCreateResult{}, ErrIdempotencyConflict
	}
	return ports.InvoiceCreateResult{Invoice: existing, Created: false}, nil
}

func requestHash(req invoicedomain.Request) ([32]byte, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

func normalizeAndValidate(req *invoicedomain.Request) error {
	req.OrderID = strings.TrimSpace(req.OrderID)
	req.Currency = strings.TrimSpace(req.Currency)
	req.Description = strings.TrimSpace(req.Description)
	req.Payer.Kind = strings.TrimSpace(req.Payer.Kind)
	req.Payer.Name = strings.TrimSpace(req.Payer.Name)
	req.Payer.INN = strings.TrimSpace(req.Payer.INN)
	req.Payer.KPP = strings.TrimSpace(req.Payer.KPP)
	req.Payer.LegalAddress = strings.TrimSpace(req.Payer.LegalAddress)
	req.Payer.Email = strings.TrimSpace(req.Payer.Email)
	req.AcceptUntil = req.AcceptUntil.UTC()
	if req.RequestID == uuid.Nil || textLength(req.OrderID) < 1 || textLength(req.OrderID) > 100 ||
		req.AmountMinor < 1 || req.AmountMinor > invoicedomain.MaxAmountMinor || req.Currency != "RUB" ||
		textLength(req.Description) < 1 || textLength(req.Description) > 512 || req.Payer.Kind != "company" || textLength(req.Payer.Name) < 1 || textLength(req.Payer.Name) > 512 ||
		!innPattern.MatchString(req.Payer.INN) || !kppPattern.MatchString(req.Payer.KPP) ||
		textLength(req.Payer.LegalAddress) < 1 || textLength(req.Payer.LegalAddress) > 1000 || textLength(req.Payer.Email) > 254 ||
		len(req.Lines) < 1 || len(req.Lines) > 100 || req.AcceptUntil.IsZero() {
		return ErrInvalidRequest
	}
	if req.Payer.Email != "" {
		address, err := mail.ParseAddress(req.Payer.Email)
		if err != nil || address.Address != req.Payer.Email {
			return ErrInvalidRequest
		}
	}

	lineIDs := make(map[string]struct{}, len(req.Lines))
	var total int64
	for i := range req.Lines {
		line := &req.Lines[i]
		line.ID = strings.TrimSpace(line.ID)
		line.Name = strings.TrimSpace(line.Name)
		line.Unit = strings.TrimSpace(line.Unit)
		if textLength(line.ID) < 1 || textLength(line.ID) > 100 || textLength(line.Name) < 1 || textLength(line.Name) > 512 ||
			line.Quantity < 1 || line.Quantity > 1_000_000 || line.UnitPriceMinor < 1 ||
			line.UnitPriceMinor > invoicedomain.MaxAmountMinor || line.Quantity > math.MaxInt64/line.UnitPriceMinor ||
			line.AmountMinor != line.UnitPriceMinor*line.Quantity || line.AmountMinor > invoicedomain.MaxAmountMinor {
			return ErrInvalidRequest
		}
		switch line.Unit {
		case "piece", "set", "service":
		default:
			return ErrInvalidRequest
		}
		if _, exists := lineIDs[line.ID]; exists {
			return ErrInvalidRequest
		}
		lineIDs[line.ID] = struct{}{}
		line.VAT.Kind = strings.TrimSpace(line.VAT.Kind)
		switch line.VAT.Kind {
		case "none":
			if line.VAT.RateBasisPoints != nil || line.VAT.AmountMinor != 0 {
				return ErrInvalidRequest
			}
		case "included":
			if line.VAT.RateBasisPoints == nil || *line.VAT.RateBasisPoints < 0 || *line.VAT.RateBasisPoints > 10_000 ||
				line.VAT.AmountMinor < 0 || line.VAT.AmountMinor > line.AmountMinor {
				return ErrInvalidRequest
			}
		default:
			return ErrInvalidRequest
		}
		if total > invoicedomain.MaxAmountMinor-line.AmountMinor {
			return ErrInvalidRequest
		}
		total += line.AmountMinor
	}
	if total != req.AmountMinor {
		return ErrInvalidRequest
	}
	return nil
}

func textLength(value string) int { return utf8.RuneCountInString(value) }

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
