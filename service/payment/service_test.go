package payment

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

func TestCreatePaymentSuccessAndIdempotency(t *testing.T) {
	t.Parallel()

	repo := newMemoryPaymentRepo()
	provider := &fakeProvider{}
	service := NewService(repo, fakeRegistry{provider: provider}, fixedNow)
	merchant := merchantdomain.Merchant{ID: uuid.New(), ProviderName: "mock"}

	first, err := service.CreatePayment(t.Context(), ports.CreatePaymentRequest{
		Merchant:    merchant,
		OrderID:     "order-1",
		AmountMinor: 1000,
		Currency:    "RUB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Payment.PaymentURL == "" {
		t.Fatalf("first result = %+v", first)
	}

	second, err := service.CreatePayment(t.Context(), ports.CreatePaymentRequest{
		Merchant:    merchant,
		OrderID:     "order-1",
		AmountMinor: 1000,
		Currency:    "RUB",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Created {
		t.Fatal("second idempotent call created a payment")
	}
	if first.Payment.ID != second.Payment.ID {
		t.Fatalf("payment ids differ: %s != %s", first.Payment.ID, second.Payment.ID)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d", provider.calls)
	}
}

func TestCreatePaymentProviderErrorDoesNotLeaveInitializedDuplicate(t *testing.T) {
	t.Parallel()

	repo := newMemoryPaymentRepo()
	provider := &fakeProvider{err: errors.New("provider unavailable")}
	service := NewService(repo, fakeRegistry{provider: provider}, fixedNow)
	merchant := merchantdomain.Merchant{ID: uuid.New(), ProviderName: "mock"}

	_, err := service.CreatePayment(t.Context(), ports.CreatePaymentRequest{
		Merchant:    merchant,
		OrderID:     "order-1",
		AmountMinor: 1000,
		Currency:    "RUB",
	})
	if err == nil {
		t.Fatal("expected provider error")
	}
	existing, err := repo.FindByMerchantAndIdempotencyKey(t.Context(), merchant.ID, "order-1:1000:RUB")
	if err != nil {
		t.Fatal(err)
	}
	if existing.Status != paymentdomain.StatusFailed {
		t.Fatalf("status = %q", existing.Status)
	}
}

type fakeRegistry struct {
	provider ports.PaymentProvider
}

func (r fakeRegistry) Get(name string) (ports.PaymentProvider, bool) {
	return r.provider, true
}

type fakeProvider struct {
	calls int
	err   error
}

func (p *fakeProvider) Name() string { return "mock" }

func (p *fakeProvider) CreatePayment(_ context.Context, req providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error) {
	p.calls++
	if p.err != nil {
		return providerdomain.CreatePaymentResult{}, p.err
	}
	return providerdomain.CreatePaymentResult{
		ProviderPaymentID: "provider-" + req.PaymentID,
		PaymentURL:        "https://pay.test/" + req.PaymentID,
	}, nil
}

func (p *fakeProvider) VerifyWebhook(_ context.Context, _ http.Header, _ []byte) error {
	return nil
}

func (p *fakeProvider) ParseWebhook(_ context.Context, _ []byte) (providerdomain.WebhookEvent, error) {
	return providerdomain.WebhookEvent{}, nil
}

type memoryPaymentRepo struct {
	byID          map[uuid.UUID]paymentdomain.Payment
	byIdempotency map[string]uuid.UUID
	byProvider    map[string]uuid.UUID
}

func newMemoryPaymentRepo() *memoryPaymentRepo {
	return &memoryPaymentRepo{
		byID:          map[uuid.UUID]paymentdomain.Payment{},
		byIdempotency: map[string]uuid.UUID{},
		byProvider:    map[string]uuid.UUID{},
	}
}

func (r *memoryPaymentRepo) clone() *memoryPaymentRepo {
	cloned := newMemoryPaymentRepo()
	for id, payment := range r.byID {
		cloned.byID[id] = payment
	}
	for key, id := range r.byIdempotency {
		cloned.byIdempotency[key] = id
	}
	for key, id := range r.byProvider {
		cloned.byProvider[key] = id
	}
	return cloned
}

func (r *memoryPaymentRepo) restore(snapshot *memoryPaymentRepo) {
	r.byID = snapshot.byID
	r.byIdempotency = snapshot.byIdempotency
	r.byProvider = snapshot.byProvider
}

func (r *memoryPaymentRepo) Create(_ context.Context, payment paymentdomain.Payment) error {
	key := payment.MerchantID.String() + ":" + payment.IdempotencyKey
	if _, ok := r.byIdempotency[key]; ok {
		return ErrAlreadyExists
	}
	r.byID[payment.ID] = payment
	r.byIdempotency[key] = payment.ID
	return nil
}

func (r *memoryPaymentRepo) FindByID(_ context.Context, id uuid.UUID) (paymentdomain.Payment, error) {
	payment, ok := r.byID[id]
	if !ok {
		return paymentdomain.Payment{}, ErrNotFound
	}
	return payment, nil
}

func (r *memoryPaymentRepo) FindByMerchantAndIdempotencyKey(_ context.Context, merchantID uuid.UUID, key string) (paymentdomain.Payment, error) {
	id, ok := r.byIdempotency[merchantID.String()+":"+key]
	if !ok {
		return paymentdomain.Payment{}, ErrNotFound
	}
	return r.byID[id], nil
}

func (r *memoryPaymentRepo) FindByProviderPaymentID(_ context.Context, providerName, providerPaymentID string) (paymentdomain.Payment, error) {
	id, ok := r.byProvider[providerName+":"+providerPaymentID]
	if !ok {
		return paymentdomain.Payment{}, ErrNotFound
	}
	return r.byID[id], nil
}

func (r *memoryPaymentRepo) UpdateProviderData(_ context.Context, id uuid.UUID, providerPaymentID, paymentURL string) error {
	payment, ok := r.byID[id]
	if !ok {
		return ErrNotFound
	}
	payment.ProviderPaymentID = providerPaymentID
	payment.PaymentURL = paymentURL
	r.byID[id] = payment
	r.byProvider[payment.ProviderName+":"+providerPaymentID] = id
	return nil
}

func (r *memoryPaymentRepo) UpdateProviderDataAndStatus(_ context.Context, id uuid.UUID, providerPaymentID, paymentURL string, status paymentdomain.Status, allowedCurrent []paymentdomain.Status) error {
	payment, ok := r.byID[id]
	if !ok {
		return ErrNotFound
	}
	if !statusAllowed(payment.Status, allowedCurrent) {
		return ErrInvalidStatusTransition
	}
	payment.ProviderPaymentID = providerPaymentID
	payment.PaymentURL = paymentURL
	payment.Status = status
	r.byID[id] = payment
	r.byProvider[payment.ProviderName+":"+providerPaymentID] = id
	return nil
}

func (r *memoryPaymentRepo) UpdateStatus(_ context.Context, id uuid.UUID, status paymentdomain.Status) error {
	payment, ok := r.byID[id]
	if !ok {
		return ErrNotFound
	}
	payment.Status = status
	r.byID[id] = payment
	return nil
}

func (r *memoryPaymentRepo) UpdateStatusIfAllowed(_ context.Context, id uuid.UUID, status paymentdomain.Status, allowedCurrent []paymentdomain.Status) error {
	payment, ok := r.byID[id]
	if !ok {
		return ErrNotFound
	}
	if !statusAllowed(payment.Status, allowedCurrent) {
		return ErrInvalidStatusTransition
	}
	payment.Status = status
	r.byID[id] = payment
	return nil
}

func fixedNow() time.Time {
	return time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
}

func statusAllowed(status paymentdomain.Status, allowed []paymentdomain.Status) bool {
	for _, allowedStatus := range allowed {
		if status == allowedStatus {
			return true
		}
	}
	return false
}
