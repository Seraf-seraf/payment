package cloudpayments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

const webhookPaymentID = "11111111-1111-4111-8111-111111111111"

type testWebhookEnvelope struct {
	ContentType string `json:"content_type"`
	RawBody     []byte `json:"raw_body"`
}

func webhookEnvelopeJSON(t *testing.T, contentType string, body []byte) []byte {
	t.Helper()
	encoded, err := json.Marshal(testWebhookEnvelope{ContentType: contentType, RawBody: body})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func webhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type webhookPaymentReader struct {
	ports.PaymentRepository
	payment paymentdomain.Payment
	err     error
	calls   int
	gotID   uuid.UUID
	gotCtx  context.Context
	writes  int
}

func (r *webhookPaymentReader) FindByID(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error) {
	r.calls++
	r.gotID = id
	r.gotCtx = ctx
	return r.payment, r.err
}

func (r *webhookPaymentReader) Create(context.Context, paymentdomain.Payment) error {
	r.writes++
	return nil
}

func (r *webhookPaymentReader) UpdateProviderData(context.Context, uuid.UUID, string, string) error {
	r.writes++
	return nil
}

func (r *webhookPaymentReader) UpdateProviderDataAndStatus(context.Context, uuid.UUID, string, string, paymentdomain.Status, []paymentdomain.Status) error {
	r.writes++
	return nil
}

func (r *webhookPaymentReader) UpdateStatus(context.Context, uuid.UUID, paymentdomain.Status) error {
	r.writes++
	return nil
}

func (r *webhookPaymentReader) UpdateStatusIfAllowed(context.Context, uuid.UUID, paymentdomain.Status, []paymentdomain.Status) error {
	r.writes++
	return nil
}

func newWebhookTestProvider(t *testing.T, reader *webhookPaymentReader, testMode bool) *Provider {
	t.Helper()
	provider, err := New(Options{
		APIURL: "https://api.example.test", PublicID: "public-id", APISecret: "test-secret",
		TestMode: testMode, Timeout: time.Second, Payments: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func readyWebhookPayment() paymentdomain.Payment {
	return paymentdomain.Payment{
		ID: uuid.MustParse(webhookPaymentID), ProviderName: "cloudpayments", ProviderPaymentID: webhookPaymentID,
		AmountMinor: 1001, Currency: "RUB", Status: paymentdomain.StatusPending,
	}
}

func payJSON(transactionID string) []byte {
	return []byte(fmt.Sprintf(`{"InvoiceId":%q,"TransactionId":%s,"Amount":10.01,"Currency":"RUB","TestMode":true,"OperationType":"Payment","Status":"Completed"}`, webhookPaymentID, transactionID))
}

func payForm(transactionID string) []byte {
	values := url.Values{
		"InvoiceId": {webhookPaymentID}, "TransactionId": {transactionID}, "Amount": {"10.01"},
		"Currency": {"RUB"}, "TestMode": {"1"}, "OperationType": {"Payment"}, "Status": {"Completed"},
	}
	return []byte(values.Encode())
}

func TestCloudPaymentsWebhookVerifiesContentHMACBeforeReader(t *testing.T) {
	body := []byte("InvoiceId=" + webhookPaymentID + "&Amount=10.01")
	reader := &webhookPaymentReader{payment: readyWebhookPayment()}
	provider := newWebhookTestProvider(t, reader, true)
	valid := webhookSignature("test-secret", body)
	for _, tc := range []struct {
		name       string
		values     []string
		mutateBody bool
		wantValid  bool
	}{
		{"valid", []string{valid}, false, true},
		{"missing", nil, false, false},
		{"empty", []string{""}, false, false},
		{"invalid base64", []string{"not base64!"}, false, false},
		{"wrong signature", []string{webhookSignature("wrong-secret", body)}, false, false},
		{"duplicate header", []string{valid, valid}, false, false},
		{"one changed byte", []string{valid}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			for _, value := range tc.values {
				headers.Add("Content-HMAC", value)
			}
			input := body
			if tc.mutateBody {
				input = append(append([]byte(nil), body...), 'x')
			}
			err := provider.VerifyWebhook(t.Context(), headers, webhookEnvelopeJSON(t, "application/x-www-form-urlencoded", input))
			if tc.wantValid && err != nil {
				t.Fatalf("VerifyWebhook error = %v", err)
			}
			if !tc.wantValid && !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("VerifyWebhook error = %v, want ErrInvalidSignature", err)
			}
			if reader.calls != 0 {
				t.Fatalf("invalid signature caused %d payment reads", reader.calls)
			}
		})
	}
}

func TestCloudPaymentsWebhookUsesExactRawFormBytesForSignature(t *testing.T) {
	body := []byte("InvoiceId=" + webhookPaymentID + "&Description=%D0%9E%D0%BF%D0%BB%D0%B0%D1%82%D0%B0+%2B+%26&Amount=10.01")
	provider := newWebhookTestProvider(t, &webhookPaymentReader{payment: readyWebhookPayment()}, true)
	headers := http.Header{"Content-Hmac": []string{webhookSignature("test-secret", body)}}
	if err := provider.VerifyWebhook(t.Context(), headers, webhookEnvelopeJSON(t, "application/x-www-form-urlencoded", body)); err != nil {
		t.Fatalf("VerifyWebhook error = %v", err)
	}
}

func TestCloudPaymentsWebhookNormalizesPayAndFailWithoutFloatIDs(t *testing.T) {
	const transactionID = "9007199254740993"
	reader := &webhookPaymentReader{payment: readyWebhookPayment()}
	provider := newWebhookTestProvider(t, reader, true)
	for _, tc := range []struct {
		name      string
		body      []byte
		content   string
		eventID   string
		status    paymentdomain.Status
		eventType string
	}{
		{"JSON Pay", payJSON(transactionID), "application/json", "pay:" + transactionID, paymentdomain.StatusSucceeded, "payment.succeeded"},
		{"form Pay", payForm(transactionID), "application/x-www-form-urlencoded", "pay:" + transactionID, paymentdomain.StatusSucceeded, "payment.succeeded"},
		{"Fail", []byte(fmt.Sprintf(`{"InvoiceId":%q,"TransactionId":42,"Amount":10.01,"Currency":"RUB","TestMode":true,"OperationType":"Payment","ReasonCode":13}`, webhookPaymentID)), "application/json", "fail:42", paymentdomain.StatusPending, "payment.pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader.calls = 0
			event, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, tc.content, tc.body))
			if err != nil {
				t.Fatalf("ParseWebhook error = %v", err)
			}
			if event.ProviderEventID != tc.eventID || event.ProviderPaymentID != webhookPaymentID || event.Status != tc.status || event.EventType != tc.eventType {
				t.Errorf("event = %+v", event)
			}
			if reader.calls != 1 || reader.gotID != uuid.MustParse(webhookPaymentID) {
				t.Errorf("reader calls/id = %d/%s", reader.calls, reader.gotID)
			}
		})
	}
}

func TestCloudPaymentsWebhookRejectsUnsupportedOrMismatchedEvents(t *testing.T) {
	valid := func(fields string) []byte {
		return []byte(fmt.Sprintf(`{"InvoiceId":%q,%s}`, webhookPaymentID, fields))
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		mode    bool
		payment paymentdomain.Payment
		want    error
	}{
		{"authorized", valid(`"TransactionId":1,"Amount":10.01,"Currency":"RUB","TestMode":true,"OperationType":"Payment","Status":"Authorized"`), true, readyWebhookPayment(), ErrInvalidNotification},
		{"refund operation", valid(`"TransactionId":1,"Amount":10.01,"Currency":"RUB","TestMode":true,"OperationType":"Refund","Status":"Completed"`), true, readyWebhookPayment(), ErrInvalidNotification},
		{"lower amount", valid(`"TransactionId":1,"Amount":10.00,"Currency":"RUB","TestMode":true,"OperationType":"Payment","Status":"Completed"`), true, readyWebhookPayment(), ErrPaymentMismatch},
		{"wrong amount", valid(`"TransactionId":1,"Amount":10.02,"Currency":"RUB","TestMode":true,"OperationType":"Payment","Status":"Completed"`), true, readyWebhookPayment(), ErrPaymentMismatch},
		{"wrong currency", valid(`"TransactionId":1,"Amount":10.01,"Currency":"USD","TestMode":true,"OperationType":"Payment","Status":"Completed"`), true, readyWebhookPayment(), ErrPaymentMismatch},
		{"wrong mode", valid(`"TransactionId":1,"Amount":10.01,"Currency":"RUB","TestMode":false,"OperationType":"Payment","Status":"Completed"`), true, readyWebhookPayment(), ErrPaymentMismatch},
		{"foreign provider", valid(`"TransactionId":1,"Amount":10.01,"Currency":"RUB","TestMode":true,"OperationType":"Payment","Status":"Completed"`), true, func() paymentdomain.Payment { p := readyWebhookPayment(); p.ProviderName = "tbank"; return p }(), ErrPaymentMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &webhookPaymentReader{payment: tc.payment}
			provider := newWebhookTestProvider(t, reader, tc.mode)
			_, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("ParseWebhook error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestCloudPaymentsWebhookRequiresReadyPaymentAndPreservesReaderContext(t *testing.T) {
	reader := &webhookPaymentReader{payment: readyWebhookPayment()}
	provider := newWebhookTestProvider(t, reader, true)
	type contextKey struct{}
	ctx := context.WithValue(t.Context(), contextKey{}, "trace")
	if _, err := provider.ParseWebhook(ctx, webhookEnvelopeJSON(t, "application/json", payJSON("1"))); err != nil {
		t.Fatalf("ParseWebhook error = %v", err)
	}
	if reader.gotCtx != ctx || reader.calls != 1 || reader.writes != 0 {
		t.Fatalf("reader context/calls/writes = %v/%d/%d", reader.gotCtx, reader.calls, reader.writes)
	}

	reader.err = paymentservice.ErrNotFound
	if _, err := provider.ParseWebhook(ctx, webhookEnvelopeJSON(t, "application/json", payJSON("1"))); !errors.Is(err, ErrPaymentNotReady) {
		t.Fatalf("missing payment: %v", err)
	}
	reader.err = nil
	reader.payment.Status = paymentdomain.StatusCreating
	reader.payment.ProviderPaymentID = ""
	_, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", payJSON("1")))
	if !errors.Is(err, ErrPaymentNotReady) {
		t.Fatalf("ParseWebhook error = %v, want ErrPaymentNotReady", err)
	}
	reader.payment.ProviderPaymentID = webhookPaymentID
	if _, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", payJSON("1"))); err != nil {
		t.Fatalf("ParseWebhook error after provider ID is saved = %v", err)
	}
}

func TestCloudPaymentsWebhookRejectsMalformedAndAmbiguousPayloads(t *testing.T) {
	invalidForms := [][]byte{
		{},
		[]byte("InvoiceId=" + webhookPaymentID + "&InvoiceId=" + webhookPaymentID),
		[]byte("InvoiceId=" + webhookPaymentID + "&TransactionId=1&TransactionId=2"),
		[]byte("InvoiceId=" + webhookPaymentID + "&Amount=1.001"),
		[]byte("InvoiceId=" + webhookPaymentID + "&Amount=92233720368547758.08"),
		[]byte("InvoiceId=" + webhookPaymentID + "&Amount=1e2"),
		[]byte("InvoiceId=" + webhookPaymentID + "&Amount=-1"),
		[]byte("InvoiceId=" + webhookPaymentID + "&Amount=1,00"),
	}
	for i, body := range invalidForms {
		t.Run(fmt.Sprintf("invalid %d", i), func(t *testing.T) {
			reader := &webhookPaymentReader{payment: readyWebhookPayment()}
			provider := newWebhookTestProvider(t, reader, true)
			_, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/x-www-form-urlencoded", body))
			if !errors.Is(err, ErrInvalidNotification) {
				t.Fatalf("ParseWebhook error = %v, want ErrInvalidNotification", err)
			}
			if reader.calls != 0 {
				t.Fatalf("invalid payload caused %d payment reads", reader.calls)
			}
		})
	}

	for name, body := range map[string][]byte{
		"empty JSON":        {},
		"large transaction": payJSON("9223372036854775808"),
		"missing fields":    []byte(`{"InvoiceId":"` + webhookPaymentID + `"}`),
		"exponent":          []byte(`{"InvoiceId":"` + webhookPaymentID + `","TransactionId":1,"Amount":1e2,"Currency":"RUB","TestMode":true,"OperationType":"Payment","Status":"Completed"}`),
	} {
		t.Run(name, func(t *testing.T) {
			reader := &webhookPaymentReader{payment: readyWebhookPayment()}
			provider := newWebhookTestProvider(t, reader, true)
			_, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", body))
			if !errors.Is(err, ErrInvalidNotification) {
				t.Fatalf("ParseWebhook error = %v, want ErrInvalidNotification", err)
			}
			if reader.calls != 0 {
				t.Fatalf("invalid payload caused %d payment reads", reader.calls)
			}
		})
	}

}

func TestCloudPaymentsWebhookPreservesReaderErrorAndCancellation(t *testing.T) {
	dbErr := errors.New("database unavailable")
	reader := &webhookPaymentReader{err: dbErr}
	provider := newWebhookTestProvider(t, reader, true)
	_, err := provider.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", payJSON("1")))
	if !errors.Is(err, dbErr) {
		t.Fatalf("ParseWebhook error = %v, want wrapped reader error", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reader.err = context.Canceled
	_, err = provider.ParseWebhook(ctx, webhookEnvelopeJSON(t, "application/json", payJSON("1")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ParseWebhook error = %v, want context.Canceled", err)
	}
}

func TestCloudPaymentsWebhookFieldBoundaries(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"Amount", "0"}, {"Amount", "-1"}, {"Amount", "+10.01"}, {"Amount", "10,01"}, {"Amount", "1e1"}, {"Amount", "10.011"}, {"Amount", "92233720368547758.08"},
		{"TransactionId", "0"}, {"TransactionId", "-1"}, {"TransactionId", "9223372036854775808"}, {"TransactionId", "1.1"},
		{"TestMode", "true"}, {"TestMode", "1"}, {"TestMode", nil}, {"TestMode", 2}, {"InvoiceId", "00000000-0000-0000-0000-000000000000"},
		{"ReasonCode", 1}, {"OperationType", "Refund"}, {"Status", "Authorized"},
	} {
		t.Run(fmt.Sprintf("%s-%v", tc.field, tc.value), func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(payJSON("1"), &fields); err != nil {
				t.Fatal(err)
			}
			fields[tc.field] = tc.value
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			reader := &webhookPaymentReader{payment: readyWebhookPayment()}
			p := newWebhookTestProvider(t, reader, true)
			_, err = p.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", raw))
			if !errors.Is(err, ErrInvalidNotification) || reader.calls != 0 {
				t.Fatalf("error=%v reads=%d", err, reader.calls)
			}
		})
	}
	for _, amount := range []string{"10.01", "10.0100", "92233720368547758.07"} {
		t.Run(amount, func(t *testing.T) {
			payment := readyWebhookPayment()
			if amount == "92233720368547758.07" {
				payment.AmountMinor = 9223372036854775807
			}
			reader := &webhookPaymentReader{payment: payment}
			p := newWebhookTestProvider(t, reader, true)
			values, _ := url.ParseQuery(string(payForm("9007199254740993")))
			values.Set("Amount", amount)
			for _, extra := range []bool{false, true} {
				raw := []byte(values.Encode())
				if extra {
					raw = append(raw, []byte("&PaymentAmount=999&PaymentCurrency=USD")...)
				}
				event, err := p.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/x-www-form-urlencoded", raw))
				if err != nil || event.ProviderEventID != "pay:9007199254740993" {
					t.Fatalf("event=%+v err=%v", event, err)
				}
			}
		})
	}
	for _, field := range []string{"InvoiceId", "TransactionId", "Amount", "Currency", "TestMode", "Status", "OperationType", "ReasonCode"} {
		t.Run("duplicate-"+field, func(t *testing.T) {
			raw := append(payForm("1"), []byte("&"+field+"=x&"+field+"=y")...)
			p := newWebhookTestProvider(t, &webhookPaymentReader{payment: readyWebhookPayment()}, true)
			if _, err := p.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/x-www-form-urlencoded", raw)); !errors.Is(err, ErrInvalidNotification) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCloudPaymentsWebhookIdentityAndNoSignatureFallback(t *testing.T) {
	p := newWebhookTestProvider(t, &webhookPaymentReader{payment: readyWebhookPayment()}, true)
	raw := payJSON("1")
	if err := p.VerifyWebhook(t.Context(), http.Header{"X-Content-Hmac": {webhookSignature("test-secret", raw)}}, webhookEnvelopeJSON(t, "application/json", raw)); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("fallback: %v", err)
	}
	a, err := p.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", raw))
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.ParseWebhook(t.Context(), webhookEnvelopeJSON(t, "application/json", payJSON("2")))
	if err != nil {
		t.Fatal(err)
	}
	if a.ProviderEventID == b.ProviderEventID {
		t.Fatal("distinct transactions share key")
	}
}
