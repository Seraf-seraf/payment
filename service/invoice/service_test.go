package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

func TestCreateInvoicePaymentIsAtomicAndReplaysAfterDeadline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC)
	merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.example/callback"}
	request := validRequest()
	repositories := newMemoryRepositories()
	service := NewService(repositories.invoices, fixedConnectionRegistry{merchantID: merchant.ID}, repositories, func() time.Time { return now })

	first, err := service.CreateInvoicePayment(t.Context(), request, merchant)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Invoice.InvoiceNumber != 1 || first.Invoice.Revision != 1 {
		t.Fatalf("create result = %+v", first)
	}
	if len(repositories.payments.created) != 1 || len(repositories.commands.commands) != 1 || len(repositories.outbox.events) != 1 {
		t.Fatalf("side effects = payment %d, command %d, callback %d", len(repositories.payments.created), len(repositories.commands.commands), len(repositories.outbox.events))
	}
	payment := repositories.payments.created[0]
	if payment.ProviderName != "invoice:local-invoice" || payment.ProviderPaymentID != "" || payment.PaymentURL != "" ||
		payment.IdempotencyKey != "invoice:"+request.RequestID.String() || payment.Status != paymentdomain.StatusCreating {
		t.Fatalf("parent payment = %+v", payment)
	}
	command := repositories.commands.commands[0]
	if command.CommandType != "invoice.issue" || command.PaymentID != payment.ID || command.Status != "pending" {
		t.Fatalf("issue command = %+v", command)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(repositories.outbox.events[0].Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if repositories.outbox.events[0].AggregateType != "payment" || repositories.outbox.events[0].AggregateID != payment.ID ||
		repositories.outbox.events[0].EventType != "payment.invoice.updated" || string(envelope["revision"]) != "1" || string(envelope["reason"]) != `"created"` {
		t.Fatalf("initial callback event = %+v", repositories.outbox.events[0])
	}

	now = request.AcceptUntil.Add(time.Second)
	second, err := service.CreateInvoicePayment(t.Context(), request, merchant)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Invoice.PaymentID != first.Invoice.PaymentID {
		t.Fatalf("replay result = %+v", second)
	}
	changed := request
	changed.Description += " edited"
	if _, err := service.CreateInvoicePayment(t.Context(), changed, merchant); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed replay error = %v", err)
	}
	if len(repositories.payments.created) != 1 || len(repositories.commands.commands) != 1 || len(repositories.outbox.events) != 1 {
		t.Fatal("replay created new rows")
	}
}

func TestCreateInvoicePaymentRollsBackEveryWriteOnOutboxFailure(t *testing.T) {
	t.Parallel()

	merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.example/callback"}
	repositories := newMemoryRepositories()
	repositories.outbox.createErr = errors.New("database error")
	service := NewService(repositories.invoices, fixedConnectionRegistry{merchantID: merchant.ID}, repositories, fixedNow)
	if _, err := service.CreateInvoicePayment(t.Context(), validRequest(), merchant); err == nil {
		t.Fatal("expected transaction failure")
	}
	if len(repositories.payments.created) != 0 || len(repositories.commands.commands) != 0 || len(repositories.invoices.records) != 0 {
		t.Fatal("failed creation left partial rows")
	}
}

func TestCreateInvoicePaymentRejectsOccupiedAcquiringKey(t *testing.T) {
	t.Parallel()

	merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.example/callback"}
	request := validRequest()
	repositories := newMemoryRepositories()
	repositories.payments.occupiedKeys = map[string]bool{
		merchant.ID.String() + ":invoice:" + request.RequestID.String(): true,
	}
	service := NewService(repositories.invoices, fixedConnectionRegistry{merchantID: merchant.ID}, repositories, fixedNow)
	if _, err := service.CreateInvoicePayment(t.Context(), request, merchant); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("occupied acquiring key error = %v", err)
	}
	if len(repositories.invoices.records) != 0 || len(repositories.commands.commands) != 0 || len(repositories.outbox.events) != 0 {
		t.Fatal("key collision left invoice rows")
	}
}

func TestCreateInvoicePaymentRejectsUnsupportedConnectionTermsBeforeTransaction(t *testing.T) {
	t.Parallel()

	merchant := merchantdomain.Merchant{ID: uuid.New(), CallbackURL: "https://merchant.example/callback"}
	request := validRequest()
	vatRate := int32(2000)
	request.Lines[0].VAT.Kind = "included"
	request.Lines[0].VAT.RateBasisPoints = &vatRate
	request.Lines[0].VAT.AmountMinor = 500
	repositories := newMemoryRepositories()
	service := NewService(repositories.invoices, fixedConnectionRegistry{merchantID: merchant.ID}, repositories, fixedNow)
	if _, err := service.CreateInvoicePayment(t.Context(), request, merchant); !errors.Is(err, ErrUnsupportedTerms) {
		t.Fatalf("CreateInvoicePayment error = %v, want ErrUnsupportedTerms", err)
	}
	if repositories.txCalls != 0 || len(repositories.payments.created) != 0 {
		t.Fatalf("side effects before capability rejection: tx calls=%d payments=%d", repositories.txCalls, len(repositories.payments.created))
	}
}

func TestAcceptDeadlineCapabilityDoesNotSaturateForDistantDates(t *testing.T) {
	maxDuration := time.Duration(1<<63 - 1)
	deadline := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if supportsAcceptDeadline(fixedNow(), deadline, maxDuration) {
		t.Fatal("distant deadline passed the maximum duration check")
	}
}

func TestNormalizeInvoiceRequestMoneyBoundariesAndLineOrder(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		edit func(*invoicedomain.Request)
	}{
		{name: "zero amount", edit: func(r *invoicedomain.Request) { r.AmountMinor = 0 }},
		{name: "line multiplication mismatch", edit: func(r *invoicedomain.Request) { r.Lines[0].Quantity = 2 }},
		{name: "line multiplication overflow", edit: func(r *invoicedomain.Request) {
			r.Lines[0].Quantity = 1_000_000
			r.Lines[0].UnitPriceMinor = invoicedomain.MaxAmountMinor
			r.Lines[0].AmountMinor = invoicedomain.MaxAmountMinor
			r.AmountMinor = invoicedomain.MaxAmountMinor
		}},
		{name: "duplicate line id", edit: func(r *invoicedomain.Request) {
			duplicate := r.Lines[0]
			r.Lines = append(r.Lines, duplicate)
			r.AmountMinor *= 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := validRequest()
			test.edit(&req)
			if err := normalizeAndValidate(&req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}

	first := validRequest()
	second := validRequest()
	second.RequestID = first.RequestID
	first.Lines = append(first.Lines, invoicedomain.Line{ID: "line-2", Name: "Дополнительная услуга", Quantity: 1, Unit: "service", UnitPriceMinor: 1, AmountMinor: 1, VAT: invoicedomain.VAT{Kind: "none"}})
	first.AmountMinor++
	second.Lines = []invoicedomain.Line{first.Lines[1], first.Lines[0]}
	second.AmountMinor = first.AmountMinor
	if err := normalizeAndValidate(&first); err != nil {
		t.Fatal(err)
	}
	if err := normalizeAndValidate(&second); err != nil {
		t.Fatal(err)
	}
	firstHash, err := requestHash(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := requestHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash == secondHash {
		t.Fatal("line order did not affect the request hash")
	}
}

func validRequest() invoicedomain.Request {
	return invoicedomain.Request{
		RequestID: uuid.New(), OrderID: "DV-1042", AmountMinor: 1_200_000,
		Currency: "RUB", Description: "Оплата заказа DV-1042",
		AcceptUntil: time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC),
		Payer:       invoicedomain.Payer{Kind: "company", Name: "ООО Пример", INN: "7701234567", KPP: "770101001", LegalAddress: "Тестовый адрес", Email: "accounts@example.com"},
		Lines: []invoicedomain.Line{{
			ID: "order-line-1", Name: "Неделимый лот", Quantity: 1, Unit: "set",
			UnitPriceMinor: 1_200_000, AmountMinor: 1_200_000,
			VAT: invoicedomain.VAT{Kind: "none", RateBasisPoints: nil, AmountMinor: 0},
		}},
	}
}

func fixedNow() time.Time { return time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC) }

type fixedConnectionRegistry struct{ merchantID uuid.UUID }

func (r fixedConnectionRegistry) ForMerchant(id uuid.UUID) (invoicedomain.Connection, bool) {
	if id != r.merchantID {
		return invoicedomain.Connection{}, false
	}
	return invoicedomain.Connection{
		ID: "local-invoice", MerchantID: id, ProviderKind: "mock_invoice", EvidenceMode: "bank_credits",
		Capabilities: invoicedomain.Capabilities{
			SupportedUnits: []string{"piece", "set", "service"}, SupportsVATNone: true,
			SupportedVATRatesBPS: []int32{0, 500, 700, 1000, 2200, 10500, 10700, 11000, 12200},
			MaxAcceptDuration:    5 * 24 * time.Hour,
		},
	}, true
}

type memoryRepositories struct {
	payments *memoryPayments
	invoices *memoryInvoices
	commands *memoryCommands
	outbox   *memoryOutbox
	txCalls  int
}

func newMemoryRepositories() *memoryRepositories {
	return &memoryRepositories{payments: &memoryPayments{}, invoices: &memoryInvoices{records: map[string]invoicedomain.Record{}}, commands: &memoryCommands{}, outbox: &memoryOutbox{}}
}

func (r *memoryRepositories) ensure() {
	if r.payments == nil {
		r.payments = &memoryPayments{}
	}
	if r.invoices == nil {
		r.invoices = &memoryInvoices{records: map[string]invoicedomain.Record{}}
	}
	if r.commands == nil {
		r.commands = &memoryCommands{}
	}
	if r.outbox == nil {
		r.outbox = &memoryOutbox{}
	}
}

func (r *memoryRepositories) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	r.txCalls++
	r.ensure()
	payments := append([]paymentdomain.Payment(nil), r.payments.created...)
	commands := append([]ports.InvoiceCommand(nil), r.commands.commands...)
	events := append([]outboxdomain.Event(nil), r.outbox.events...)
	records := make(map[string]invoicedomain.Record, len(r.invoices.records))
	for key, record := range r.invoices.records {
		records[key] = record
	}
	repos := ports.Repositories{Payments: r.payments, Invoices: r.invoices, InvoiceCommands: r.commands, Outbox: r.outbox}
	if err := fn(ctx, repos); err != nil {
		r.payments.created = payments
		r.commands.commands = commands
		r.outbox.events = events
		r.invoices.records = records
		return err
	}
	return nil
}

type memoryPayments struct {
	ports.PaymentRepository
	created      []paymentdomain.Payment
	occupiedKeys map[string]bool
}

func (r *memoryPayments) Create(_ context.Context, payment paymentdomain.Payment) error {
	key := payment.MerchantID.String() + ":" + payment.IdempotencyKey
	if r.occupiedKeys[key] {
		return paymentservice.ErrAlreadyExists
	}
	for _, existing := range r.created {
		if existing.MerchantID == payment.MerchantID && existing.IdempotencyKey == payment.IdempotencyKey {
			return paymentservice.ErrAlreadyExists
		}
	}
	r.created = append(r.created, payment)
	return nil
}

type memoryInvoices struct {
	ports.InvoiceRepository
	records map[string]invoicedomain.Record
}

func (r *memoryInvoices) FindByMerchantAndRequestID(_ context.Context, merchantID, requestID uuid.UUID) (invoicedomain.Record, error) {
	record, ok := r.records[merchantID.String()+":"+requestID.String()]
	if !ok {
		return invoicedomain.Record{}, paymentservice.ErrNotFound
	}
	return record, nil
}

func (r *memoryInvoices) Create(_ context.Context, record invoicedomain.Record) (invoicedomain.Record, error) {
	key := record.MerchantID.String() + ":" + record.Request.RequestID.String()
	if _, exists := r.records[key]; exists {
		return invoicedomain.Record{}, paymentservice.ErrAlreadyExists
	}
	record.InvoiceNumber = int64(len(r.records) + 1)
	r.records[key] = record
	return record, nil
}

type memoryCommands struct {
	ports.InvoiceCommandRepository
	commands []ports.InvoiceCommand
	err      error
}

func (r *memoryCommands) Create(_ context.Context, command ports.InvoiceCommand) error {
	if r.err != nil {
		return r.err
	}
	r.commands = append(r.commands, command)
	return nil
}

type memoryOutbox struct {
	ports.OutboxRepository
	events    []outboxdomain.Event
	createErr error
}

func (r *memoryOutbox) Create(_ context.Context, event outboxdomain.Event) error {
	if r.createErr != nil {
		return r.createErr
	}
	r.events = append(r.events, event)
	return nil
}
