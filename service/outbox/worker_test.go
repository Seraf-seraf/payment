package outbox

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/google/uuid"
)

func TestProcessBatchMarksSentOnCallbackSuccess(t *testing.T) {
	t.Parallel()

	payment := paymentdomain.Payment{ID: uuid.New(), MerchantID: uuid.New(), Status: paymentdomain.StatusSucceeded}
	outboxRepo := &memoryOutboxRepo{events: []outboxdomain.Event{{
		ID:          uuid.New(),
		AggregateID: payment.ID,
		Status:      outboxdomain.StatusPending,
	}}}
	worker := NewWorker(slog.Default(), Config{BatchSize: 10, MaxAttempts: 3}, memoryMerchantRepo{}, memoryPaymentRepo{payment: payment}, outboxRepo, callbackStub{}, nil, fixedNow)

	if err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if outboxRepo.sent != 1 {
		t.Fatalf("sent = %d", outboxRepo.sent)
	}
}

func TestProcessBatchSchedulesRetryOnCallbackFailure(t *testing.T) {
	t.Parallel()

	payment := paymentdomain.Payment{ID: uuid.New(), MerchantID: uuid.New()}
	eventID := uuid.New()
	outboxRepo := &memoryOutboxRepo{events: []outboxdomain.Event{{
		ID:          eventID,
		AggregateID: payment.ID,
		Status:      outboxdomain.StatusPending,
		Attempts:    1,
	}}}
	worker := NewWorker(slog.Default(), Config{BatchSize: 10, MaxAttempts: 3}, memoryMerchantRepo{}, memoryPaymentRepo{payment: payment}, outboxRepo, callbackStub{err: errors.New("down")}, nil, fixedNow)

	if err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if outboxRepo.retryAttempts != 2 {
		t.Fatalf("retry attempts = %d", outboxRepo.retryAttempts)
	}
	if !outboxRepo.nextAttempt.After(fixedNow()) {
		t.Fatalf("next attempt = %s", outboxRepo.nextAttempt)
	}
}

type memoryMerchantRepo struct{}

func (memoryMerchantRepo) FindByAPIKeyHash(context.Context, string) (merchantdomain.Merchant, error) {
	return merchantdomain.Merchant{}, nil
}

func (memoryMerchantRepo) FindByID(context.Context, uuid.UUID) (merchantdomain.Merchant, error) {
	return merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://callback.test", SharedSecret: "secret"}, nil
}

type memoryPaymentRepo struct {
	payment paymentdomain.Payment
}

func (r memoryPaymentRepo) Create(context.Context, paymentdomain.Payment) error { return nil }
func (r memoryPaymentRepo) FindByID(context.Context, uuid.UUID) (paymentdomain.Payment, error) {
	return r.payment, nil
}
func (r memoryPaymentRepo) FindByMerchantAndIdempotencyKey(context.Context, uuid.UUID, string) (paymentdomain.Payment, error) {
	return paymentdomain.Payment{}, nil
}
func (r memoryPaymentRepo) FindByProviderPaymentID(context.Context, string, string) (paymentdomain.Payment, error) {
	return paymentdomain.Payment{}, nil
}
func (r memoryPaymentRepo) UpdateProviderData(context.Context, uuid.UUID, string, string) error {
	return nil
}
func (r memoryPaymentRepo) UpdateProviderDataAndStatus(context.Context, uuid.UUID, string, string, paymentdomain.Status, []paymentdomain.Status) error {
	return nil
}
func (r memoryPaymentRepo) UpdateStatus(context.Context, uuid.UUID, paymentdomain.Status) error {
	return nil
}
func (r memoryPaymentRepo) UpdateStatusIfAllowed(context.Context, uuid.UUID, paymentdomain.Status, []paymentdomain.Status) error {
	return nil
}

type memoryOutboxRepo struct {
	events        []outboxdomain.Event
	sent          int
	retryAttempts int
	nextAttempt   time.Time
}

func (r *memoryOutboxRepo) Create(context.Context, outboxdomain.Event) error { return nil }
func (r *memoryOutboxRepo) FetchPending(context.Context, int, time.Time) ([]outboxdomain.Event, error) {
	return r.events, nil
}
func (r *memoryOutboxRepo) MarkSent(context.Context, uuid.UUID) error {
	r.sent++
	return nil
}
func (r *memoryOutboxRepo) MarkRetry(_ context.Context, _ uuid.UUID, attempts int, nextAttemptAt time.Time, _ string) error {
	r.retryAttempts = attempts
	r.nextAttempt = nextAttemptAt
	return nil
}
func (r *memoryOutboxRepo) MarkFailed(context.Context, uuid.UUID, int, string) error { return nil }

type callbackStub struct {
	err error
}

func (s callbackStub) Send(context.Context, merchantdomain.Merchant, paymentdomain.Payment, outboxdomain.Event) error {
	return s.err
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
}
