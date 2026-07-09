package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

// Server реализует HTTP handlers, сгенерированные из OpenAPI контракта.
type Server struct {
	logger      *slog.Logger
	merchants   ports.MerchantAuthenticator
	payments    ports.PaymentUseCase
	webhooks    ports.WebhookHandler
	hmacMaxSkew time.Duration
}

// NewServer создает HTTP server handlers с зависимостями бизнес-слоя.
func NewServer(
	logger *slog.Logger,
	merchants ports.MerchantAuthenticator,
	payments ports.PaymentUseCase,
	webhooks ports.WebhookHandler,
	hmacMaxSkew time.Duration,
) *Server {
	return &Server{
		logger:      logger,
		merchants:   merchants,
		payments:    payments,
		webhooks:    webhooks,
		hmacMaxSkew: hmacMaxSkew,
	}
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
