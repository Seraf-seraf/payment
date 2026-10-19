package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	cloudpaymentsprovider "github.com/Seraf-seraf/payment/adapter/provider/cloudpayments"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
	invoiceservice "github.com/Seraf-seraf/payment/service/invoice"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// Server реализует HTTP handlers, сгенерированные из OpenAPI контракта.
type Server struct {
	logger               *slog.Logger
	merchants            ports.MerchantAuthenticator
	payments             ports.PaymentUseCase
	invoices             ports.InvoiceUseCase
	webhooks             ports.WebhookHandler
	hmacMaxSkew          time.Duration
	invoiceNotifications ports.InvoiceNotificationUseCase
	invoiceDecoders      ports.InvoiceNotificationDecoderRegistry
}

// NewServer создает HTTP server handlers с зависимостями бизнес-слоя.
func NewServer(
	logger *slog.Logger,
	merchants ports.MerchantAuthenticator,
	payments ports.PaymentUseCase,
	invoices ports.InvoiceUseCase,
	webhooks ports.WebhookHandler,
	hmacMaxSkew time.Duration,
	invoiceWebhook ...InvoiceWebhookDependencies,
) *Server {
	server := &Server{
		logger:      logger,
		merchants:   merchants,
		payments:    payments,
		invoices:    invoices,
		webhooks:    webhooks,
		hmacMaxSkew: hmacMaxSkew,
	}
	if len(invoiceWebhook) > 0 {
		server.invoiceNotifications = invoiceWebhook[0].Notifications
		server.invoiceDecoders = invoiceWebhook[0].Decoders
	}
	return server
}

func (s *Server) CreateInvoicePayment(w http.ResponseWriter, r *http.Request, params CreateInvoicePaymentParams) {
	if r.TLS == nil && !remoteIsLoopback(r.RemoteAddr) {
		writeError(w, http.StatusBadRequest, "TLS_REQUIRED", "Для удалённых запросов требуется TLS.")
		return
	}
	rawBody, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Некорректное тело запроса.")
		return
	}
	merchant, ok := s.authenticate(w, r.Context(), CreatePaymentParams{
		XAPIKey: params.XAPIKey, XTimestamp: params.XTimestamp, XSignature: params.XSignature,
	}, rawBody)
	if !ok {
		return
	}

	var body CreateInvoicePaymentJSONRequestBody
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Некорректное тело запроса.")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Некорректное тело запроса.")
		return
	}
	requestID := uuid.UUID(body.RequestId)
	_, offset := body.AcceptUntil.Zone()
	if requestID == uuid.Nil || uuid.UUID(params.IdempotencyKey) != requestID || offset != 0 {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key должен совпадать с request_id.")
		return
	}
	request := invoicedomain.Request{
		RequestID: requestID, OrderID: body.OrderId, AmountMinor: body.AmountMinor,
		Currency: string(body.Currency), Description: body.Description,
		AcceptUntil: body.AcceptUntil,
		Payer: invoicedomain.Payer{
			Kind: string(body.Payer.Kind), Name: body.Payer.Name, INN: body.Payer.Inn,
			KPP: body.Payer.Kpp, LegalAddress: body.Payer.LegalAddress,
		},
		Lines: make([]invoicedomain.Line, 0, len(body.Lines)),
	}
	if body.Payer.Email != nil {
		request.Payer.Email = string(*body.Payer.Email)
	}
	for _, line := range body.Lines {
		request.Lines = append(request.Lines, invoicedomain.Line{
			ID: line.Id, Name: line.Name, Quantity: line.Quantity, Unit: string(line.Unit),
			UnitPriceMinor: line.UnitPriceMinor, AmountMinor: line.AmountMinor,
			VAT: invoicedomain.VAT{Kind: string(line.Vat.Kind), RateBasisPoints: line.Vat.RateBasisPoints, AmountMinor: line.Vat.AmountMinor},
		})
	}
	if s.invoices == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Выставление счетов отключено.")
		return
	}
	result, err := s.invoices.CreateInvoicePayment(r.Context(), request, merchant)
	if err != nil {
		s.writeInvoiceError(w, err, merchant.ID)
		return
	}
	response, err := invoicePaymentResponse(result.Invoice, result.Status)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("invoice response projection failed", slog.String("error_code", "invoice_projection_invalid"))
		}
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Сервис временно недоступен.")
		return
	}
	if result.Created {
		location := "/api/v1/invoice-payments/" + result.Invoice.PaymentID.String()
		w.Header().Set("Location", location)
		writeJSON(w, http.StatusAccepted, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) GetInvoicePayment(w http.ResponseWriter, r *http.Request, paymentID openapi_types.UUID, params GetInvoicePaymentParams) {
	if r.TLS == nil && !remoteIsLoopback(r.RemoteAddr) {
		writeError(w, http.StatusBadRequest, "TLS_REQUIRED", "Для удалённых запросов требуется TLS.")
		return
	}
	merchant, ok := s.authenticate(w, r.Context(), CreatePaymentParams{
		XAPIKey: params.XAPIKey, XTimestamp: params.XTimestamp, XSignature: params.XSignature,
	}, nil)
	if !ok {
		return
	}
	if s.invoices == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Выставление счетов отключено.")
		return
	}
	result, err := s.invoices.GetInvoicePayment(r.Context(), uuid.UUID(paymentID), merchant)
	if err != nil {
		s.writeInvoiceError(w, err, merchant.ID)
		return
	}
	response, err := invoicePaymentResponse(result.Invoice, result.Status)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("invoice response projection failed", slog.String("error_code", "invoice_projection_invalid"))
		}
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Сервис временно недоступен.")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) writeInvoiceError(w http.ResponseWriter, err error, merchantID uuid.UUID) {
	switch {
	case errors.Is(err, invoiceservice.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Счёт не найден.")
	case errors.Is(err, invoiceservice.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Некорректные данные счёта.")
	case errors.Is(err, invoiceservice.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "request_id уже использован с другими данными.")
	case errors.Is(err, invoiceservice.ErrUnsupportedTerms), errors.Is(err, invoiceservice.ErrConnectionUnavailable):
		writeError(w, http.StatusUnprocessableEntity, "UNSUPPORTED_INVOICE_TERMS", "Счёт с указанными условиями не поддерживается.")
	default:
		s.logger.Error("create invoice payment failed", slog.Any("error", err), slog.String("merchant_id", merchantID.String()))
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Сервис временно недоступен.")
	}
}

func invoicePaymentResponse(record invoicedomain.Record, status paymentdomain.Status) (InvoicePaymentResponse, error) {
	switch status {
	case paymentdomain.StatusCreating, paymentdomain.StatusPending, paymentdomain.StatusSucceeded, paymentdomain.StatusFailed, paymentdomain.StatusCanceled:
	default:
		return InvoicePaymentResponse{}, errors.New("invalid invoice payment status")
	}
	if !validInvoiceIssuanceState(record.IssuanceState) || !validInvoiceFundingState(record.FundingState) {
		return InvoicePaymentResponse{}, errors.New("invalid invoice state")
	}
	var artifact *InvoiceArtifact
	if record.Artifact != nil {
		var err error
		artifact, err = invoiceArtifact(record)
		if err != nil {
			return InvoicePaymentResponse{}, err
		}
	}
	reasons := append([]string(nil), record.ReviewReasons...)
	if reasons == nil {
		reasons = []string{}
	}
	confirmedPaid := record.CreditedMinor >= record.Request.AmountMinor
	return InvoicePaymentResponse{
		PaymentId: openapi_types.UUID(record.PaymentID), OrderId: record.Request.OrderID,
		PaymentMethod: LegalEntityInvoice,
		Status:        InvoicePaymentResponseStatus(status), IssuanceState: InvoicePaymentResponseIssuanceState(record.IssuanceState),
		FundingState: InvoicePaymentResponseFundingState(record.FundingState),
		AmountMinor:  record.Request.AmountMinor, CreditedMinor: record.CreditedMinor,
		Currency: InvoicePaymentResponseCurrency(record.Request.Currency), ConfirmedPaid: confirmedPaid,
		RequiresReview: len(reasons) > 0, ReviewReasons: reasons, Revision: record.Revision,
		AcceptUntil: record.Request.AcceptUntil.UTC(), PaymentConfirmedAt: utcTime(record.PaymentConfirmedAt),
		ConfirmationRecordedAt: utcTime(record.ConfirmationRecordedAt), Invoice: artifact,
	}, nil
}

func validInvoiceIssuanceState(state string) bool {
	switch state {
	case "queued", "dispatched", "issued", "unknown", "rejected", "expired_before_issue":
		return true
	default:
		return false
	}
}

func validInvoiceFundingState(state string) bool {
	switch state {
	case "none", "partial", "paid", "overpaid":
		return true
	default:
		return false
	}
}

func invoiceArtifact(record invoicedomain.Record) (*InvoiceArtifact, error) {
	stored := record.Artifact
	artifact := &InvoiceArtifact{
		Number: strconv.FormatInt(record.InvoiceNumber, 10), Reference: record.Reference,
		ProviderExpiresAt: utcTime(record.ProviderExpiresAt), Documents: make([]InvoiceDocument, 0, len(stored.Documents)),
	}
	if stored.NextAction != nil {
		action := stored.NextAction
		switch action.Type {
		case "bank_transfer":
			if action.Purpose == "" || action.Beneficiary == nil {
				return nil, errors.New("invalid bank transfer invoice action")
			}
			beneficiary := action.Beneficiary
			nextAction := &InvoiceArtifact_NextAction{}
			if err := nextAction.FromBankTransferNextAction(BankTransferNextAction{
				Type: BankTransfer, Purpose: action.Purpose,
				Beneficiary: InvoiceBeneficiary{
					Name: beneficiary.Name, Inn: beneficiary.INN, Kpp: beneficiary.KPP,
					Account: beneficiary.Account, BankBik: beneficiary.BankBIK, BankName: beneficiary.BankName,
					CorrespondentAccount: beneficiary.CorrespondentAccount,
				},
			}); err != nil {
				return nil, err
			}
			artifact.NextAction = nextAction
		case "redirect":
			parsed, err := url.Parse(action.URL)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || action.ValidUntil == nil {
				return nil, errors.New("invalid redirect invoice action")
			}
			nextAction := &InvoiceArtifact_NextAction{}
			if err := nextAction.FromRedirectNextAction(RedirectNextAction{Type: Redirect, Url: action.URL, ValidUntil: action.ValidUntil.UTC()}); err != nil {
				return nil, err
			}
			artifact.NextAction = nextAction
		default:
			return nil, errors.New("unsupported invoice action")
		}
	}
	for _, document := range stored.Documents {
		parsed, err := url.Parse(document.URL)
		if err != nil || document.Type != "invoice" || parsed.Scheme == "" || parsed.Host == "" {
			return nil, errors.New("invalid invoice document")
		}
		artifact.Documents = append(artifact.Documents, InvoiceDocument{Type: InvoiceDocumentType(document.Type), Url: document.URL, ValidUntil: utcTime(document.ValidUntil)})
	}
	return artifact, nil
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}

// GetHealth возвращает состояние HTTP API.
func (s *Server) GetHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ok"})
}

// CreatePayment обрабатывает запрос мерчанта на создание платежа.
func (s *Server) CreatePayment(w http.ResponseWriter, r *http.Request, params CreatePaymentParams) {
	rawBody, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Некорректное тело запроса.")
		return
	}

	merchant, ok := s.authenticate(w, r.Context(), params, rawBody)
	if !ok {
		return
	}

	var body CreatePaymentJSONRequestBody
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Некорректное тело запроса.")
		return
	}

	description := stringValue(body.Description)
	paymentMethod := stringValue(body.PaymentMethod)
	customerEmail := ""
	if body.Customer != nil && body.Customer.Email != nil {
		customerEmail = string(*body.Customer.Email)
	}
	idempotencyKey := ""
	if params.IdempotencyKey != nil {
		idempotencyKey = *params.IdempotencyKey
	}
	receipt := providerReceipt(body.Receipt)
	if err := validateReceipt(receipt, body.AmountMinor); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Некорректные данные чека.")
		return
	}

	result, err := s.payments.CreatePayment(r.Context(), ports.CreatePaymentRequest{
		Merchant:       merchant,
		OrderID:        body.OrderId,
		AmountMinor:    body.AmountMinor,
		Currency:       body.Currency,
		Description:    description,
		PaymentMethod:  paymentMethod,
		CustomerEmail:  customerEmail,
		Receipt:        receipt,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.logger.Error("create payment failed", slog.Any("error", err), slog.String("merchant_id", merchant.ID.String()))
		writeError(w, http.StatusBadRequest, "payment_provider_error", "Не удалось создать платеж. Попробуйте позже.")
		return
	}

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, paymentResponse(result.Payment))
}

// GetPayment возвращает платеж по идентификатору.
func (s *Server) GetPayment(w http.ResponseWriter, r *http.Request, paymentID uuid.UUID, params GetPaymentParams) {
	merchant, ok := s.authenticate(w, r.Context(), CreatePaymentParams{
		XAPIKey:    params.XAPIKey,
		XTimestamp: params.XTimestamp,
		XSignature: params.XSignature,
	}, nil)
	if !ok {
		return
	}

	payment, err := s.payments.GetPayment(r.Context(), paymentID)
	if err != nil {
		writeError(w, http.StatusNotFound, "payment_not_found", "Платеж не найден.")
		return
	}
	if payment.MerchantID != merchant.ID {
		writeError(w, http.StatusNotFound, "payment_not_found", "Платеж не найден.")
		return
	}
	writeJSON(w, http.StatusOK, paymentResponse(payment))
}

// ProviderWebhook принимает webhook от платежного провайдера.
func (s *Server) ProviderWebhook(w http.ResponseWriter, r *http.Request, providerName string) {
	if providerName == "cloudpayments" {
		cloudpaymentsprovider.HandleWebhook(w, r, s.webhooks, s.logger)
		return
	}
	rawBody, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Некорректное тело запроса.")
		return
	}
	if err := s.webhooks.Handle(r.Context(), providerName, r.Header, rawBody); err != nil {
		s.logger.Error("provider webhook failed", slog.Any("error", err), slog.String("provider_name", providerName))
		writeError(w, http.StatusBadRequest, "webhook_error", "Некорректный webhook.")
		return
	}
	if providerName == "tbank" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
		return
	}
	writeJSON(w, http.StatusOK, WebhookResponse{Status: "ok"})
}

func (s *Server) authenticate(w http.ResponseWriter, ctx context.Context, params CreatePaymentParams, rawBody []byte) (merchantdomain.Merchant, bool) {
	merchant, err := s.merchants.AuthenticateAPIKey(ctx, params.XAPIKey)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Неверные учетные данные.")
		return merchantdomain.Merchant{}, false
	}

	timestamp, err := strconv.ParseInt(params.XTimestamp, 10, 64)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Неверные учетные данные.")
		return merchantdomain.Merchant{}, false
	}
	requestTime := time.Unix(timestamp, 0)
	if time.Since(requestTime) > s.hmacMaxSkew || time.Until(requestTime) > s.hmacMaxSkew {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Неверные учетные данные.")
		return merchantdomain.Merchant{}, false
	}

	message := []byte(fmt.Sprintf("%s.%s", params.XTimestamp, rawBody))
	expected := crypto.HMACSHA256Hex(merchant.SharedSecret, message)
	if !crypto.EqualHex(params.XSignature, expected) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Неверные учетные данные.")
		return merchantdomain.Merchant{}, false
	}

	return merchant, true
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, errors.New("empty body")
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(rawBody))
	return rawBody, nil
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Code: code, Message: message})
}

func paymentResponse(payment paymentdomain.Payment) PaymentResponse {
	var paymentURL *string
	if payment.PaymentURL != "" {
		paymentURL = &payment.PaymentURL
	}
	status := payment.Status
	if status == paymentdomain.StatusCreating {
		status = paymentdomain.StatusPending
	}
	return PaymentResponse{
		PaymentId:   payment.ID,
		OrderId:     payment.MerchantOrderID,
		AmountMinor: payment.AmountMinor,
		Currency:    payment.Currency,
		Status:      PaymentStatus(status),
		PaymentUrl:  paymentURL,
	}
}

func providerReceipt(receipt Receipt) providerdomain.Receipt {
	items := make([]providerdomain.ReceiptItem, 0, len(receipt.Items))
	for _, item := range receipt.Items {
		items = append(items, providerdomain.ReceiptItem{
			Name:          item.Name,
			PriceMinor:    item.PriceMinor,
			Quantity:      item.Quantity,
			AmountMinor:   item.AmountMinor,
			PaymentMethod: stringValue(item.PaymentMethod),
			PaymentObject: stringValue(item.PaymentObject),
			Tax:           string(item.Tax),
		})
	}

	return providerdomain.Receipt{
		Email:    stringValue(receipt.Email),
		Phone:    stringValue(receipt.Phone),
		Taxation: string(receipt.Taxation),
		Items:    items,
	}
}

func validateReceipt(receipt providerdomain.Receipt, amountMinor int64) error {
	if receipt.Email == "" && receipt.Phone == "" {
		return errors.New("receipt email or phone is required")
	}
	if receipt.Taxation == "" {
		return errors.New("receipt taxation is required")
	}
	if len(receipt.Items) == 0 {
		return errors.New("receipt items are required")
	}
	var itemsAmount int64
	for _, item := range receipt.Items {
		if item.AmountMinor <= 0 || item.PriceMinor <= 0 || item.Quantity <= 0 || item.Name == "" || item.Tax == "" {
			return errors.New("invalid receipt item")
		}
		itemsAmount += item.AmountMinor
	}
	if itemsAmount != amountMinor {
		return errors.New("receipt amount mismatch")
	}
	return nil
}

func stringValue[T ~string](value *T) string {
	if value == nil {
		return ""
	}
	return string(*value)
}
