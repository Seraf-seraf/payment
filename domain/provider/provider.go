package provider

import "github.com/Seraf-seraf/payment/domain/payment"

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

type CreatePaymentResult struct {
	ProviderPaymentID string
	PaymentURL        string
}

type WebhookEvent struct {
	ProviderEventID   string
	ProviderPaymentID string
	EventType         string
	Status            payment.Status
}
