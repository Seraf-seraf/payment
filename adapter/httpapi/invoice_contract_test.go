package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	invoiceservice "github.com/Seraf-seraf/payment/service/invoice"
	"github.com/google/uuid"
)

const validInvoiceRequestTemplate = `{"request_id":"%s","order_id":"DV-1042","amount_minor":1200000,"currency":"RUB","description":"Оплата заказа DV-1042","accept_until":"2030-10-13T06:00:00Z","payer":{"kind":"company","name":"ООО Пример","inn":"7701234567","kpp":"770101001","legal_address":"Тестовый адрес"},"lines":[{"id":"line-1","name":"Неделимый лот","quantity":1,"unit":"set","unit_price_minor":1200000,"amount_minor":1200000,"vat":{"kind":"none","rate_basis_points":null,"amount_minor":0}}]}`

type invoiceResponseFixture struct {
	PaymentID              string          `json:"payment_id"`
	OrderID                string          `json:"order_id"`
	PaymentMethod          string          `json:"payment_method"`
	Status                 string          `json:"status"`
	IssuanceState          string          `json:"issuance_state"`
	FundingState           string          `json:"funding_state"`
	AmountMinor            int64           `json:"amount_minor"`
	CreditedMinor          int64           `json:"credited_minor"`
	Currency               string          `json:"currency"`
	ConfirmedPaid          bool            `json:"confirmed_paid"`
	RequiresReview         bool            `json:"requires_review"`
	ReviewReasons          []string        `json:"review_reasons"`
	Revision               int64           `json:"revision"`
	AcceptUntil            time.Time       `json:"accept_until"`
	PaymentConfirmedAt     *time.Time      `json:"payment_confirmed_at"`
	ConfirmationRecordedAt *time.Time      `json:"confirmation_recorded_at"`
	Invoice                json.RawMessage `json:"invoice"`
}

type sequenceInvoices struct {
	results []ports.InvoiceCreateResult
	calls   int
}

func (s *sequenceInvoices) CreateInvoicePayment(context.Context, invoicedomain.Request, merchantdomain.Merchant) (ports.InvoiceCreateResult, error) {
	result := s.results[s.calls]
	s.calls++
	return result, nil
}

func (s *sequenceInvoices) GetInvoicePayment(context.Context, uuid.UUID, merchantdomain.Merchant) (ports.InvoiceGetResult, error) {
	return ports.InvoiceGetResult{}, errors.New("unused invoice read")
}

func invoiceContractRouter(invoices ports.InvoiceUseCase) http.Handler {
	return NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), fixedMerchantAuth{}, &noopPayments{}, invoices, nil, 5*time.Minute)
}

func signedInvoiceRequest(method, path, body string, requestID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "api-key")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+body)))
	if method == http.MethodPost {
		req.Header.Set("Idempotency-Key", requestID.String())
	}
	return req
}

func TestInvoiceCreateReplayReturnsSavedProjection(t *testing.T) {
	requestID, paymentID := uuid.New(), uuid.New()
	first := invoicedomain.Record{
		PaymentID: paymentID, Request: invoicedomain.Request{RequestID: requestID, OrderID: "DV-1042", AmountMinor: 1200000, Currency: "RUB", AcceptUntil: time.Date(2030, 10, 13, 6, 0, 0, 0, time.UTC)},
		IssuanceState: "queued", FundingState: "none", Revision: 1,
	}
	paidAt := time.Date(2030, 10, 9, 8, 30, 0, 0, time.UTC)
	second := first
	second.IssuanceState, second.FundingState = "issued", "paid"
	second.CreditedMinor, second.Revision = 1200000, 3
	second.PaymentConfirmedAt, second.ConfirmationRecordedAt = &paidAt, &paidAt
	second.Artifact = &invoicedomain.Artifact{Documents: []invoicedomain.Document{}}
	invoices := &sequenceInvoices{results: []ports.InvoiceCreateResult{
		{Invoice: first, Status: "creating", Created: true},
		{Invoice: second, Status: "succeeded", Created: false},
	}}
	router := invoiceContractRouter(invoices)
	body := fmtInvoiceRequest(requestID)

	for i, wantStatus := range []int{http.StatusAccepted, http.StatusOK} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, signedInvoiceRequest(http.MethodPost, "/api/v1/invoice-payments", body, requestID))
		if rec.Code != wantStatus {
			t.Fatalf("request %d status = %d body=%s", i+1, rec.Code, rec.Body.String())
		}
		var response invoiceResponseFixture
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("request %d response is not JSON: %v", i+1, err)
		}
		if response.PaymentID != paymentID.String() || response.PaymentMethod != "legal_entity_invoice" {
			t.Fatalf("request %d identity changed: %+v", i+1, response)
		}
		if i == 0 && (response.Status != "creating" || response.IssuanceState != "queued" || response.FundingState != "none" || response.CreditedMinor != 0 || response.ConfirmedPaid || string(response.Invoice) != "null") {
			t.Fatalf("202 response did not represent a queued intent: %+v", response)
		}
		if i == 1 && (response.Status != "succeeded" || response.IssuanceState != "issued" || response.FundingState != "paid" || response.CreditedMinor != 1200000 || !response.ConfirmedPaid || response.Revision != 3 || response.PaymentConfirmedAt == nil || response.ConfirmationRecordedAt == nil) {
			t.Fatalf("replay did not return the saved invoice projection: %+v", response)
		}
	}
	if invoices.calls != 2 {
		t.Fatalf("invoice use case calls = %d", invoices.calls)
	}
}

func TestGetInvoicePaymentUsesSignedEmptyBodyAndReturnsProjection(t *testing.T) {
	merchantID, paymentID := uuid.New(), uuid.New()
	invoices := &noopInvoices{getResult: ports.InvoiceGetResult{Status: "pending", Invoice: invoicedomain.Record{
		PaymentID: paymentID, MerchantID: merchantID,
		Request:       invoicedomain.Request{OrderID: "DV-1042", AmountMinor: 1200000, Currency: "RUB", AcceptUntil: time.Date(2030, 10, 13, 6, 0, 0, 0, time.UTC)},
		IssuanceState: "issued", FundingState: "partial", CreditedMinor: 250000, Revision: 2,
	}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), fixedMerchantAuth{merchantID: merchantID}, &noopPayments{}, invoices, nil, 5*time.Minute)
	req := signedInvoiceRequest(http.MethodGet, "/api/v1/invoice-payments/"+paymentID.String(), "", uuid.Nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET invoice response = %d %s", rec.Code, rec.Body.String())
	}
	var response invoiceResponseFixture
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.PaymentID != paymentID.String() || response.Status != "pending" || response.FundingState != "partial" || response.CreditedMinor != 250000 || response.Revision != 2 {
		t.Fatalf("GET returned wrong saved projection: %+v", response)
	}
}

func TestGetInvoicePaymentUsesMerchantScopedNotFound(t *testing.T) {
	merchantID, paymentID := uuid.New(), uuid.New()
	invoices := &noopInvoices{getResult: ports.InvoiceGetResult{Status: "pending", Invoice: invoicedomain.Record{
		PaymentID: paymentID, MerchantID: uuid.New(),
	}}}
	router := NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), fixedMerchantAuth{merchantID: merchantID}, &noopPayments{}, invoices, nil, 5*time.Minute)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, signedInvoiceRequest(http.MethodGet, "/api/v1/invoice-payments/"+paymentID.String(), "", uuid.Nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign invoice status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGetInvoicePaymentRequiresValidHMAC(t *testing.T) {
	merchantID, paymentID := uuid.New(), uuid.New()
	invoices := &noopInvoices{}
	req := signedInvoiceRequest(http.MethodGet, "/api/v1/invoice-payments/"+paymentID.String(), "", uuid.Nil)
	req.Header.Set("X-Signature", "invalid")
	rec := httptest.NewRecorder()
	NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), fixedMerchantAuth{merchantID: merchantID}, &noopPayments{}, invoices, nil, 5*time.Minute).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || invoices.getCalls != 0 {
		t.Fatalf("invalid GET signature: status=%d calls=%d body=%s", rec.Code, invoices.getCalls, rec.Body.String())
	}
}

func TestInvoiceOpenAPISchemaErrorsUseContractJSON(t *testing.T) {
	requestID := uuid.New()
	body := strings.Replace(fmtInvoiceRequest(requestID), `"request_id":"`+requestID.String()+`",`, "", 1)
	request := signedInvoiceRequest(http.MethodPost, "/api/v1/invoice-payments", body, requestID)
	recorder := httptest.NewRecorder()
	invoiceContractRouter(&noopInvoices{}).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("schema error response = %d content-type=%q body=%q", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Code != "VALIDATION_ERROR" {
		t.Fatalf("schema error body = %q, err=%v", recorder.Body.String(), err)
	}
}

func TestInvoiceOpenAPIRejectsClientPaymentStatus(t *testing.T) {
	requestID := uuid.New()
	body := strings.Replace(fmtInvoiceRequest(requestID), `"order_id":`, `"status":"succeeded","order_id":`, 1)
	invoices := &noopInvoices{}
	rec := httptest.NewRecorder()
	invoiceContractRouter(invoices).ServeHTTP(rec, signedInvoiceRequest(http.MethodPost, "/api/v1/invoice-payments", body, requestID))
	if rec.Code != http.StatusBadRequest || invoices.calls != 0 {
		t.Fatalf("client supplied payment status accepted: status=%d calls=%d body=%s", rec.Code, invoices.calls, rec.Body.String())
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Code != "VALIDATION_ERROR" {
		t.Fatalf("unknown-property error = %q err=%v", rec.Body.String(), err)
	}
}

func TestInvoiceCreateReturnsIdempotencyConflict(t *testing.T) {
	requestID := uuid.New()
	invoices := &noopInvoices{createErr: invoiceservice.ErrIdempotencyConflict}
	rec := httptest.NewRecorder()
	invoiceContractRouter(invoices).ServeHTTP(rec, signedInvoiceRequest(http.MethodPost, "/api/v1/invoice-payments", fmtInvoiceRequest(requestID), requestID))
	if rec.Code != http.StatusConflict || invoices.calls != 1 {
		t.Fatalf("idempotency conflict response = %d calls=%d body=%s", rec.Code, invoices.calls, rec.Body.String())
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("idempotency conflict body = %q err=%v", rec.Body.String(), err)
	}
}

func TestInvoiceCreateSignatureCoversOriginalRequestBytes(t *testing.T) {
	requestID := uuid.New()
	compact := fmtInvoiceRequest(requestID)
	body := "\n" + compact
	request := signedInvoiceRequest(http.MethodPost, "/api/v1/invoice-payments", body, requestID)
	timestamp := request.Header.Get("X-Timestamp")
	request.Header.Set("X-Signature", crypto.HMACSHA256Hex("secret", []byte(timestamp+"."+compact)))
	invoices := &noopInvoices{}
	recorder := httptest.NewRecorder()
	invoiceContractRouter(invoices).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || invoices.calls != 0 {
		t.Fatalf("normalized-body signature accepted: status=%d calls=%d body=%s", recorder.Code, invoices.calls, recorder.Body.String())
	}
}

func TestInvoiceCreateAcceptsExactlyOneMiB(t *testing.T) {
	requestID := uuid.New()
	body := fmtInvoiceRequest(requestID)
	body += strings.Repeat(" ", int(maxInvoiceBodyBytes)-len(body))
	invoices := &noopInvoices{result: ports.InvoiceCreateResult{Created: true, Invoice: invoicedomain.Record{
		PaymentID: uuid.New(), Request: invoicedomain.Request{RequestID: requestID, OrderID: "DV-1042", AmountMinor: 1200000, Currency: "RUB", AcceptUntil: time.Date(2030, 10, 13, 6, 0, 0, 0, time.UTC)},
		IssuanceState: "queued", FundingState: "none", Revision: 1,
	}, Status: "creating"}}
	recorder := httptest.NewRecorder()
	invoiceContractRouter(invoices).ServeHTTP(recorder, signedInvoiceRequest(http.MethodPost, "/api/v1/invoice-payments", body, requestID))
	if recorder.Code != http.StatusAccepted || invoices.calls != 1 {
		t.Fatalf("exact 1MiB request = %d calls=%d body=%s", recorder.Code, invoices.calls, recorder.Body.String())
	}
}

func TestInvoiceResponseSupportsBankTransferWithoutPaymentURL(t *testing.T) {
	record := invoicedomain.Record{
		InvoiceNumber: 42, Reference: "INV-00000000000000000000000000000042",
		Artifact: &invoicedomain.Artifact{
			NextAction: &invoicedomain.NextAction{
				Type: "bank_transfer", Purpose: "Оплата счёта 42",
				Beneficiary: &invoicedomain.Beneficiary{
					Name: "ООО Получатель", INN: "7707654321", KPP: "770701001", Account: "00000000000000000000",
					BankBIK: "000000000", BankName: "Тестовый банк", CorrespondentAccount: "00000000000000000000",
				},
			},
			Documents: []invoicedomain.Document{},
		},
		IssuanceState: "issued", FundingState: "none", Revision: 2,
	}
	response, err := invoicePaymentResponse(record, paymentdomain.StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if response.Invoice == nil || response.Invoice.Number != "42" || response.Invoice.Reference != record.Reference || response.Invoice.NextAction == nil || response.Invoice.Documents == nil || len(response.Invoice.Documents) != 0 {
		t.Fatalf("invoice response = %+v", response.Invoice)
	}
	bankTransfer, err := response.Invoice.NextAction.AsBankTransferNextAction()
	if err != nil || bankTransfer.Purpose != "Оплата счёта 42" || bankTransfer.Beneficiary.Account != "00000000000000000000" {
		t.Fatalf("bank transfer response = %+v err=%v", bankTransfer, err)
	}
}

func fmtInvoiceRequest(requestID uuid.UUID) string {
	return strings.ReplaceAll(validInvoiceRequestTemplate, "%s", requestID.String())
}

var _ ports.InvoiceUseCase = (*sequenceInvoices)(nil)
