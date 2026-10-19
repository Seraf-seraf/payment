package cloudpayments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

type unusedPaymentRepository struct{ ports.PaymentRepository }

func newTestProvider(t *testing.T, handler http.Handler) (*Provider, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := New(Options{
		APIURL:    server.URL,
		PublicID:  "public-id",
		APISecret: "api-secret",
		TestMode:  true,
		Timeout:   time.Second,
		Payments:  unusedPaymentRepository{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, server
}

func baseCreateRequest() providerdomain.CreatePaymentRequest {
	return providerdomain.CreatePaymentRequest{
		PaymentID:       uuid.MustParse("11111111-1111-4111-8111-111111111111").String(),
		OrderID:         "order-42",
		AmountMinor:     1001,
		Currency:        "RUB",
		Description:     "Оплата заказа order-42",
		PaymentMethod:   "card",
		CustomerEmail:   "buyer@example.test",
		MerchantSuccess: "https://merchant.test/success",
		MerchantFail:    "https://merchant.test/fail",
	}
}

func TestCloudPaymentsNewValidatesOptionsWithoutNetworkAccess(t *testing.T) {
	base := Options{PublicID: "public-id", APISecret: "api-secret", Timeout: time.Second, Payments: unusedPaymentRepository{}}
	for _, tc := range []struct {
		name   string
		change func(*Options)
	}{
		{"missing public ID", func(o *Options) { o.PublicID = "" }},
		{"missing secret", func(o *Options) { o.APISecret = "" }},
		{"missing payment reader", func(o *Options) { o.Payments = nil }},
		{"nonpositive timeout", func(o *Options) { o.Timeout = 0 }},
		{"relative API URL", func(o *Options) { o.APIURL = "/api" }},
		{"unsupported API URL scheme", func(o *Options) { o.APIURL = "ftp://api.example.test" }},
		{"API URL userinfo", func(o *Options) { o.APIURL = "https://user:pass@api.example.test" }},
		{"API URL query", func(o *Options) { o.APIURL = "https://api.example.test?" }},
		{"API URL fragment", func(o *Options) { o.APIURL = "https://api.example.test#" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := base
			tc.change(&options)
			if _, err := New(options); err == nil {
				t.Fatal("New returned no error")
			}
		})
	}
	provider, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.apiURL.String(); got != defaultAPIURL {
		t.Fatalf("default API URL = %q", got)
	}
}

func decodeJSONMap(t *testing.T, reader io.Reader) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return value
}

func requireCardResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"Success":true,"Model":{"Id":"page-1","Url":"https://pay.example.test/page-1"}}`)
}

func TestCloudPaymentsCreateCardModes(t *testing.T) {
	for _, method := range []string{"card", "bank_card", ""} {
		t.Run(fmt.Sprintf("method-%q", method), func(t *testing.T) {
			provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/orders/create" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				username, password, ok := r.BasicAuth()
				if !ok || username != "public-id" || password != "api-secret" {
					t.Errorf("Basic auth = %q:%q, present=%t", username, password, ok)
				}
				if got := r.Header.Get("X-Request-ID"); got != "cloudpayments:11111111-1111-4111-8111-111111111111:card" {
					t.Errorf("X-Request-ID = %q", got)
				}
				payload := decodeJSONMap(t, r.Body)
				for key, want := range map[string]any{
					"Amount": json.Number("10.01"), "Currency": "RUB", "InvoiceId": "11111111-1111-4111-8111-111111111111",
					"Description": "Оплата заказа order-42", "Email": "buyer@example.test", "RequireConfirmation": false,
					"SendEmail": false, "SendSms": false, "SendViber": false,
					"SuccessRedirectUrl": "https://merchant.test/success", "FailRedirectUrl": "https://merchant.test/fail",
				} {
					if payload[key] != want {
						t.Errorf("payload[%q] = %#v, want %#v", key, payload[key], want)
					}
				}
				if _, exists := payload["IsTest"]; exists {
					t.Error("card page request must not contain IsTest")
				}
				requireCardResponse(w)
			}))
			req := baseCreateRequest()
			req.PaymentMethod = method
			result, err := provider.CreatePayment(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if result.ProviderPaymentID != req.PaymentID || result.PaymentURL != "https://pay.example.test/page-1" {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestCloudPaymentsCreateSBPLink(t *testing.T) {
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/payments/qr/sbp/link" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		payload := decodeJSONMap(t, r.Body)
		for key, want := range map[string]any{
			"Amount": json.Number("10.01"), "Currency": "RUB", "InvoiceId": "11111111-1111-4111-8111-111111111111",
			"PublicId": "public-id", "Scheme": "charge", "IsTest": true,
			"SuccessRedirectUrl": "https://merchant.test/success",
		} {
			if payload[key] != want {
				t.Errorf("payload[%q] = %#v, want %#v", key, payload[key], want)
			}
		}
		for _, key := range []string{"FailRedirectUrl", "TtlMinutes", "Device", "AccountId"} {
			if _, exists := payload[key]; exists {
				t.Errorf("SBP request unexpectedly contains %s", key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"Success":true,"Model":{"TransactionId":123,"QrUrl":"https://qr.example.test/pay/123"}}`)
	}))
	req := baseCreateRequest()
	req.PaymentMethod = "sbp"
	result, err := provider.CreatePayment(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderPaymentID != req.PaymentID || result.PaymentURL != "https://qr.example.test/pay/123" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCloudPaymentsCreateRejectsInvalidRequestsBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		requireCardResponse(w)
	}))
	tests := []struct {
		name   string
		change func(*providerdomain.CreatePaymentRequest)
	}{
		{"unsupported method", func(r *providerdomain.CreatePaymentRequest) { r.PaymentMethod = "wallet" }},
		{"unsupported currency", func(r *providerdomain.CreatePaymentRequest) { r.Currency = "USD" }},
		{"zero amount", func(r *providerdomain.CreatePaymentRequest) { r.AmountMinor = 0 }},
		{"negative amount", func(r *providerdomain.CreatePaymentRequest) { r.AmountMinor = -1 }},
		{"invalid payment UUID", func(r *providerdomain.CreatePaymentRequest) { r.PaymentID = "payment-1" }},
		{"unknown taxation", func(r *providerdomain.CreatePaymentRequest) { r.Receipt = testReceipt(); r.Receipt.Taxation = "other" }},
		{"unknown tax", func(r *providerdomain.CreatePaymentRequest) {
			r.Receipt = testReceipt()
			r.Receipt.Items[0].Tax = "vat99"
		}},
		{"unknown method enum", func(r *providerdomain.CreatePaymentRequest) {
			r.Receipt = testReceipt()
			r.Receipt.Items[0].PaymentMethod = "later"
		}},
		{"unknown object enum", func(r *providerdomain.CreatePaymentRequest) {
			r.Receipt = testReceipt()
			r.Receipt.Items[0].PaymentObject = "later"
		}},
		{"non-finite quantity", func(r *providerdomain.CreatePaymentRequest) {
			r.Receipt = testReceipt()
			r.Receipt.Items[0].Quantity = math.NaN()
		}},
		{"zero quantity", func(r *providerdomain.CreatePaymentRequest) {
			r.Receipt = testReceipt()
			r.Receipt.Items[0].Quantity = 0
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := baseCreateRequest()
			tc.change(&req)
			if _, err := provider.CreatePayment(t.Context(), req); err == nil {
				t.Fatal("CreatePayment returned no error")
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid requests made %d HTTP calls", got)
	}
}

func TestCloudPaymentsCreateRejectsInvalidProviderResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"http 400", http.StatusBadRequest, `{"Success":true,"Model":{"Id":"x","Url":"https://pay.test"}}`},
		{"http 500", http.StatusInternalServerError, `{"Success":true,"Model":{"Id":"x","Url":"https://pay.test"}}`},
		{"provider failure", http.StatusOK, `{"Success":false,"Message":"declined"}`},
		{"malformed JSON", http.StatusOK, `{`},
		{"missing model", http.StatusOK, `{"Success":true}`},
		{"missing page ID", http.StatusOK, `{"Success":true,"Model":{"Url":"https://pay.test"}}`},
		{"missing URL", http.StatusOK, `{"Success":true,"Model":{"Id":"x"}}`},
		{"relative URL", http.StatusOK, `{"Success":true,"Model":{"Id":"x","Url":"/pay"}}`},
		{"oversized response", http.StatusOK, `{"Success":true,"Model":{"Id":"x","Url":"https://pay.test"}}` + strings.Repeat(" ", (1<<20))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = io.WriteString(w, tc.body)
			}))
			result, err := provider.CreatePayment(t.Context(), baseCreateRequest())
			if err == nil || result.PaymentURL != "" || result.ProviderPaymentID != "" {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestCloudPaymentsCreatePreservesMalformedResponseCause(t *testing.T) {
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{`)
	}))
	_, err := provider.CreatePayment(t.Context(), baseCreateRequest())
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("error = %v, want wrapped *json.SyntaxError", err)
	}
}

func TestCloudPaymentsCreateHonorsCancellationAndTimeout(t *testing.T) {
	t.Run("caller cancellation after request starts", func(t *testing.T) {
		started := make(chan struct{}, 1)
		var calls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			started <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		provider, err := New(Options{APIURL: "https://api.example.test", PublicID: "public-id", APISecret: "api-secret", Timeout: time.Second, HTTPClient: client, Payments: unusedPaymentRepository{}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := provider.CreatePayment(ctx, baseCreateRequest()); result <- err }()
		<-started
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("CreatePayment error = %v, want context.Canceled", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("HTTP calls = %d, want 1", got)
		}
	})

	t.Run("provider timeout with unbounded injected client", func(t *testing.T) {
		started := make(chan struct{}, 1)
		var calls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			started <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		provider, err := New(Options{APIURL: "https://api.example.test", PublicID: "public-id", APISecret: "api-secret", Timeout: 25 * time.Millisecond, HTTPClient: client, Payments: unusedPaymentRepository{}})
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { _, err := provider.CreatePayment(t.Context(), baseCreateRequest()); result <- err }()
		<-started
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CreatePayment error = %v, want context.DeadlineExceeded", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("HTTP calls = %d, want 1 (no retries)", got)
		}
	})

	t.Run("response body is closed", func(t *testing.T) {
		body := &closeTrackingBody{Reader: strings.NewReader(`{"Success":true,"Model":{"Id":"page-1","Url":"https://pay.example.test/page-1"}}`)}
		var calls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: r}, nil
		})}
		provider, err := New(Options{APIURL: "https://api.example.test", PublicID: "public-id", APISecret: "api-secret", Timeout: time.Second, HTTPClient: client, Payments: unusedPaymentRepository{}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.CreatePayment(t.Context(), baseCreateRequest()); err != nil {
			t.Fatal(err)
		}
		if !body.closed.Load() {
			t.Fatal("response body was not closed")
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("HTTP calls = %d, want 1 (no retries)", got)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closeTrackingBody struct {
	*strings.Reader
	closed atomic.Bool
}

func (b *closeTrackingBody) Close() error { b.closed.Store(true); return nil }

func TestCloudPaymentsCreateUsesStableRequestIdentityAndExactMinorUnits(t *testing.T) {
	type received struct {
		requestID string
		body      string
	}
	requests := make(chan received, 8)
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		requests <- received{requestID: r.Header.Get("X-Request-ID"), body: string(body)}
		requireCardResponse(w)
	}))
	req := baseCreateRequest()
	req.IdempotencyKey = "same-merchant-key"
	for _, amount := range []int64{1, 99, 100, 101, 1001, math.MaxInt64} {
		req.AmountMinor = amount
		if _, err := provider.CreatePayment(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		got := <-requests
		payload := decodeJSONMap(t, strings.NewReader(got.body))
		want := json.Number(fmt.Sprintf("%d.%02d", amount/100, amount%100))
		if payload["Amount"] != want {
			t.Errorf("amount_minor %d encoded as %#v, want %s", amount, payload["Amount"], want)
		}
		if got.requestID != "cloudpayments:"+req.PaymentID+":card" {
			t.Errorf("X-Request-ID = %q", got.requestID)
		}
		if amount == 1 {
			firstBody := got.body
			for i := 0; i < 2; i++ {
				if _, err := provider.CreatePayment(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				retry := <-requests
				if retry.body != firstBody || retry.requestID != got.requestID {
					t.Fatalf("identical request changed: first=%+v retry=%+v", got, retry)
				}
			}
		}
	}
	other := req
	other.PaymentID = uuid.NewString()
	other.AmountMinor = 1001
	if _, err := provider.CreatePayment(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if got := <-requests; got.requestID == "cloudpayments:11111111-1111-4111-8111-111111111111:card" {
		t.Fatalf("different PaymentID reused X-Request-ID %q", got.requestID)
	}
}

func testReceipt() providerdomain.Receipt {
	return providerdomain.Receipt{
		Email: "receipt@example.test", Taxation: "osn",
		Items: []providerdomain.ReceiptItem{{Name: "Item", PriceMinor: 1000, Quantity: 1, AmountMinor: 1000, Tax: "none"}},
	}
}
