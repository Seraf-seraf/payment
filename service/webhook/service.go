package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	webhookdomain "github.com/Seraf-seraf/payment/domain/webhook"
	"github.com/Seraf-seraf/payment/pkg/metrics"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

// Service реализует обработку webhook от платежных провайдеров.
type Service struct {
	providers ports.ProviderRegistry
	payments  ports.PaymentRepository
	events    ports.WebhookEventRepository
	outbox    ports.OutboxRepository
	tx        ports.TransactionManager
}

var _ ports.WebhookHandler = (*Service)(nil)

// NewService создает сервис обработки webhook от платежных провайдеров.
func NewService(
	providers ports.ProviderRegistry,
	payments ports.PaymentRepository,
	events ports.WebhookEventRepository,
	outbox ports.OutboxRepository,
	tx ...ports.TransactionManager,
) *Service {
	service := &Service{
		providers: providers,
		payments:  payments,
		events:    events,
		outbox:    outbox,
	}
	if len(tx) > 0 {
		service.tx = tx[0]
	}
	return service
}

// Handle проверяет, сохраняет и применяет webhook от платежного провайдера.
func (s *Service) Handle(ctx context.Context, providerName string, headers http.Header, rawBody []byte) error {
	metrics.WebhooksReceivedTotal.Inc()
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

	if s.tx == nil {
		return s.handleEvent(ctx, s.payments, s.events, s.outbox, providerName, event, rawBody)
	}
	return s.tx.WithinTx(ctx, func(txCtx context.Context, repos ports.Repositories) error {
		return s.handleEvent(txCtx, repos.Payments, repos.WebhookEvents, repos.Outbox, providerName, event, rawBody)
	})
}

func (s *Service) handleEvent(
	ctx context.Context,
	payments ports.PaymentRepository,
	events ports.WebhookEventRepository,
	outbox ports.OutboxRepository,
	providerName string,
	event providerdomain.WebhookEvent,
	rawBody []byte,
) error {
	now := time.Now().UTC()
	created, err := events.Create(ctx, webhookdomain.Event{
		ID:                uuid.New(),
		ProviderName:      providerName,
		ProviderEventID:   event.ProviderEventID,
		ProviderPaymentID: event.ProviderPaymentID,
		EventType:         event.EventType,
		RawPayload:        json.RawMessage(append([]byte(nil), rawBody...)),
		SignatureValid:    true,
		CreatedAt:         now,
	})
	if err != nil {
		return err
	}
	if !created {
		metrics.WebhooksDuplicateTotal.Inc()
		return nil
	}

	payment, err := payments.FindByProviderPaymentID(ctx, providerName, event.ProviderPaymentID)
	if err != nil {
		return err
	}
	if event.Status == "" {
		event.Status = paymentdomain.StatusPending
	}
	if err := payments.UpdateStatusIfAllowed(ctx, payment.ID, event.Status, allowedPriorStatuses(event.Status)); err != nil {
		if errors.Is(err, paymentservice.ErrInvalidStatusTransition) {
			return events.MarkProcessed(ctx, providerName, event.ProviderEventID, now)
		}
		return err
	}
	payment.Status = event.Status
	payload, err := json.Marshal(map[string]any{
		"payment_id":   payment.ID,
		"merchant_id":  payment.MerchantID,
		"order_id":     payment.MerchantOrderID,
		"status":       payment.Status,
		"amount_minor": payment.AmountMinor,
		"currency":     payment.Currency,
	})
	if err != nil {
		return err
	}
	outboxEventType := event.EventType
	if outboxEventType == "" {
		outboxEventType = "payment." + string(event.Status)
	}
	if err := outbox.Create(ctx, outboxdomain.Event{
		ID:            uuid.New(),
		AggregateType: "payment",
		AggregateID:   payment.ID,
		EventType:     outboxEventType,
		Payload:       payload,
		Status:        outboxdomain.StatusPending,
		Attempts:      0,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}); err != nil {
		return err
	}
	switch event.Status {
	case paymentdomain.StatusSucceeded:
		metrics.PaymentsSucceededTotal.Inc()
	case paymentdomain.StatusFailed:
		metrics.PaymentsFailedTotal.Inc()
	}
	return events.MarkProcessed(ctx, providerName, event.ProviderEventID, now)
}

func allowedPriorStatuses(next paymentdomain.Status) []paymentdomain.Status {
	switch next {
	case paymentdomain.StatusPending:
		return []paymentdomain.Status{paymentdomain.StatusCreating, paymentdomain.StatusPending}
	case paymentdomain.StatusSucceeded:
		return []paymentdomain.Status{paymentdomain.StatusPending}
	case paymentdomain.StatusFailed, paymentdomain.StatusCanceled:
		return []paymentdomain.Status{paymentdomain.StatusPending}
	case paymentdomain.StatusRefunded:
		return []paymentdomain.Status{paymentdomain.StatusSucceeded}
	default:
		return []paymentdomain.Status{paymentdomain.StatusPending}
	}
}
