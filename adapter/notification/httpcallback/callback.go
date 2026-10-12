package httpcallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

// Sender отправляет HTTP callback мерчанту.
type Sender struct {
	client *http.Client
	now    func() time.Time
}

var _ ports.CallbackSender = (*Sender)(nil)

// New создает HTTP callback sender с заданным timeout.
func New(timeout time.Duration) *Sender {
	return &Sender{
		client: &http.Client{Timeout: timeout},
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// NewWithClient создает HTTP callback sender с пользовательским HTTP client.
func NewWithClient(client *http.Client, now func() time.Time) *Sender {
	return &Sender{client: client, now: now}
}

// Send отправляет подписанное уведомление о платеже на callback_url мерчанта.
func (s *Sender) Send(ctx context.Context, merchant merchantdomain.Merchant, payment paymentdomain.Payment, event outboxdomain.Event) error {
	if merchant.CallbackURL == "" {
		return errors.New("merchant callback url is empty")
	}
	var discriminator struct {
		SchemaVersion json.RawMessage `json:"schema_version"`
		EventType     string          `json:"event_type"`
	}
	// A damaged invoice event must never be reinterpreted as acquiring.
	_ = json.Unmarshal(event.Payload, &discriminator)
	if event.EventType == "payment.invoice.updated" || discriminator.SchemaVersion != nil || discriminator.EventType == "payment.invoice.updated" {
		snapshot, err := decodeInvoiceSnapshot(event.Payload)
		if err != nil {
			return fmt.Errorf("decode outbox invoice snapshot: %w", err)
		}
		if event.AggregateType != "payment" || event.ID == uuid.Nil || event.AggregateID == uuid.Nil ||
			snapshot.EventID != event.ID || snapshot.EventType != event.EventType ||
			snapshot.MerchantID != merchant.ID || snapshot.MerchantID != payment.MerchantID || snapshot.PaymentID != event.AggregateID ||
			snapshot.PaymentID != payment.ID {
			return errors.New("outbox invoice snapshot does not match event")
		}
		return s.sendPayload(ctx, merchant, event.Payload)
	}
	payload := map[string]any{
		"event_id":     event.ID,
		"event_type":   event.EventType,
		"payment_id":   payment.ID,
		"order_id":     payment.MerchantOrderID,
		"status":       payment.Status,
		"amount_minor": payment.AmountMinor,
		"currency":     payment.Currency,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return s.sendPayload(ctx, merchant, body)
}

func (s *Sender) sendPayload(ctx context.Context, merchant merchantdomain.Merchant, body []byte) error {
	timestamp := fmt.Sprintf("%d", s.now().Unix())
	signature := crypto.HMACSHA256Hex(merchant.SharedSecret, []byte(timestamp+"."+string(body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, merchant.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", signature)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback returned status %d", resp.StatusCode)
	}
	return nil
}

func decodeInvoiceSnapshot(body []byte) (invoicedomain.CallbackEnvelope, error) {
	var snapshot invoicedomain.CallbackEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return snapshot, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return snapshot, err
	}
	required := []string{"schema_version", "event_id", "event_type", "merchant_id", "payment_id", "order_id", "payment_method", "revision", "reason", "occurred_at", "status", "issuance_state", "funding_state", "amount_minor", "credited_minor", "currency", "confirmed_paid", "requires_review", "review_reasons", "accept_until", "payment_confirmed_at", "confirmation_recorded_at"}
	for _, name := range required {
		value, ok := fields[name]
		nullable := name == "payment_confirmed_at" || name == "confirmation_recorded_at"
		if !ok || (!nullable && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return snapshot, errors.New("incomplete invoice snapshot")
		}
	}
	if snapshot.SchemaVersion != "invoice-integration/1" || snapshot.EventType != "payment.invoice.updated" ||
		snapshot.PaymentMethod != "legal_entity_invoice" || snapshot.MerchantID == uuid.Nil || snapshot.OrderID == "" ||
		snapshot.Revision <= 0 || snapshot.OccurredAt.IsZero() || snapshot.AcceptUntil.IsZero() ||
		snapshot.AmountMinor <= 0 || snapshot.AmountMinor > invoicedomain.MaxAmountMinor || snapshot.CreditedMinor < 0 || snapshot.Currency != "RUB" ||
		snapshot.ConfirmedPaid != (snapshot.CreditedMinor >= snapshot.AmountMinor) || snapshot.RequiresReview != (len(snapshot.ReviewReasons) > 0) {
		return snapshot, errors.New("invalid invoice snapshot")
	}
	if !oneOf(snapshot.Status, "creating", "pending", "succeeded", "failed", "canceled") ||
		!oneOf(snapshot.IssuanceState, "queued", "dispatched", "issued", "unknown", "rejected", "expired_before_issue") ||
		!oneOf(snapshot.FundingState, "none", "partial", "paid", "overpaid") ||
		!oneOf(snapshot.Reason, "created", "issued", "receipt_recorded", "review_required", "issue_failed", "deadline_elapsed") {
		return snapshot, errors.New("invalid invoice snapshot state")
	}
	return snapshot, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
