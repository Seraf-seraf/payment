package webhook

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	webhookdomain "github.com/Seraf-seraf/payment/domain/webhook"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

func TestWebhookSucceededCreatesOutbox(t *testing.T) {
	t.Parallel()

	paymentID := uuid.New()
	repo := &webhookPaymentRepo{
		payment: paymentdomain.Payment{
			ID:                paymentID,
			MerchantID:        uuid.New(),
			ProviderName:      "mock",
			ProviderPaymentID: "provider-1",
			MerchantOrderID:   "order-1",
			AmountMinor:       1000,
			Currency:          "RUB",
			Status:            paymentdomain.StatusPending,
		},
	}
	events := newWebhookEventRepo()
	outbox := &webhookOutboxRepo{}
	service := NewService(webhookRegistry{event: providerdomain.WebhookEvent{
		ProviderEventID:   "event-1",
		ProviderPaymentID: "provider-1",
		EventType:         "payment.succeeded",
		Status:            paymentdomain.StatusSucceeded,
	}}, repo, events, outbox)

	if err := service.Handle(t.Context(), "mock", nil, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if repo.payment.Status != paymentdomain.StatusSucceeded {
		t.Fatalf("status = %q", repo.payment.Status)
	}
	if len(outbox.events) != 1 {
		t.Fatalf("outbox events = %d", len(outbox.events))
	}
	if events.processed != 1 {
		t.Fatalf("processed = %d", events.processed)
	}
}

func TestWebhookDuplicateDoesNotCreateOutbox(t *testing.T) {
	t.Parallel()

	repo := &webhookPaymentRepo{payment: paymentdomain.Payment{
		ID:                uuid.New(),
		ProviderName:      "mock",
		ProviderPaymentID: "provider-1",
		Status:            paymentdomain.StatusPending,
	}}
	events := newWebhookEventRepo()
	outbox := &webhookOutboxRepo{}
	service := NewService(webhookRegistry{event: providerdomain.WebhookEvent{
		ProviderEventID:   "event-1",
		ProviderPaymentID: "provider-1",
		Status:            paymentdomain.StatusSucceeded,
	}}, repo, events, outbox)

	if err := service.Handle(t.Context(), "mock", nil, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := service.Handle(t.Context(), "mock", nil, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if len(outbox.events) != 1 {
		t.Fatalf("outbox events = %d", len(outbox.events))
	}
}

func TestWebhookInvalidSignature(t *testing.T) {
	t.Parallel()

	service := NewService(webhookRegistry{err: errors.New("invalid signature")}, &webhookPaymentRepo{}, newWebhookEventRepo(), &webhookOutboxRepo{})

	if err := service.Handle(t.Context(), "mock", nil, []byte(`{}`)); err == nil {
		t.Fatal("expected invalid signature error")
	}
}

func TestWebhookInvalidTransitionDoesNotCreateOutbox(t *testing.T) {
	t.Parallel()

	repo := &webhookPaymentRepo{payment: paymentdomain.Payment{
		ID:                uuid.New(),
		ProviderName:      "mock",
		ProviderPaymentID: "provider-1",
		Status:            paymentdomain.StatusSucceeded,
	}}
	events := newWebhookEventRepo()
	outbox := &webhookOutboxRepo{}
	service := NewService(webhookRegistry{event: providerdomain.WebhookEvent{
		ProviderEventID:   "event-1",
		ProviderPaymentID: "provider-1",
		Status:            paymentdomain.StatusFailed,
	}}, repo, events, outbox)

	if err := service.Handle(t.Context(), "mock", nil, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if repo.payment.Status != paymentdomain.StatusSucceeded {
		t.Fatalf("status = %q", repo.payment.Status)
	}
	if len(outbox.events) != 0 {
		t.Fatalf("outbox events = %d", len(outbox.events))
	}
	if events.processed != 1 {
		t.Fatalf("processed = %d", events.processed)
	}
}

type webhookRegistry struct {
	event providerdomain.WebhookEvent
	err   error
}

func (r webhookRegistry) Get(name string) (ports.PaymentProvider, bool) {
	return webhookProvider{event: r.event, err: r.err}, true
}

type webhookProvider struct {
	event providerdomain.WebhookEvent
	err   error
}

func (p webhookProvider) Name() string { return "mock" }

func (p webhookProvider) CreatePayment(context.Context, providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error) {
	return providerdomain.CreatePaymentResult{}, nil
}

func (p webhookProvider) VerifyWebhook(context.Context, http.Header, []byte) error {
	return p.err
}

func (p webhookProvider) ParseWebhook(context.Context, []byte) (providerdomain.WebhookEvent, error) {
	return p.event, nil
}

type webhookPaymentRepo struct {
	payment paymentdomain.Payment
}

func (r *webhookPaymentRepo) Create(context.Context, paymentdomain.Payment) error { return nil }
func (r *webhookPaymentRepo) FindByID(context.Context, uuid.UUID) (paymentdomain.Payment, error) {
	return r.payment, nil
}
func (r *webhookPaymentRepo) FindByMerchantAndIdempotencyKey(context.Context, uuid.UUID, string) (paymentdomain.Payment, error) {
	return paymentdomain.Payment{}, paymentservice.ErrNotFound
}
func (r *webhookPaymentRepo) FindByProviderPaymentID(_ context.Context, _, _ string) (paymentdomain.Payment, error) {
	return r.payment, nil
}
func (r *webhookPaymentRepo) UpdateProviderData(context.Context, uuid.UUID, string, string) error {
	return nil
}
func (r *webhookPaymentRepo) UpdateProviderDataAndStatus(context.Context, uuid.UUID, string, string, paymentdomain.Status, []paymentdomain.Status) error {
	return nil
}
func (r *webhookPaymentRepo) UpdateStatus(_ context.Context, _ uuid.UUID, status paymentdomain.Status) error {
	r.payment.Status = status
	return nil
}
func (r *webhookPaymentRepo) UpdateStatusIfAllowed(_ context.Context, _ uuid.UUID, status paymentdomain.Status, allowed []paymentdomain.Status) error {
	for _, allowedStatus := range allowed {
		if r.payment.Status == allowedStatus {
			r.payment.Status = status
			return nil
		}
	}
	return paymentservice.ErrInvalidStatusTransition
}

type webhookEventRepo struct {
	seen      map[string]bool
	processed int
}

func newWebhookEventRepo() *webhookEventRepo {
	return &webhookEventRepo{seen: map[string]bool{}}
}

func (r *webhookEventRepo) Create(_ context.Context, event webhookdomain.Event) (bool, error) {
	key := event.ProviderName + ":" + event.ProviderEventID
	if r.seen[key] {
		return false, nil
	}
	r.seen[key] = true
	return true, nil
}

func (r *webhookEventRepo) MarkProcessed(context.Context, string, string, time.Time) error {
	r.processed++
	return nil
}

type webhookOutboxRepo struct {
	events []outboxdomain.Event
}

func (r *webhookOutboxRepo) Create(_ context.Context, event outboxdomain.Event) error {
	r.events = append(r.events, event)
	return nil
}
func (r *webhookOutboxRepo) FetchPending(context.Context, int, time.Time) ([]outboxdomain.Event, error) {
	return nil, nil
}
func (r *webhookOutboxRepo) MarkSent(context.Context, uuid.UUID) error { return nil }
func (r *webhookOutboxRepo) MarkRetry(context.Context, uuid.UUID, int, time.Time, string) error {
	return nil
}
func (r *webhookOutboxRepo) MarkFailed(context.Context, uuid.UUID, int, string) error { return nil }
