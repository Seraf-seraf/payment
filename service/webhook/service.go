package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	webhookdomain "github.com/Seraf-seraf/payment/domain/webhook"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

type Service struct {
	providers ports.ProviderRegistry
	payments  ports.PaymentRepository
	events    ports.WebhookEventRepository
}

var _ ports.WebhookHandler = (*Service)(nil)

func NewService(providers ports.ProviderRegistry, payments ports.PaymentRepository, events ports.WebhookEventRepository) *Service {
	return &Service{
		providers: providers,
		payments:  payments,
		events:    events,
	}
}

func (s *Service) Handle(ctx context.Context, providerName string, headers http.Header, rawBody []byte) error {
	provider, ok := s.providers.Get(providerName)
	if !ok {
		return errors.New("provider not registered")
	}
	if err := provider.VerifyWebhook(ctx, headers, rawBody); err != nil {
		return err
	}
	event, err := provider.ParseWebhook(ctx, rawBody)
	if err != nil {
		return err
	}

	created, err := s.events.Create(ctx, webhookdomain.Event{
		ID:                uuid.New(),
		ProviderName:      providerName,
		ProviderEventID:   event.ProviderEventID,
		ProviderPaymentID: event.ProviderPaymentID,
		EventType:         event.EventType,
		RawPayload:        json.RawMessage(append([]byte(nil), rawBody...)),
		SignatureValid:    true,
		CreatedAt:         time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if !created {
		return nil
	}

	payment, err := s.payments.FindByProviderPaymentID(ctx, providerName, event.ProviderPaymentID)
	if err != nil {
		return err
	}
	if event.Status == "" {
		event.Status = paymentdomain.StatusPending
	}
	if err := s.payments.UpdateStatus(ctx, payment.ID, event.Status); err != nil {
		return err
	}
	return s.events.MarkProcessed(ctx, providerName, event.ProviderEventID, time.Now().UTC())
}
