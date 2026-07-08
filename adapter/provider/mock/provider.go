package mock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
)

type Provider struct {
	webhookSecret string
}

var _ ports.PaymentProvider = (*Provider)(nil)

func New(webhookSecret string) *Provider {
	return &Provider{webhookSecret: webhookSecret}
}

func (p *Provider) Name() string {
	return "mock"
}

func (p *Provider) CreatePayment(_ context.Context, req providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error) {
	providerPaymentID := "mock_" + req.PaymentID
	return providerdomain.CreatePaymentResult{
		ProviderPaymentID: providerPaymentID,
		PaymentURL:        fmt.Sprintf("https://mock.payments.local/pay/%s", providerPaymentID),
	}, nil
}

func (p *Provider) VerifyWebhook(_ context.Context, headers http.Header, rawBody []byte) error {
	if p.webhookSecret == "" {
		return nil
	}
	signature := headers.Get("X-Mock-Signature")
	expected := crypto.HMACSHA256Hex(p.webhookSecret, rawBody)
	if !crypto.EqualHex(signature, expected) {
		return errors.New("invalid provider signature")
	}
	return nil
}

func (p *Provider) ParseWebhook(_ context.Context, rawBody []byte) (providerdomain.WebhookEvent, error) {
	var payload struct {
		EventID           string               `json:"event_id"`
		ProviderPaymentID string               `json:"provider_payment_id"`
		EventType         string               `json:"event_type"`
		Status            paymentdomain.Status `json:"status"`
	}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		return providerdomain.WebhookEvent{}, err
	}
	if payload.EventID == "" || payload.ProviderPaymentID == "" || payload.Status == "" {
		return providerdomain.WebhookEvent{}, errors.New("invalid webhook payload")
	}
	return providerdomain.WebhookEvent{
		ProviderEventID:   payload.EventID,
		ProviderPaymentID: payload.ProviderPaymentID,
		EventType:         payload.EventType,
		Status:            payload.Status,
	}, nil
}
