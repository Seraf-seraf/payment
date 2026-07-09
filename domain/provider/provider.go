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
	Receipt         Receipt
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

// Receipt содержит данные онлайн-чека для провайдера.
type Receipt struct {
	Email    string
	Phone    string
	Taxation string
	Items    []ReceiptItem
}

// ReceiptItem содержит товарную позицию онлайн-чека.
type ReceiptItem struct {
	Name          string
	PriceMinor    int64
	Quantity      float64
	AmountMinor   int64
	PaymentMethod string
	PaymentObject string
	Tax           string
}

// WebhookEvent представляет нормализованное событие webhook от платежного провайдера.
type WebhookEvent struct {
	ProviderEventID   string
	ProviderPaymentID string
	EventType         string
	Status            payment.Status
}
