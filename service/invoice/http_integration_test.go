//go:build functional

package invoice_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	pg "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	service "github.com/Seraf-seraf/payment/service/invoice"
	merchantservice "github.com/Seraf-seraf/payment/service/merchant"
	"github.com/google/uuid"
)

func httpInvoiceRequest(h *harness) d.Request {
	return d.Request{RequestID: uuid.New(), OrderID: "HTTP-invoice", AmountMinor: 10000, Currency: "RUB", Description: "Invoice acceptance", AcceptUntil: h.now.Add(24 * time.Hour), Payer: d.Payer{Kind: "company", Name: "Test company", INN: "7701234567", KPP: "770101001", LegalAddress: "Test address"}, Lines: []d.Line{{ID: "lot", Name: "Test lot", Quantity: 1, Unit: "set", UnitPriceMinor: 10000, AmountMinor: 10000, VAT: d.VAT{Kind: "none"}}}}
}

func (h *harness) merchantHTTPRouter(t *testing.T) http.Handler {
	t.Helper()
	encrypted, err := crypto.EncryptSecret(callbackTestEncryptionKey, callbackTestSecret)
	must(t, err)
	_, err = h.pool.Exec(t.Context(), `UPDATE merchants SET api_key_hash=$2,shared_secret_encrypted=$3 WHERE id=$1`, h.registry.connection.MerchantID, crypto.SHA256Hex("invoice-test-api-key"), encrypted)
	must(t, err)
	return httpapi.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)),
		merchantservice.NewService(pg.NewMerchantRepository(h.pool, callbackTestEncryptionKey)), nil,
		service.NewService(pg.NewInvoiceRepository(h.pool), h.registry, h.tx, func() time.Time { return h.now }), nil, time.Minute)
}

func invoiceHTTPRequest(method, path string, body []byte, key uuid.UUID, apiKey string) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-API-Key", apiKey)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	r.Header.Set("X-Timestamp", timestamp)
	r.Header.Set("X-Signature", crypto.HMACSHA256Hex(callbackTestSecret, append([]byte(timestamp+"."), body...)))
	if method == http.MethodPost {
		r.Header.Set("Idempotency-Key", key.String())
	}
	return r
}

func TestFunctionalInvoiceHTTPConcurrentIdempotency(t *testing.T) {
	h := newHarnessWithoutInvoice(t, "bank_credits", "recover_only")
	router := h.merchantHTTPRouter(t)
	req := httpInvoiceRequest(h)
	body, err := json.Marshal(req)
	must(t, err)
	const requests = 20
	responses := make([]*httptest.ResponseRecorder, requests)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range responses {
		wg.Go(func() {
			<-start
			r := httptest.NewRecorder()
			router.ServeHTTP(r, invoiceHTTPRequest(http.MethodPost, "/api/v1/invoice-payments", body, req.RequestID, "invoice-test-api-key"))
			responses[i] = r
		})
	}
	close(start)
	wg.Wait()
	var paymentID uuid.UUID
	created := 0
	for _, response := range responses {
		if response.Code == http.StatusAccepted {
			created++
		} else if response.Code != http.StatusOK {
			t.Fatalf("concurrent response=%d %s", response.Code, response.Body.String())
		}
		var projection struct {
			PaymentID uuid.UUID `json:"payment_id"`
		}
		must(t, json.Unmarshal(response.Body.Bytes(), &projection))
		if paymentID == uuid.Nil {
			paymentID = projection.PaymentID
		}
		if projection.PaymentID != paymentID {
			t.Fatal("concurrent replay changed payment ID")
		}
	}
	if created != 1 {
		t.Fatalf("created responses=%d, want 1", created)
	}
	for _, table := range []string{"payments", "invoice_payments", "invoice_commands", "outbox_events"} {
		if n := h.count(t, "SELECT count(*) FROM "+table); n != 1 {
			t.Fatalf("concurrent creation left %d rows in %s", n, table)
		}
	}
	var number int64
	must(t, h.pool.QueryRow(t.Context(), "SELECT invoice_number FROM invoice_payments WHERE payment_id=$1", paymentID).Scan(&number))
	if number != 1 || h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.issue'") != 1 || h.registry.provider.(*providerStub).issues != 0 {
		t.Fatal("idempotency allocated extra number/issue or dispatched during HTTP create")
	}
	req.Description = "different payload"
	changed, err := json.Marshal(req)
	must(t, err)
	conflict := httptest.NewRecorder()
	router.ServeHTTP(conflict, invoiceHTTPRequest(http.MethodPost, "/api/v1/invoice-payments", changed, req.RequestID, "invoice-test-api-key"))
	if conflict.Code != http.StatusConflict || h.count(t, "SELECT count(*) FROM payments") != 1 {
		t.Fatalf("changed payload=%d %s", conflict.Code, conflict.Body.String())
	}
	firstMerchant := h.registry.connection.MerchantID
	secondMerchant := uuid.New()
	encrypted, err := crypto.EncryptSecret(callbackTestEncryptionKey, callbackTestSecret)
	must(t, err)
	_, err = h.pool.Exec(t.Context(), `INSERT INTO merchants(id,name,api_key_hash,shared_secret_encrypted,callback_url,provider_name) VALUES($1,'second merchant',$2,$3,'https://merchant.example/callback','mock')`, secondMerchant, crypto.SHA256Hex("second-api-key"), encrypted)
	must(t, err)
	h.registry.connection.MerchantID = secondMerchant
	h.registry.connection.ID = "second-connection"
	independent := httptest.NewRecorder()
	router.ServeHTTP(independent, invoiceHTTPRequest(http.MethodPost, "/api/v1/invoice-payments", body, req.RequestID, "second-api-key"))
	if independent.Code != http.StatusAccepted || h.count(t, "SELECT count(*) FROM payments") != 2 {
		t.Fatalf("another merchant did not get independent intent: %d %s", independent.Code, independent.Body.String())
	}
	foreign := httptest.NewRecorder()
	router.ServeHTTP(foreign, invoiceHTTPRequest(http.MethodGet, "/api/v1/invoice-payments/"+paymentID.String(), nil, uuid.Nil, "second-api-key"))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("merchant %s can read invoice of %s: %d", secondMerchant, firstMerchant, foreign.Code)
	}
}

func TestFunctionalInvoiceHTTPMoneyRejectedBeforeWrites(t *testing.T) {
	h := newHarnessWithoutInvoice(t, "bank_credits", "recover_only")
	router := h.merchantHTTPRouter(t)
	cases := []struct {
		name   string
		change func(*d.Request)
		raw    func(string) string
		valid  bool
	}{
		{name: "zero", change: func(r *d.Request) { r.AmountMinor = 0; r.Lines[0].UnitPriceMinor = 0; r.Lines[0].AmountMinor = 0 }},
		{name: "one", valid: true, change: func(r *d.Request) { r.AmountMinor = 1; r.Lines[0].UnitPriceMinor = 1; r.Lines[0].AmountMinor = 1 }},
		{name: "maximum", valid: true, change: func(r *d.Request) {
			r.AmountMinor = d.MaxAmountMinor
			r.Lines[0].UnitPriceMinor = d.MaxAmountMinor
			r.Lines[0].AmountMinor = d.MaxAmountMinor
		}},
		{name: "above_maximum", change: func(r *d.Request) {
			r.AmountMinor = d.MaxAmountMinor + 1
			r.Lines[0].UnitPriceMinor = d.MaxAmountMinor + 1
			r.Lines[0].AmountMinor = d.MaxAmountMinor + 1
		}},
		{name: "product_overflow", change: func(r *d.Request) {
			r.AmountMinor = 1
			r.Lines[0].Quantity = 1000000
			r.Lines[0].UnitPriceMinor = d.MaxAmountMinor
			r.Lines[0].AmountMinor = 1
		}},
		{name: "sum_above_limit", change: func(r *d.Request) {
			r.AmountMinor = d.MaxAmountMinor
			r.Lines[0].UnitPriceMinor = d.MaxAmountMinor
			r.Lines[0].AmountMinor = d.MaxAmountMinor
			extra := r.Lines[0]
			extra.ID = "extra"
			extra.UnitPriceMinor = 1
			extra.AmountMinor = 1
			r.Lines = append(r.Lines, extra)
		}},
		{name: "float_amount", raw: func(s string) string { return strings.Replace(s, `"amount_minor":10000`, `"amount_minor":10000.5`, 1) }},
		{name: "string_amount", raw: func(s string) string { return strings.Replace(s, `"amount_minor":10000`, `"amount_minor":"10000"`, 1) }},
		{name: "float_quantity", raw: func(s string) string { return strings.Replace(s, `"quantity":1`, `"quantity":1.5`, 1) }},
		{name: "string_price", raw: func(s string) string {
			return strings.Replace(s, `"unit_price_minor":10000`, `"unit_price_minor":"10000"`, 1)
		}},
		{name: "line_product_mismatch", change: func(r *d.Request) { r.Lines[0].Quantity = 2 }},
		{name: "request_sum_mismatch", change: func(r *d.Request) { r.AmountMinor++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httpInvoiceRequest(h)
			if tc.change != nil {
				tc.change(&r)
			}
			body, err := json.Marshal(r)
			must(t, err)
			if tc.raw != nil {
				body = []byte(tc.raw(string(body)))
			}
			before := h.count(t, "SELECT count(*) FROM payments")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, invoiceHTTPRequest(http.MethodPost, "/api/v1/invoice-payments", body, r.RequestID, "invoice-test-api-key"))
			want := http.StatusBadRequest
			writes := before
			if tc.valid {
				want = http.StatusAccepted
				writes++
			}
			if response.Code != want {
				t.Fatalf("money response=%d want=%d %s", response.Code, want, response.Body.String())
			}
			for _, table := range []string{"payments", "invoice_payments", "invoice_commands", "outbox_events"} {
				if got := h.count(t, "SELECT count(*) FROM "+table); got != writes {
					t.Fatalf("money validation left %d rows in %s, want %d", got, table, writes)
				}
			}
			if h.registry.provider.(*providerStub).issues != 0 {
				t.Fatal("money validation dispatched provider")
			}
		})
	}
}

func TestFunctionalInvoiceHTTPAccessRejectsTampering(t *testing.T) {
	h := newHarnessWithoutInvoice(t, "bank_credits", "recover_only")
	router := h.merchantHTTPRouter(t)
	r := httpInvoiceRequest(h)
	body, err := json.Marshal(r)
	must(t, err)
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		want   int
	}{
		{"raw_body", func(req *http.Request) { req.Body = io.NopCloser(strings.NewReader(" " + string(body))) }, http.StatusUnauthorized},
		{"hmac", func(req *http.Request) { req.Header.Set("X-Signature", strings.Repeat("0", 64)) }, http.StatusUnauthorized},
		{"request_id_key_mismatch", func(req *http.Request) { req.Header.Set("Idempotency-Key", uuid.NewString()) }, http.StatusBadRequest},
		{"unknown_api_key", func(req *http.Request) { req.Header.Set("X-API-Key", "unknown") }, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := invoiceHTTPRequest(http.MethodPost, "/api/v1/invoice-payments", body, r.RequestID, "invoice-test-api-key")
			tc.change(req)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != tc.want {
				t.Fatalf("access response=%d want=%d %s", response.Code, tc.want, response.Body.String())
			}
			if h.count(t, "SELECT count(*) FROM payments") != 0 {
				t.Fatal("rejected request created payment")
			}
		})
	}
	hook, _ := h.notificationRouter(t, "shared_operation_id", nil, nil)
	response := httptest.NewRecorder()
	unknown := httptest.NewRequest(http.MethodPost, "/api/v1/provider-invoice-webhooks/unknown-connection", strings.NewReader(`{"type":"status_hint"}`))
	unknown.Header.Set("Content-Type", "application/json")
	hook.ServeHTTP(response, unknown)
	if response.Code != http.StatusNotFound || h.count(t, "SELECT count(*) FROM invoice_inbox") != 0 {
		t.Fatalf("unknown connection accepted: %d %s", response.Code, response.Body.String())
	}
}

func TestFunctionalAcquiringStillRequiresReceipt(t *testing.T) {
	h := newHarnessWithoutInvoice(t, "bank_credits", "recover_only")
	router := h.merchantHTTPRouter(t)
	for _, method := range []string{"card", "sbp"} {
		body := []byte(`{"order_id":"acquiring","amount_minor":10000,"currency":"RUB","payment_method":"` + method + `"}`)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, invoiceHTTPRequest(http.MethodPost, "/api/v1/payments", body, uuid.New(), "invoice-test-api-key"))
		if response.Code != http.StatusBadRequest || h.count(t, "SELECT count(*) FROM payments") != 0 {
			t.Fatalf("%s accepted without receipt: %d %s", method, response.Code, response.Body.String())
		}
	}
}
