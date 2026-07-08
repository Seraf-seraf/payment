package provider

import "github.com/Seraf-seraf/payment/domain/payment"

// CreatePaymentRequest содержит данные для создания платежа у провайдера.
type CreatePaymentRequest struct {
	PaymentID       string
	OrderID         string
	AmountMinor     int64
	Currency        string
	Description     string
	PaymentMethod   string
	CustomerEmail   string
	IdempotencyKey  string
	MerchantID      string
	MerchantSuccess string
	MerchantFail    string
}

// CreatePaymentResult содержит идентификатор и ссылку оплаты, полученные от провайдера.
type CreatePaymentResult struct {
	ProviderPaymentID string
	PaymentURL        string
}

// WebhookEvent представляет нормализованное событие webhook от платежного провайдера.
type WebhookEvent struct {
	ProviderEventID   string
	ProviderPaymentID string
	EventType         string
	Status            payment.Status
}
