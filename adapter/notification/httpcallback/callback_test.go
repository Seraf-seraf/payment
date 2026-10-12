package httpcallback

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/google/uuid"
)

func TestSendSignsCallback(t *testing.T) {
	t.Parallel()

	var gotSignature string
	var gotBody []byte
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotSignature = req.Header.Get("X-Signature")
		var err error
		gotBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	sender := NewWithClient(client, now)
	merchant := merchantdomain.Merchant{CallbackURL: "https://merchant.test/callback", SharedSecret: "secret"}
	payment := paymentdomain.Payment{ID: uuid.New(), MerchantOrderID: "order-1", Status: paymentdomain.StatusSucceeded, AmountMinor: 1000, Currency: "RUB"}

	if err := sender.Send(t.Context(), merchant, payment, outboxdomain.Event{ID: uuid.New(), EventType: "payment.succeeded"}); err != nil {
		t.Fatal(err)
	}
	expected := crypto.HMACSHA256Hex("secret", []byte("100."+string(gotBody)))
	if gotSignature != expected {
		t.Fatalf("signature = %q, want %q", gotSignature, expected)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSendInvoiceUsesImmutablePayloadSnapshot(t *testing.T) {
	t.Parallel()

	merchantID := uuid.New()
	paymentID := uuid.New()
	eventID := uuid.New()
	const timestamp = "100"
	body, err := json.Marshal(invoiceSnapshot(eventID, merchantID, paymentID))
	if err != nil {
		t.Fatal(err)
	}

	var gotBody []byte
	var gotSignature string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var err error
		gotBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		gotSignature = req.Header.Get("X-Signature")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})}
	sender := NewWithClient(client, func() time.Time { return time.Unix(100, 0) })
	merchant := merchantdomain.Merchant{ID: merchantID, CallbackURL: "https://merchant.test/callback", SharedSecret: "secret"}
	payment := paymentdomain.Payment{ID: paymentID, MerchantID: merchantID, Status: paymentdomain.StatusSucceeded}
	event := outboxdomain.Event{ID: eventID, AggregateType: "payment", AggregateID: paymentID, EventType: "payment.invoice.updated", Payload: body}
	if err := sender.Send(t.Context(), merchant, payment, event); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("callback body changed: got %s, want %s", gotBody, body)
	}
	if want := crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+string(body))); gotSignature != want {
		t.Fatalf("signature = %q, want %q", gotSignature, want)
	}
}

func invoiceSnapshot(eventID, merchantID, paymentID uuid.UUID) invoicedomain.CallbackEnvelope {
	return invoicedomain.CallbackEnvelope{
		SchemaVersion: "invoice-integration/1", EventID: eventID, EventType: "payment.invoice.updated", MerchantID: merchantID, PaymentID: paymentID,
		OrderID: "order-1", PaymentMethod: "legal_entity_invoice", Revision: 1, Reason: "created", OccurredAt: time.Unix(100, 0).UTC(),
		Status: "creating", IssuanceState: "queued", FundingState: "none", AmountMinor: 1000, Currency: "RUB", ReviewReasons: []string{}, AcceptUntil: time.Unix(1000, 0).UTC(),
	}
}

func TestSendInvoiceRejectsCorruptSnapshotWithoutHTTP(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(map[string]any, *outboxdomain.Event)
	}{
		{"missing amount", func(p map[string]any, _ *outboxdomain.Event) { delete(p, "amount_minor") }},
		{"missing false boolean", func(p map[string]any, _ *outboxdomain.Event) { delete(p, "confirmed_paid") }},
		{"missing nullable timestamp", func(p map[string]any, _ *outboxdomain.Event) { delete(p, "payment_confirmed_at") }},
		{"null boolean", func(p map[string]any, _ *outboxdomain.Event) { p["confirmed_paid"] = nil }},
		{"wrong schema", func(p map[string]any, _ *outboxdomain.Event) { p["schema_version"] = "invoice-integration/2" }},
		{"wrong event", func(p map[string]any, _ *outboxdomain.Event) { p["event_id"] = uuid.New().String() }},
		{"wrong payment", func(p map[string]any, _ *outboxdomain.Event) { p["payment_id"] = uuid.New().String() }},
		{"wrong merchant", func(p map[string]any, _ *outboxdomain.Event) { p["merchant_id"] = uuid.New().String() }},
		{"wrong aggregate", func(_ map[string]any, e *outboxdomain.Event) { e.AggregateType = "invoice_command" }},
		{"invalid status", func(p map[string]any, _ *outboxdomain.Event) { p["status"] = "paid" }},
		{"unknown field", func(p map[string]any, _ *outboxdomain.Event) { p["secret"] = "forbidden" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.test/callback"}
			payment := paymentdomain.Payment{ID: uuid.New(), MerchantID: merchant.ID, Status: paymentdomain.StatusSucceeded}
			event := outboxdomain.Event{ID: uuid.New(), AggregateType: "payment", AggregateID: payment.ID, EventType: "payment.invoice.updated"}
			body, _ := json.Marshal(invoiceSnapshot(event.ID, merchant.ID, payment.ID))
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			test.corrupt(payload, &event)
			event.Payload, _ = json.Marshal(payload)
			calls := 0
			sender := NewWithClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })}, time.Now)
			if err := sender.Send(t.Context(), merchant, payment, event); err == nil {
				t.Fatal("corrupt snapshot accepted")
			}
			if calls != 0 {
				t.Fatal("corrupt snapshot sent over HTTP")
			}
		})
	}
}

func TestSendInvoiceRejectsConfirmedSnapshotWithoutRecordedAt(t *testing.T) {
	merchantID, paymentID, eventID := uuid.New(), uuid.New(), uuid.New()
	snapshot := invoiceSnapshot(eventID, merchantID, paymentID)
	snapshot.Status = "succeeded"
	snapshot.FundingState = "paid"
	snapshot.CreditedMinor = snapshot.AmountMinor
	snapshot.ConfirmedPaid = true
	snapshot.ConfirmationRecordedAt = nil
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	sender := NewWithClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}, time.Now)
	merchant := merchantdomain.Merchant{ID: merchantID, CallbackURL: "https://merchant.test/callback"}
	payment := paymentdomain.Payment{ID: paymentID, MerchantID: merchantID}
	event := outboxdomain.Event{ID: eventID, AggregateType: "payment", AggregateID: paymentID, EventType: snapshot.EventType, Payload: body}
	if err := sender.Send(t.Context(), merchant, payment, event); err == nil {
		t.Fatal("confirmed snapshot without recorded timestamp accepted")
	}
	if calls != 0 {
		t.Fatal("invalid financial snapshot sent over HTTP")
	}
}

func TestSendInvoiceRetryPreservesBodyAndRefreshesSignature(t *testing.T) {
	merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.test/callback", SharedSecret: "secret"}
	payment := paymentdomain.Payment{ID: uuid.New(), MerchantID: merchant.ID, Status: paymentdomain.StatusSucceeded}
	event := outboxdomain.Event{ID: uuid.New(), AggregateType: "payment", AggregateID: payment.ID, EventType: "payment.invoice.updated"}
	event.Payload, _ = json.Marshal(invoiceSnapshot(event.ID, merchant.ID, payment.ID))
	now := time.Unix(100, 0)
	calls := 0
	sender := NewWithClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, event.Payload) {
			t.Fatal("retry changed snapshot")
		}
		timestamp := req.Header.Get("X-Timestamp")
		if got := req.Header.Get("X-Signature"); got != crypto.HMACSHA256Hex(merchant.SharedSecret, []byte(timestamp+"."+string(body))) {
			t.Fatal("invalid signature")
		}
		if calls == 1 && timestamp != "100" || calls == 2 && timestamp != "101" {
			t.Fatal("timestamp not refreshed")
		}
		status := http.StatusNoContent
		if calls == 1 {
			status = http.StatusServiceUnavailable
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}, func() time.Time { return now })
	if err := sender.Send(t.Context(), merchant, payment, event); err == nil {
		t.Fatal("503 accepted")
	}
	now = now.Add(time.Second)
	if err := sender.Send(t.Context(), merchant, payment, event); err != nil {
		t.Fatal(err)
	}
}

func TestSendInvoiceWithLegacyEventTypeDoesNotFallBack(t *testing.T) {
	merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.test/callback"}
	payment := paymentdomain.Payment{ID: uuid.New(), MerchantID: merchant.ID, MerchantOrderID: "order-1", AmountMinor: 1000, Currency: "RUB", Status: paymentdomain.StatusPending}
	event := outboxdomain.Event{ID: uuid.New(), AggregateType: "payment", AggregateID: payment.ID, EventType: "payment.succeeded"}
	snapshot := invoiceSnapshot(event.ID, merchant.ID, payment.ID)
	snapshot.Status = "pending"
	event.Payload, _ = json.Marshal(snapshot)
	calls := 0
	sender := NewWithClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}, time.Now)
	if err := sender.Send(t.Context(), merchant, payment, event); err == nil {
		t.Fatal("invoice payload incorrectly delivered as legacy callback")
	}
	if calls != 0 {
		t.Fatal("invoice with mismatched event type sent")
	}
}
