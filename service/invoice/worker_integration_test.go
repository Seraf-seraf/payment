//go:build functional

package invoice_test

import (
	"context"
	"crypto/sha256"
	"errors"
	mock "github.com/Seraf-seraf/payment/adapter/provider/mockinvoice"
	pg "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	merchant "github.com/Seraf-seraf/payment/domain/merchant"
	outbox "github.com/Seraf-seraf/payment/domain/outbox"
	"github.com/Seraf-seraf/payment/internal/testpostgres"
	"github.com/Seraf-seraf/payment/ports"
	service "github.com/Seraf-seraf/payment/service/invoice"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) { os.Exit(testpostgres.Run(m)) }

type testRegistry struct {
	connection d.Connection
	provider   ports.InvoiceProvider
}

func (r *testRegistry) ForMerchant(id uuid.UUID) (d.Connection, bool) {
	return r.connection, r.connection.MerchantID == id
}
func (r *testRegistry) ForConnection(id string) (d.Connection, ports.InvoiceProvider, bool) {
	return r.connection, r.provider, id == r.connection.ID
}

type providerStub struct {
	capabilities              d.Capabilities
	issues, recoveries, reads int
	issue                     func(context.Context, d.Record) (d.IssueResult, error)
	recover                   func(context.Context, d.Record) (d.Recovery, error)
	read                      func(context.Context, d.Record) ([]d.Observation, error)
}

func (p *providerStub) Capabilities() d.Capabilities { return p.capabilities }

func (p *providerStub) Issue(ctx context.Context, v d.Record) (d.IssueResult, error) {
	p.issues++
	if p.issue != nil {
		return p.issue(ctx, v)
	}
	return mock.New().Issue(ctx, v)
}
func (p *providerStub) Recover(ctx context.Context, v d.Record) (d.Recovery, error) {
	p.recoveries++
	if p.recover != nil {
		return p.recover(ctx, v)
	}
	return mock.New().Recover(ctx, v)
}
func (p *providerStub) Reconcile(ctx context.Context, v d.Record) ([]d.Observation, error) {
	p.reads++
	if p.read != nil {
		return p.read(ctx, v)
	}
	return nil, nil
}

type harness struct {
	pool     *pgxpool.Pool
	tx       ports.TransactionManager
	registry *testRegistry
	now      time.Time
	worker   *service.Worker
	record   d.Record
}

func newHarness(t *testing.T, mode, replay string) *harness {
	t.Helper()
	h := newHarnessWithoutInvoice(t, mode, replay)
	h.createInvoice(t)
	return h
}

func newHarnessWithoutInvoice(t *testing.T, mode, replay string) *harness {
	t.Helper()
	pool, _ := testpostgres.Schema(t, "../../migrations")
	ctx := t.Context()
	h := &harness{pool: pool, tx: pg.NewTransactionManager(pool, ""), now: time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC)}
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO merchants(id,name,api_key_hash,shared_secret_encrypted,callback_url,provider_name) VALUES($1,'test',$2,'test','https://merchant.example/callback','mock')`, id, uuid.NewString())
	must(t, err)
	h.registry = &testRegistry{connection: d.Connection{ID: "test-connection", MerchantID: id, ProviderKind: "mock_invoice", EvidenceMode: mode, Capabilities: d.Capabilities{SupportedUnits: []string{"set"}, SupportsVATNone: true, MaxAcceptDuration: 120 * time.Hour, CreationReplay: replay}, Beneficiary: d.Beneficiary{Name: "mock", INN: "7707654321", KPP: "770701001", Account: "00000000000000000000", BankBIK: "000000000", BankName: "mock", CorrespondentAccount: "00000000000000000000"}}, provider: &providerStub{}}
	h.registry.provider.(*providerStub).capabilities = h.registry.connection.Capabilities
	h.worker, err = service.NewWorker(h.tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	return h
}

func (h *harness) createInvoice(t *testing.T) {
	t.Helper()
	req := d.Request{RequestID: uuid.New(), OrderID: "order", AmountMinor: 10000, Currency: "RUB", Description: "test", AcceptUntil: h.now.Add(24 * time.Hour), Payer: d.Payer{Kind: "company", Name: "mock", INN: "7701234567", KPP: "770101001", LegalAddress: "mock"}, Lines: []d.Line{{ID: "lot", Name: "lot", Quantity: 1, Unit: "set", UnitPriceMinor: 10000, AmountMinor: 10000, VAT: d.VAT{Kind: "none"}}}}
	result, err := service.NewService(pg.NewInvoiceRepository(h.pool), h.registry, h.tx, func() time.Time { return h.now }).CreateInvoicePayment(t.Context(), req, merchant.Merchant{ID: h.registry.connection.MerchantID, CallbackURL: "https://merchant.example/callback"})
	must(t, err)
	h.record = result.Invoice
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (h *harness) run(t *testing.T) { t.Helper(); must(t, h.worker.RunOnce(t.Context())) }
func (h *harness) load(t *testing.T) d.Record {
	t.Helper()
	var v d.Record
	must(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		var err error
		v, err = r.Invoices.Lock(ctx, h.record.PaymentID)
		return err
	}))
	return v
}
func (h *harness) inbox(t *testing.T, o d.Observation) uuid.UUID {
	t.Helper()
	id := uuid.New()
	must(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		created, err := r.InvoiceInbox.Create(ctx, d.Inbox{ID: id, ConnectionID: h.registry.connection.ID, EventKey: uuid.NewString(), RawBodyHash: sha256.Sum256([]byte(id.String())), Observation: o, VerificationKind: "verified_mock", ReceivedAt: h.now})
		if err != nil {
			return err
		}
		if !created {
			return errors.New("inbox not created")
		}
		return r.InvoiceCommands.Create(ctx, ports.InvoiceCommand{ID: uuid.New(), CommandType: "invoice.apply_notification", InboxID: id, Status: "pending", NextAttemptAt: h.now, CreatedAt: h.now, UpdatedAt: h.now})
	}))
	return id
}
func (h *harness) credit(key string, amount int64) d.Observation {
	return d.Observation{CanonicalFinancialKey: true, Kind: "settlement", Reference: h.record.Reference, FinancialKey: key, EvidenceMode: h.record.EvidenceMode, AmountMinor: amount, Currency: "RUB", RecipientAccount: h.registry.connection.Beneficiary.Account, PayerINN: h.record.Request.Payer.INN, FinalIncoming: true}
}
func (h *harness) assertStatus(t *testing.T, want string) {
	t.Helper()
	var status string
	must(t, h.pool.QueryRow(t.Context(), "SELECT status FROM payments WHERE id=$1", h.record.PaymentID).Scan(&status))
	if status != want {
		t.Fatalf("payment status=%s want=%s", status, want)
	}
}

func TestFunctionalInvoiceWorkerDispatchRecoverAndNeverBlindlyReissue(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	stub := h.registry.provider.(*providerStub)
	stub.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		stored := h.load(t)
		if stored.DispatchedAt == nil || stored.IssuanceState != "dispatched" {
			t.Fatal("Issue called before durable dispatch")
		}
		return d.IssueResult{Outcome: d.OutcomeUnknown}, context.DeadlineExceeded
	}
	stub.recover = func(ctx context.Context, v d.Record) (d.Recovery, error) { return d.Recovery{Found: false}, nil }
	h.run(t)
	v := h.load(t)
	if v.IssuanceState != "unknown" || len(v.ReviewReasons) != 1 {
		t.Fatalf("unknown projection=%+v", v)
	}
	h.assertStatus(t, "creating")
	h.now = h.now.Add(10 * time.Minute)
	h.run(t)
	if stub.issues != 1 || stub.recoveries != 1 {
		t.Fatalf("calls issue=%d recover=%d", stub.issues, stub.recoveries)
	}
	stub.recover = nil
	h.now = h.now.Add(10 * time.Minute)
	h.run(t)
	v = h.load(t)
	if v.IssuanceState != "issued" || len(v.ReviewReasons) != 0 {
		t.Fatalf("recovery=%+v", v)
	}
	h.assertStatus(t, "pending")
}

func TestFunctionalInvoiceCommandConcurrentClaimAndFencing(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	var all []ports.InvoiceCommand
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := h.tx.WithinTx(context.Background(), func(ctx context.Context, r ports.Repositories) error {
				commands, err := r.InvoiceCommands.Claim(ctx, h.now, 1, 60*time.Second)
				mu.Lock()
				all = append(all, commands...)
				mu.Unlock()
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(all) != 1 {
		t.Fatalf("claimed %d commands", len(all))
	}
	old := all[0]
	h.now = h.now.Add(60 * time.Second)
	var current ports.InvoiceCommand
	must(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		commands, err := r.InvoiceCommands.Claim(ctx, h.now, 1, 60*time.Second)
		if err == nil {
			current = commands[0]
		}
		return err
	}))
	if current.LeaseToken == old.LeaseToken || current.Attempts != 2 {
		t.Fatal("reclaim did not change token/attempt")
	}
	err := h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		return r.InvoiceCommands.Finish(ctx, old, "completed", h.now, "", h.now)
	})
	if !errors.Is(err, ports.ErrLeaseLost) {
		t.Fatalf("stale finish=%v", err)
	}
	must(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		return r.InvoiceCommands.Finish(ctx, current, "completed", h.now, "", h.now)
	}))
}

func TestFunctionalInvoiceWorkerFinancialDedupAndLateMoney(t *testing.T) {
	h := newHarness(t, "bank_credits", "provider_idempotent")
	h.run(t)
	h.inbox(t, h.credit("operation-1", 4000))
	h.run(t)
	if v := h.load(t); v.CreditedMinor != 4000 || v.FundingState != "partial" {
		t.Fatalf("partial=%+v", v)
	}
	h.assertStatus(t, "pending")
	before := h.load(t).Revision
	h.inbox(t, h.credit("operation-1", 4000))
	h.run(t)
	if h.load(t).Revision != before {
		t.Fatal("duplicate changed revision")
	}
	changed := h.credit("operation-1", 5000)
	h.inbox(t, changed)
	h.run(t)
	if h.load(t).CreditedMinor != 4000 {
		t.Fatal("changed operation credited twice")
	}
	h.now = h.record.Request.AcceptUntil
	h.inbox(t, h.credit("operation-2", 6000))
	h.run(t)
	v := h.load(t)
	if v.CreditedMinor != 10000 || v.ConfirmationRecordedAt == nil || len(v.ReviewReasons) < 2 {
		t.Fatalf("late payment=%+v", v)
	}
	h.assertStatus(t, "succeeded")
	if h.registry.provider.(*providerStub).issues != 1 || h.count(t, "SELECT count(*) FROM payments") != 1 {
		t.Fatal("coverage triggered another payment")
	}
	var commandsInOutbox int
	must(t, h.pool.QueryRow(t.Context(), "SELECT count(*) FROM outbox_events WHERE aggregate_type<>'payment'").Scan(&commandsInOutbox))
	if commandsInOutbox != 0 {
		t.Fatal("commands leaked into callback outbox")
	}
}

func TestFunctionalInvoiceWorkerUnmatchedSurvivesBindingLostWakeup(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	o := h.credit("early-credit", 10000)
	o.ExternalInvoiceID = "external-before-binding"
	id := h.inbox(t, o)
	// Defer Issue so Apply durably records unmatched before binding.
	_, err := h.pool.Exec(t.Context(), "UPDATE invoice_commands SET next_attempt_at=$1 WHERE command_type='invoice.issue'", h.now.Add(time.Hour))
	must(t, err)
	h.run(t)
	var state string
	must(t, h.pool.QueryRow(t.Context(), "SELECT state FROM invoice_inbox WHERE id=$1", id).Scan(&state))
	if state != "unmatched" {
		t.Fatalf("state=%s", state)
	}
	if h.load(t).CreditedMinor != 0 || h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.apply_notification' AND status='completed'") != 1 {
		t.Fatal("early apply credited unmatched observation or stayed active")
	}
	// Simulate committed binding followed by a crash before fast-path wakeup.
	must(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		v, err := r.Invoices.Lock(ctx, h.record.PaymentID)
		if err != nil {
			return err
		}
		v.ExternalInvoiceID = o.ExternalInvoiceID
		v.IssuanceState = "issued"
		v.UpdatedAt = h.now
		return r.Invoices.Save(ctx, v)
	}))
	h.now = h.now.Add(time.Minute)
	h.worker, err = service.NewWorker(h.tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	h.run(t)
	h.assertStatus(t, "succeeded")
	must(t, h.pool.QueryRow(t.Context(), "SELECT state FROM invoice_inbox WHERE id=$1", id).Scan(&state))
	if state != "processed" {
		t.Fatalf("state after sweep=%s", state)
	}
	var applyCount int
	must(t, h.pool.QueryRow(t.Context(), "SELECT count(*) FROM invoice_commands WHERE inbox_id=$1", id).Scan(&applyCount))
	if applyCount != 2 {
		t.Fatalf("apply count=%d want=2", applyCount)
	}
}

func TestFunctionalInvoiceWorkerReconciliationUsesSameFinancialKey(t *testing.T) {
	h := newHarness(t, "bank_credits", "provider_idempotent")
	h.run(t)
	o := h.credit("readback-operation", 10000)
	stub := h.registry.provider.(*providerStub)
	stub.read = func(ctx context.Context, v d.Record) ([]d.Observation, error) { return []d.Observation{o}, nil }
	h.now = h.now.Add(10 * time.Minute)
	h.run(t)
	h.run(t)
	h.assertStatus(t, "succeeded")
	before := h.load(t).Revision
	h.inbox(t, o)
	h.run(t)
	if h.load(t).Revision != before {
		t.Fatal("webhook duplicate after reconciliation changed revision")
	}
	if h.load(t).NextReconcileAt != nil {
		t.Fatal("paid invoice kept reconciliation schedule")
	}
}

func TestFunctionalInvoiceWorkerExpiredBeforeIssueAndContextCancellation(t *testing.T) {
	h := newHarness(t, "bank_credits", "provider_idempotent")
	h.now = h.record.Request.AcceptUntil
	h.run(t)
	if h.registry.provider.(*providerStub).issues != 0 {
		t.Fatal("expired invoice was issued")
	}
	h.assertStatus(t, "canceled")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(h.worker.RunOnce(ctx), context.Canceled) {
		t.Fatal("worker ignored cancellation")
	}
}

// failOutboxTx models the provider reply succeeding but its local transaction rolling back.
type failOutboxTx struct {
	base ports.TransactionManager
	fail bool
}

func (f *failOutboxTx) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return f.base.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		r.Outbox = &failureOutbox{OutboxRepository: r.Outbox, owner: f}
		return fn(ctx, r)
	})
}

type failureOutbox struct {
	ports.OutboxRepository
	owner *failOutboxTx
}

func (f *failureOutbox) Create(ctx context.Context, event outbox.Event) error {
	if f.owner.fail {
		f.owner.fail = false
		return errors.New("injected result commit path failure")
	}
	return f.OutboxRepository.Create(ctx, event)
}

func TestFunctionalInvoiceWorkerLostSuccessfulReplyRecoversAndKeepsConfirmedMoney(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	wrapper := &failOutboxTx{base: h.tx, fail: true}
	var err error
	h.worker, err = service.NewWorker(wrapper, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	if err := h.worker.RunOnce(t.Context()); err == nil {
		t.Fatal("injected result persistence failure not observed")
	}
	v := h.load(t)
	if v.DispatchedAt == nil || v.ExternalInvoiceID != "" || v.IssuanceState != "unknown" {
		t.Fatalf("lost reply=%+v", v)
	}
	// Financial evidence arrives by exact reference while the issuance result is unknown.
	h.inbox(t, h.credit("already-paid", 10000))
	h.run(t)
	h.assertStatus(t, "succeeded")
	h.now = h.now.Add(time.Second)
	h.run(t)
	v = h.load(t)
	stub := h.registry.provider.(*providerStub)
	if stub.issues != 1 || stub.recoveries != 1 || v.CreditedMinor != 10000 || v.ExternalInvoiceID == "" {
		t.Fatalf("recovered result=%+v calls=%d/%d", v, stub.issues, stub.recoveries)
	}
	h.assertStatus(t, "succeeded")
}

func TestFunctionalInvoiceWorkerProviderIdempotentReplayKeepsIdentity(t *testing.T) {
	h := newHarness(t, "bank_credits", "provider_idempotent")
	stub := h.registry.provider.(*providerStub)
	var first d.Record
	stub.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		if stub.issues == 1 {
			first = v
			return d.IssueResult{Outcome: d.OutcomeUnknown}, nil
		}
		if first.PaymentID != v.PaymentID || first.Reference != v.Reference || first.Request.RequestID != v.Request.RequestID || first.RequestHash != v.RequestHash || first.InvoiceNumber != v.InvoiceNumber {
			t.Fatal("provider retry changed immutable creation identity")
		}
		return mock.New().Issue(ctx, v)
	}
	h.run(t)
	h.now = h.now.Add(time.Second)
	h.run(t)
	if stub.issues != 2 || stub.recoveries != 0 || h.load(t).IssuanceState != "issued" {
		t.Fatal("provider idempotent replay failed")
	}
}

func TestFunctionalInvoiceWorkerBoundedInfrastructureRetryAndSafeTerminalCode(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	stub := h.registry.provider.(*providerStub)
	stub.read = func(ctx context.Context, v d.Record) ([]d.Observation, error) {
		return nil, errors.New("bank response containing sensitive details")
	}
	hint := d.Observation{Kind: "status_hint", ExternalInvoiceID: h.load(t).ExternalInvoiceID, Reference: h.record.Reference}
	h.inbox(t, hint)
	h.run(t)
	for _, advance := range []time.Duration{0, time.Second, 4 * time.Second} {
		h.now = h.now.Add(advance)
		if err := h.worker.RunOnce(t.Context()); err == nil {
			t.Fatal("infrastructure failure not reported")
		}
	}
	var status, code string
	var attempts int
	must(t, h.pool.QueryRow(t.Context(), "SELECT status,attempts,last_error_code FROM invoice_commands WHERE command_type='invoice.reconcile' AND payment_id=$1", h.record.PaymentID).Scan(&status, &attempts, &code))
	if status != "failed" || attempts != 3 || code != "invoice_infrastructure_failed" {
		t.Fatalf("terminal command status=%s attempts=%d code=%s", status, attempts, code)
	}
	before := stub.reads
	h.now = h.now.Add(time.Minute)
	h.run(t)
	if stub.reads != before {
		t.Fatal("terminal command was reclaimed")
	}
}

func TestFunctionalInvoiceWorkerConfirmationAndCurrencyMismatch(t *testing.T) {
	h := newHarness(t, "invoice_confirmation", "provider_idempotent")
	h.run(t)
	externalID := h.load(t).ExternalInvoiceID
	o := h.credit("transport-does-not-define-confirmation", 10000)
	o.ExternalInvoiceID = externalID
	h.inbox(t, o)
	h.run(t)
	before := h.load(t).Revision
	o.FinancialKey = "another-transport-key"
	o.Reference = ""
	h.inbox(t, o)
	h.run(t)
	if h.load(t).Revision != before || h.load(t).CreditedMinor != 10000 {
		t.Fatal("confirmation was counted as several credits")
	}
	o.AmountMinor = 11000
	h.inbox(t, o)
	h.run(t)
	if h.load(t).CreditedMinor != 10000 {
		t.Fatal("changed confirmation overwrote accepted amount")
	}
	h2 := newHarness(t, "bank_credits", "recover_only")
	h2.run(t)
	wrong := h2.credit("wrong-currency", 10000)
	wrong.Currency = "USD"
	id := h2.inbox(t, wrong)
	h2.run(t)
	if h2.load(t).CreditedMinor != 0 {
		t.Fatal("foreign-currency amount was credited")
	}
	var state string
	must(t, h2.pool.QueryRow(t.Context(), "SELECT state FROM invoice_inbox WHERE id=$1", id).Scan(&state))
	if state != "review" {
		t.Fatalf("mismatch inbox state=%s", state)
	}
}

func TestFunctionalInvoiceWorkerCancellationDuringProviderCallLeavesRecoverableDispatch(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	stub := h.registry.provider.(*providerStub)
	started := make(chan struct{})
	done := make(chan error, 1)
	stub.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 11*time.Second {
			t.Error("provider timeout is missing")
		}
		close(started)
		<-ctx.Done()
		return d.IssueResult{Outcome: d.OutcomeUnknown}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- h.worker.RunOnce(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider call did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not cancel")
	}
	v := h.load(t)
	if v.DispatchedAt == nil || v.IssuanceState != "dispatched" {
		t.Fatal("cancellation lost durable dispatch")
	}
	h.now = h.now.Add(time.Minute)
	stub.issue = nil
	h.run(t)
	if stub.issues != 1 || stub.recoveries != 1 {
		t.Fatal("cancelled dispatched command did not use Recover")
	}
}

func TestFunctionalInvoiceWorkerStaleProviderReplyCannotOverwriteReclaimedResult(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	stub := h.registry.provider.(*providerStub)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	stub.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return d.IssueResult{Outcome: d.OutcomeUnknown}, ctx.Err()
		}
		result, err := mock.New().Issue(ctx, v)
		result.ExternalInvoiceID = "stale-response"
		return result, err
	}
	go func() { done <- h.worker.RunOnce(t.Context()) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first worker did not call provider")
	}
	h.now = h.now.Add(time.Minute)
	second, err := service.NewWorker(h.tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	must(t, second.RunOnce(t.Context()))
	close(release)
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first worker did not finish")
	}
	v := h.load(t)
	if v.ExternalInvoiceID == "stale-response" || v.IssuanceState != "issued" {
		t.Fatalf("stale reply changed fenced result=%+v", v)
	}
	if stub.issues != 1 || stub.recoveries != 1 {
		t.Fatalf("provider calls issue=%d recover=%d", stub.issues, stub.recoveries)
	}
}

func TestFunctionalInvoiceWorkerIncompleteConfirmationDoesNotConsumeFinancialSlot(t *testing.T) {
	h := newHarness(t, "invoice_confirmation", "provider_idempotent")
	h.run(t)
	invalid := h.credit("not-a-financial-confirmation", 10000)
	id := h.inbox(t, invalid)
	h.run(t)
	var evidenceCount int
	must(t, h.pool.QueryRow(t.Context(), "SELECT count(*) FROM invoice_evidence WHERE payment_id=$1", h.record.PaymentID).Scan(&evidenceCount))
	if evidenceCount != 0 {
		t.Fatal("confirmation without invoice ID consumed unique evidence slot")
	}
	var state string
	must(t, h.pool.QueryRow(t.Context(), "SELECT state FROM invoice_inbox WHERE id=$1", id).Scan(&state))
	if state != "review" {
		t.Fatalf("incomplete confirmation state=%s", state)
	}
	valid := invalid
	valid.ExternalInvoiceID = h.load(t).ExternalInvoiceID
	h.inbox(t, valid)
	h.run(t)
	if h.load(t).CreditedMinor != 10000 {
		t.Fatal("valid confirmation was blocked by preceding incomplete input")
	}
	h.assertStatus(t, "succeeded")
}

func TestFunctionalInvoiceWorkerFinancialRollbackIsAtomicAndRetried(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	before := h.load(t)
	callbacksBefore := h.count(t, "SELECT count(*) FROM outbox_events")
	wrapper := &failOutboxTx{base: h.tx, fail: true}
	var err error
	h.worker, err = service.NewWorker(wrapper, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	id := h.inbox(t, h.credit("atomic-financial-fact", 10000))
	if err := h.worker.RunOnce(t.Context()); err == nil {
		t.Fatal("injected callback transaction failure not observed")
	}
	var evidenceCount int
	must(t, h.pool.QueryRow(t.Context(), "SELECT count(*) FROM invoice_evidence WHERE payment_id=$1", h.record.PaymentID).Scan(&evidenceCount))
	var inboxState string
	must(t, h.pool.QueryRow(t.Context(), "SELECT state FROM invoice_inbox WHERE id=$1", id).Scan(&inboxState))
	if evidenceCount != 0 || h.load(t).CreditedMinor != 0 || inboxState != "received" || h.load(t).Revision != before.Revision || h.count(t, "SELECT count(*) FROM outbox_events") != callbacksBefore {
		t.Fatal("financial rollback left a partial evidence/projection/inbox commit")
	}
	h.assertStatus(t, "pending")
	h.now = h.now.Add(time.Second)
	h.run(t)
	h.assertStatus(t, "succeeded")
	must(t, h.pool.QueryRow(t.Context(), "SELECT count(*) FROM invoice_evidence WHERE payment_id=$1", h.record.PaymentID).Scan(&evidenceCount))
	if evidenceCount != 1 || h.load(t).Revision != before.Revision+1 || h.count(t, "SELECT count(*) FROM outbox_events") != callbacksBefore+1 {
		t.Fatalf("retry evidence count=%d", evidenceCount)
	}
	h.run(t)
	if h.load(t).Revision != before.Revision+1 || h.count(t, "SELECT count(*) FROM invoice_evidence") != 1 {
		t.Fatal("completed apply repeated financial result")
	}
}

func TestFunctionalInvoiceWorkerReceiverPayerAndOverpaymentReview(t *testing.T) {
	for _, field := range []string{"payer", "recipient"} {
		t.Run(field, func(t *testing.T) {
			h := newHarness(t, "bank_credits", "recover_only")
			h.run(t)
			o := h.credit("mismatch", 10000)
			if field == "payer" {
				o.PayerINN = "7700000000"
			} else {
				o.RecipientAccount = "11111111111111111111"
			}
			h.inbox(t, o)
			h.run(t)
			if h.load(t).CreditedMinor != 0 || len(h.load(t).ReviewReasons) == 0 {
				t.Fatal("mismatching credit was applied")
			}
			var eligible bool
			must(t, h.pool.QueryRow(t.Context(), "SELECT eligible FROM invoice_evidence WHERE payment_id=$1", h.record.PaymentID).Scan(&eligible))
			if eligible {
				t.Fatal("mismatch evidence was marked eligible")
			}
		})
	}
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	h.inbox(t, h.credit("overpayment", 12000))
	h.run(t)
	v := h.load(t)
	if v.FundingState != "overpaid" || v.CreditedMinor != 12000 || len(v.ReviewReasons) != 1 || v.ReviewReasons[0] != "overpaid" {
		t.Fatalf("overpayment=%+v", v)
	}
	h.assertStatus(t, "succeeded")
}

func TestFunctionalInvoiceWorkerUsesConfiguredLeaseTimeoutAndReconcileIntervals(t *testing.T) {
	h := newHarness(t, "bank_credits", "provider_idempotent")
	var err error
	h.worker, err = service.NewWorker(h.tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: 2 * time.Second, BatchSize: 1, MaxAttempts: 3, CommandLease: 90 * time.Second, ProviderTimeout: 7 * time.Second, ReconcileInterval: 30 * time.Minute, ExpiredReconcileInterval: 48 * time.Hour}, nil)
	must(t, err)
	stub := h.registry.provider.(*providerStub)
	stub.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 7*time.Second || time.Until(deadline) < 6*time.Second {
			t.Fatal("configured provider timeout not applied")
		}
		var lease time.Time
		must(t, h.pool.QueryRow(t.Context(), `SELECT lease_expires_at FROM invoice_commands WHERE payment_id=$1 AND status='processing'`, v.PaymentID).Scan(&lease))
		if !lease.Equal(h.now.Add(90 * time.Second)) {
			t.Fatalf("lease = %v", lease)
		}
		return mock.New().Issue(ctx, v)
	}
	h.run(t)
	v := h.load(t)
	if v.NextReconcileAt == nil || !v.NextReconcileAt.Equal(h.now.Add(30*time.Minute)) {
		t.Fatalf("reconcile time %v", v.NextReconcileAt)
	}
	h.now = v.Request.AcceptUntil
	h.run(t)
	v = h.load(t)
	if v.NextReconcileAt == nil || !v.NextReconcileAt.Equal(h.now.Add(48*time.Hour)) {
		t.Fatalf("expired reconcile time %v", v.NextReconcileAt)
	}
}

func TestFunctionalInvoiceWorkerDoesNotMixEvidenceModes(t *testing.T) {
	for _, mode := range []string{"bank_credits", "invoice_confirmation"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, mode, "recover_only")
			h.run(t)
			o := h.credit("mixed-evidence", 10000)
			o.ExternalInvoiceID = h.load(t).ExternalInvoiceID
			if mode == "bank_credits" {
				o.EvidenceMode = "invoice_confirmation"
			} else {
				o.EvidenceMode = "bank_credits"
			}
			h.inbox(t, o)
			h.run(t)
			if h.load(t).CreditedMinor != 0 || h.count(t, "SELECT count(*) FROM invoice_evidence") != 0 || h.count(t, "SELECT count(*) FROM invoice_inbox WHERE state='review'") != 1 {
				t.Fatal("incompatible evidence modes mixed or incident lost")
			}
			h.assertStatus(t, "pending")
		})
	}
}

func TestFunctionalInvoiceWorkerStartStopsOnParentCancellation(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	started, canceled := make(chan struct{}), make(chan struct{})
	h.registry.provider.(*providerStub).issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return d.IssueResult{Outcome: d.OutcomeUnknown}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.worker.Start(ctx)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not dispatch")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("parent cancellation did not reach provider")
	}
	stopCtx, stopCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopCancel()
	must(t, h.worker.Stop(stopCtx))
	if h.load(t).DispatchedAt == nil {
		t.Fatal("worker shutdown lost dispatch")
	}
}
