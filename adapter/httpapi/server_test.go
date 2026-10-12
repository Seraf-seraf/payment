package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

func TestCreatePaymentRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, fixedMerchantAuth{}, &noopPayments{}, nil, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", body())
	rec := httptest.NewRecorder()

	server.CreatePayment(rec, req, CreatePaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(time.Now().Unix(), 10),
		XSignature: "bad",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCreatePaymentRejectsOldTimestamp(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, fixedMerchantAuth{}, &noopPayments{}, nil, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", body())
	rec := httptest.NewRecorder()
	timestamp := time.Now().Add(-10 * time.Minute).Unix()
	signature := crypto.HMACSHA256Hex("secret", []byte("0."+bodyString))

	server.CreatePayment(rec, req, CreatePaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(timestamp, 10),
		XSignature: signature,
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCreatePaymentAcceptsValidSignature(t *testing.T) {
	t.Parallel()

	payments := &noopPayments{}
	server := NewServer(nil, fixedMerchantAuth{}, payments, nil, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", body())
	rec := httptest.NewRecorder()
	timestamp := time.Now().Unix()
	signature := crypto.HMACSHA256Hex("secret", []byte(formatMessage(timestamp)))

	server.CreatePayment(rec, req, CreatePaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(timestamp, 10),
		XSignature: signature,
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if payments.request.Receipt.Taxation != "osn" {
		t.Fatalf("receipt taxation = %q", payments.request.Receipt.Taxation)
	}
	if len(payments.request.Receipt.Items) != 1 {
		t.Fatalf("receipt items = %+v", payments.request.Receipt.Items)
	}
	item := payments.request.Receipt.Items[0]
	if item.Name != "Test service" || item.AmountMinor != 1000 || item.PaymentMethod != "full_payment" || item.PaymentObject != "service" || item.Tax != "none" {
		t.Fatalf("receipt item = %+v", item)
	}
}

func TestGetPaymentRequiresValidSignature(t *testing.T) {
	t.Parallel()

	payments := &noopPayments{}
	server := NewServer(nil, fixedMerchantAuth{}, payments, nil, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/payments/"+uuid.NewString(), nil)
	rec := httptest.NewRecorder()

	server.GetPayment(rec, req, uuid.New(), GetPaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(time.Now().Unix(), 10),
		XSignature: "bad",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestGetPaymentReturnsMerchantPayment(t *testing.T) {
	t.Parallel()

	merchantID := uuid.New()
	paymentID := uuid.New()
	payments := &noopPayments{
		payment: paymentdomain.Payment{
			ID:              paymentID,
			MerchantID:      merchantID,
			MerchantOrderID: "order-1",
			AmountMinor:     1000,
			Currency:        "RUB",
			Status:          paymentdomain.StatusPending,
			PaymentURL:      "https://pay.test",
		},
	}
	server := NewServer(nil, fixedMerchantAuth{merchantID: merchantID}, payments, nil, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/payments/"+paymentID.String(), nil)
	rec := httptest.NewRecorder()
	timestamp := time.Now().Unix()

	server.GetPayment(rec, req, paymentID, GetPaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(timestamp, 10),
		XSignature: crypto.HMACSHA256Hex("secret", []byte(strconv.FormatInt(timestamp, 10)+".")),
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestGetPaymentHidesOtherMerchantPayment(t *testing.T) {
	t.Parallel()

	paymentID := uuid.New()
	payments := &noopPayments{
		payment: paymentdomain.Payment{
			ID:         paymentID,
			MerchantID: uuid.New(),
			Status:     paymentdomain.StatusPending,
		},
	}
	server := NewServer(nil, fixedMerchantAuth{merchantID: uuid.New()}, payments, nil, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/payments/"+paymentID.String(), nil)
	rec := httptest.NewRecorder()
	timestamp := time.Now().Unix()

	server.GetPayment(rec, req, paymentID, GetPaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(timestamp, 10),
		XSignature: crypto.HMACSHA256Hex("secret", []byte(strconv.FormatInt(timestamp, 10)+".")),
	})

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestCreateInvoicePaymentAcceptsSignedRequest(t *testing.T) {
	t.Parallel()

	requestID := uuid.New()
	paymentID := uuid.New()
	payerEmail := openapi_types.Email("accounts@example.com")
	requestBody := `{"request_id":"` + requestID.String() + `","order_id":"DV-1042","amount_minor":1200000,"currency":"RUB","description":"Оплата заказа DV-1042","accept_until":"2030-10-13T06:00:00Z","payer":{"kind":"company","name":"ООО Пример","inn":"7701234567","kpp":"770101001","legal_address":"Тестовый адрес","email":"accounts@example.com"},"lines":[{"id":"order-line-1","name":"Неделимый лот","quantity":1,"unit":"set","unit_price_minor":1200000,"amount_minor":1200000,"vat":{"kind":"none","rate_basis_points":null,"amount_minor":0}}]}`
	invoices := &noopInvoices{result: ports.InvoiceCreateResult{Created: true, Invoice: invoicedomain.Record{
		PaymentID: paymentID, Request: invoicedomain.Request{RequestID: requestID, OrderID: "DV-1042", AmountMinor: 1200000, Currency: "RUB", AcceptUntil: time.Date(2030, 10, 13, 6, 0, 0, 0, time.UTC)},
		IssuanceState: "queued", FundingState: "none", Revision: 1,
	}}}
	handler := NewRouter(slog.Default(), fixedMerchantAuth{}, &noopPayments{}, invoices, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-payments", strings.NewReader(requestBody))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Idempotency-Key", requestID.String())
	req.Header.Set("X-API-Key", "api-key")
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+requestBody)))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/api/v1/invoice-payments/"+paymentID.String() {
		t.Fatalf("response = %d %s headers=%v", rec.Code, rec.Body.String(), rec.Header())
	}
	if invoices.calls != 1 || invoices.request.RequestID != requestID || invoices.request.Payer.Email != string(payerEmail) ||
		len(invoices.request.Lines) != 1 || invoices.request.Lines[0].AmountMinor != 1200000 {
		t.Fatalf("invoice use-case request = %+v", invoices.request)
	}
}

func TestCreateInvoicePaymentRequiresIdempotencyKeyMatch(t *testing.T) {
	t.Parallel()

	requestID := uuid.New()
	requestBody := `{"request_id":"` + requestID.String() + `","order_id":"DV-1042","amount_minor":1,"currency":"RUB","description":"test","accept_until":"2030-10-13T06:00:00Z","payer":{"kind":"company","name":"ООО Пример","inn":"7701234567","kpp":"770101001","legal_address":"Адрес"},"lines":[{"id":"line-1","name":"Lot","quantity":1,"unit":"set","unit_price_minor":1,"amount_minor":1,"vat":{"kind":"none","rate_basis_points":null,"amount_minor":0}}]}`
	invoices := &noopInvoices{}
	server := NewServer(nil, fixedMerchantAuth{}, &noopPayments{}, invoices, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-payments", strings.NewReader(requestBody))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	server.CreateInvoicePayment(rec, req, CreateInvoicePaymentParams{
		IdempotencyKey: openapi_types.UUID(uuid.New()), XAPIKey: "api-key", XTimestamp: timestamp,
		XSignature: crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+requestBody)),
	})
	if rec.Code != http.StatusBadRequest || invoices.calls != 0 {
		t.Fatalf("response = %d %s, calls=%d", rec.Code, rec.Body.String(), invoices.calls)
	}
}

func TestCreateInvoicePaymentOpenAPIRejectsProviderSelection(t *testing.T) {
	t.Parallel()

	requestID := uuid.New()
	requestBody := `{"request_id":"` + requestID.String() + `","order_id":"DV-1042","amount_minor":1,"currency":"RUB","description":"test","accept_until":"2030-10-13T06:00:00Z","provider":"mock_invoice","payer":{"kind":"company","name":"ООО Пример","inn":"7701234567","kpp":"770101001","legal_address":"Адрес"},"lines":[{"id":"line-1","name":"Lot","quantity":1,"unit":"set","unit_price_minor":1,"amount_minor":1,"vat":{"kind":"none","rate_basis_points":null,"amount_minor":0}}]}`
	invoices := &noopInvoices{}
	handler := NewRouter(slog.Default(), fixedMerchantAuth{}, &noopPayments{}, invoices, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-payments", strings.NewReader(requestBody))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", requestID.String())
	req.Header.Set("X-API-Key", "api-key")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+requestBody)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || invoices.calls != 0 {
		t.Fatalf("response = %d %s, calls=%d", rec.Code, rec.Body.String(), invoices.calls)
	}
}

const bodyString = `{"order_id":"order-1","amount_minor":1000,"currency":"RUB","receipt":{"email":"customer@example.com","taxation":"osn","items":[{"name":"Test service","price_minor":1000,"quantity":1,"amount_minor":1000,"payment_method":"full_payment","payment_object":"service","tax":"none"}]}}`

func body() *strings.Reader {
	return strings.NewReader(bodyString)
}

func formatMessage(timestamp int64) string {
	return strconv.FormatInt(timestamp, 10) + "." + bodyString
}

type fixedMerchantAuth struct {
	merchantID uuid.UUID
}

func (a fixedMerchantAuth) AuthenticateAPIKey(context.Context, string) (merchantdomain.Merchant, error) {
	merchantID := a.merchantID
	if merchantID == uuid.Nil {
		merchantID = uuid.New()
	}
	return merchantdomain.Merchant{ID: merchantID, SharedSecret: "secret", CallbackURL: "https://merchant.example/callback", ProviderName: "mock", IsActive: true}, nil
}

type noopPayments struct {
	request ports.CreatePaymentRequest
	payment paymentdomain.Payment
}

func (p *noopPayments) CreatePayment(_ context.Context, req ports.CreatePaymentRequest) (ports.CreatePaymentResult, error) {
	p.request = req
	return ports.CreatePaymentResult{
		Created: true,
		Payment: paymentdomain.Payment{
			ID:              uuid.New(),
			MerchantOrderID: "order-1",
			AmountMinor:     1000,
			Currency:        "RUB",
			Status:          paymentdomain.StatusPending,
			PaymentURL:      "https://pay.test",
		},
	}, nil
}

func (p *noopPayments) GetPayment(context.Context, uuid.UUID) (paymentdomain.Payment, error) {
	return p.payment, nil
}

type noopInvoices struct {
	request invoicedomain.Request
	result  ports.InvoiceCreateResult
	calls   int
}

func (s *noopInvoices) CreateInvoicePayment(_ context.Context, request invoicedomain.Request, _ merchantdomain.Merchant) (ports.InvoiceCreateResult, error) {
	s.request = request
	s.calls++
	return s.result, nil
}

func TestCreateInvoicePaymentDisabledReturnsServiceUnavailable(t *testing.T) {
	requestID := uuid.New()
	requestBody := `{"request_id":"` + requestID.String() + `","order_id":"order","amount_minor":1,"currency":"RUB","description":"test","accept_until":"2030-10-13T06:00:00Z","payer":{"kind":"company","name":"Mock","inn":"7701234567","kpp":"770101001","legal_address":"Mock"},"lines":[{"id":"lot","name":"Lot","quantity":1,"unit":"set","unit_price_minor":1,"amount_minor":1,"vat":{"kind":"none","rate_basis_points":null,"amount_minor":0}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-payments", strings.NewReader(requestBody))
	req.RemoteAddr = "127.0.0.1:10000"
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	params := CreateInvoicePaymentParams{XAPIKey: "api-key", XTimestamp: timestamp, XSignature: crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+requestBody)), IdempotencyKey: requestID}
	rec := httptest.NewRecorder()
	NewServer(nil, fixedMerchantAuth{}, nil, nil, nil, 5*time.Minute).CreateInvoicePayment(rec, req, params)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}
