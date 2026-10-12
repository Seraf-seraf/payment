//go:build functional

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	pg "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	outbox "github.com/Seraf-seraf/payment/domain/outbox"
	payment "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/internal/testpostgres"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func TestMain(m *testing.M) { os.Exit(testpostgres.Run(m)) }

type storageHarness struct {
	pool       *pgxpool.Pool
	tx         *pg.TransactionManager
	migrations *goose.Provider
	now        time.Time
	merchant   uuid.UUID
}

func storageDB(t *testing.T) *storageHarness {
	t.Helper()
	pool, migrations := testpostgres.Schema(t, "../../../migrations")
	ctx := t.Context()
	h := &storageHarness{pool: pool, tx: pg.NewTransactionManager(pool, ""), migrations: migrations, now: time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC), merchant: uuid.New()}
	_, err := pool.Exec(ctx, `INSERT INTO merchants(id,name,api_key_hash,shared_secret_encrypted,callback_url,provider_name) VALUES($1,'Storage test',$2,'local-only','https://example.test/callback','mock')`, h.merchant, uuid.NewString())
	storageMust(t, err)
	return h
}
func storageMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func storageSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != want {
		t.Fatalf("expected SQLSTATE %s, got %v", want, err)
	}
}
func (h *storageHarness) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	storageMust(t, h.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{table}.Sanitize()).Scan(&n))
	return n
}
func (h *storageHarness) record(connection, mode string) d.Record {
	req := d.Request{RequestID: uuid.New(), OrderID: "storage-order", AmountMinor: 100, Currency: "RUB", Description: "test", AcceptUntil: h.now.Add(time.Hour), Payer: d.Payer{Kind: "company", Name: "ООО Пример", INN: "7701234567", KPP: "770101001", LegalAddress: "test"}, Lines: []d.Line{{ID: "lot", Name: "lot", Quantity: 1, Unit: "set", UnitPriceMinor: 100, AmountMinor: 100, VAT: d.VAT{Kind: "none"}}}}
	id := uuid.New()
	snapshot, _ := json.Marshal(req)
	return d.Record{PaymentID: id, MerchantID: h.merchant, Request: req, RequestHash: sha256.Sum256(snapshot), Connection: d.ConnectionSnapshot{ID: connection, ProviderKind: "mock_invoice", EvidenceMode: mode}, Reference: "INV-" + strings.ToUpper(strings.ReplaceAll(id.String(), "-", "")), EvidenceMode: mode, IssuanceState: "queued", FundingState: "none", ReviewReasons: []string{}, Revision: 1, CreatedAt: h.now, UpdatedAt: h.now}
}
func (h *storageHarness) parent(v d.Record) payment.Payment {
	return payment.Payment{ID: v.PaymentID, MerchantID: v.MerchantID, ProviderName: "invoice:" + v.Connection.ID, MerchantOrderID: v.Request.OrderID, IdempotencyKey: "invoice:" + v.Request.RequestID.String(), AmountMinor: v.Request.AmountMinor, Currency: v.Request.Currency, Status: payment.StatusCreating, Metadata: map[string]any{}, CreatedAt: h.now, UpdatedAt: h.now}
}
func (h *storageHarness) command(kind string, target uuid.UUID) ports.InvoiceCommand {
	c := ports.InvoiceCommand{ID: uuid.New(), CommandType: kind, Status: "pending", NextAttemptAt: h.now, CreatedAt: h.now, UpdatedAt: h.now}
	if kind == "invoice.apply_notification" {
		c.InboxID = target
	} else {
		c.PaymentID = target
	}
	return c
}
func (h *storageHarness) callback(v d.Record) outbox.Event {
	id := uuid.New()
	envelope := d.CallbackEnvelope{SchemaVersion: "invoice-integration/1", EventID: id, EventType: "payment.invoice.updated", MerchantID: v.MerchantID, PaymentID: v.PaymentID, OrderID: v.Request.OrderID, PaymentMethod: "legal_entity_invoice", Revision: 1, Reason: "created", OccurredAt: h.now, Status: "creating", IssuanceState: "queued", FundingState: "none", AmountMinor: 100, Currency: "RUB", ReviewReasons: []string{}, AcceptUntil: v.Request.AcceptUntil}
	payload, _ := json.Marshal(envelope)
	return outbox.Event{ID: id, AggregateType: "payment", AggregateID: v.PaymentID, EventType: "payment.invoice.updated", Payload: payload, Status: outbox.StatusPending, NextAttemptAt: h.now, CreatedAt: h.now, UpdatedAt: h.now}
}
func (h *storageHarness) seedInvoice(t *testing.T, connection, mode string) d.Record {
	t.Helper()
	v := h.record(connection, mode)
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.Create(ctx, h.parent(v)); err != nil {
			return err
		}
		var err error
		v, err = r.Invoices.Create(ctx, v)
		return err
	}))
	return v
}
func (h *storageHarness) inbox(event string, raw []byte) d.Inbox {
	return d.Inbox{ID: uuid.New(), ConnectionID: "connection-1", EventKey: event, RawBodyHash: sha256.Sum256(raw), Observation: d.Observation{Kind: "status_hint", ExternalInvoiceID: "external-1"}, VerificationKind: "verified_mock", ReceivedAt: h.now}
}
func (h *storageHarness) seedInbox(t *testing.T) d.Inbox {
	t.Helper()
	v := h.inbox(uuid.NewString(), []byte(uuid.NewString()))
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		created, err := r.InvoiceInbox.Create(ctx, v)
		if err == nil && !created {
			return errors.New("new inbox was not inserted")
		}
		return err
	}))
	return v
}

func TestFunctionalInvoiceBundleAtomicity(t *testing.T) {
	h := storageDB(t)
	abort := errors.New("abort transaction")
	for stop := 1; stop <= 4; stop++ {
		v := h.record("connection-1", "bank_credits")
		err := h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
			if err := r.Payments.Create(ctx, h.parent(v)); err != nil {
				return err
			}
			if stop == 1 {
				return abort
			}
			if _, err := r.Invoices.Create(ctx, v); err != nil {
				return err
			}
			if stop == 2 {
				return abort
			}
			if err := r.InvoiceCommands.Create(ctx, h.command("invoice.issue", v.PaymentID)); err != nil {
				return err
			}
			if stop == 3 {
				return abort
			}
			if err := r.Outbox.Create(ctx, h.callback(v)); err != nil {
				return err
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		for _, table := range []string{"payments", "invoice_payments", "invoice_commands", "outbox_events"} {
			if n := h.count(t, table); n != 0 {
				t.Fatalf("rollback after operation %d left %d rows in %s", stop, n, table)
			}
		}
	}
	v := h.record("connection-1", "bank_credits")
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		if err := r.Payments.Create(ctx, h.parent(v)); err != nil {
			return err
		}
		created, err := r.Invoices.Create(ctx, v)
		if err != nil {
			return err
		}
		if created.InvoiceNumber <= 0 {
			return errors.New("missing assigned invoice number")
		}
		if err := r.InvoiceCommands.Create(ctx, h.command("invoice.issue", v.PaymentID)); err != nil {
			return err
		}
		return r.Outbox.Create(ctx, h.callback(v))
	}))
	for _, table := range []string{"payments", "invoice_payments", "invoice_commands", "outbox_events"} {
		if n := h.count(t, table); n != 1 {
			t.Fatalf("commit missing rows in %s: %d", table, n)
		}
	}
}

func TestFunctionalInboxApplyAtomicityAndDedup(t *testing.T) {
	h := storageDB(t)
	v := h.inbox("event-1", []byte("verified body"))
	abort := errors.New("abort transaction")
	for stop := 1; stop <= 2; stop++ {
		err := h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
			_, err := r.InvoiceInbox.Create(ctx, v)
			if err != nil {
				return err
			}
			if stop == 1 {
				return abort
			}
			if err := r.InvoiceCommands.Create(ctx, h.command("invoice.apply_notification", v.ID)); err != nil {
				return err
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if h.count(t, "invoice_inbox") != 0 || h.count(t, "invoice_commands") != 0 {
			t.Fatal("inbox/apply rollback was partial")
		}
	}
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		_, err := r.InvoiceInbox.Create(ctx, v)
		if err != nil {
			return err
		}
		return r.InvoiceCommands.Create(ctx, h.command("invoice.apply_notification", v.ID))
	}))
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		duplicate := v
		duplicate.ID = uuid.New()
		created, err := r.InvoiceInbox.Create(ctx, duplicate)
		if err != nil {
			return err
		}
		if created {
			return errors.New("exact transport duplicate inserted")
		}
		changed := v
		changed.ID = uuid.New()
		changed.RawBodyHash = sha256.Sum256([]byte("different body"))
		changed.Observation.AmountMinor = 101
		created, err = r.InvoiceInbox.Create(ctx, changed)
		if err == nil && !created {
			return errors.New("changed body under same event key discarded")
		}
		return err
	}))
	if h.count(t, "invoice_inbox") != 2 || h.count(t, "invoice_commands") != 1 {
		t.Fatal("unexpected inbox/apply rows")
	}
}

func TestFunctionalInvoiceIdentityAndFinancialKeys(t *testing.T) {
	h := storageDB(t)
	a := h.seedInvoice(t, "connection-1", "bank_credits")
	b := h.seedInvoice(t, "connection-1", "bank_credits")
	c := h.seedInvoice(t, "connection-2", "bank_credits")
	if a.InvoiceNumber == b.InvoiceNumber || a.InvoiceNumber <= 0 {
		t.Fatal("invoice number not unique/positive")
	}
	_, err := h.pool.Exec(t.Context(), `UPDATE invoice_payments SET request_snapshot='{}' WHERE payment_id=$1`, a.PaymentID)
	storageSQLState(t, err, "55000")
	_, err = h.pool.Exec(t.Context(), `UPDATE invoice_payments SET external_invoice_id='external-1' WHERE payment_id=$1`, a.PaymentID)
	storageMust(t, err)
	_, err = h.pool.Exec(t.Context(), `UPDATE invoice_payments SET external_invoice_id='external-1' WHERE payment_id=$1`, b.PaymentID)
	storageSQLState(t, err, "23505")
	_, err = h.pool.Exec(t.Context(), `UPDATE invoice_payments SET external_invoice_id='external-1' WHERE payment_id=$1`, c.PaymentID)
	storageMust(t, err)
	duplicate := h.record("connection-1", "bank_credits")
	duplicate.Request.RequestID = a.Request.RequestID
	err = h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		return r.Payments.Create(ctx, h.parent(duplicate))
	})
	if !errors.Is(err, paymentservice.ErrAlreadyExists) {
		t.Fatalf("merchant/request duplicate must conflict in parent key namespace: %v", err)
	}

	evidence := d.Evidence{ID: uuid.New(), PaymentID: a.PaymentID, ConnectionID: "connection-1", FinancialKey: "operation-1", NormalizedHash: sha256.Sum256([]byte("fact")), EvidenceMode: "bank_credits", AmountMinor: 40, Currency: "RUB", Eligible: true, RecordedAt: h.now, VerificationKind: "verified_mock"}
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		inserted, conflict, err := r.Invoices.RecordEvidence(ctx, evidence)
		if err != nil {
			return err
		}
		if !inserted || conflict {
			return errors.New("first evidence not inserted")
		}
		duplicate := evidence
		duplicate.ID = uuid.New()
		inserted, conflict, err = r.Invoices.RecordEvidence(ctx, duplicate)
		if err != nil {
			return err
		}
		if inserted || conflict {
			return errors.New("financial duplicate not recognized")
		}
		changed := duplicate
		changed.NormalizedHash = sha256.Sum256([]byte("changed"))
		changed.AmountMinor = 60
		inserted, conflict, err = r.Invoices.RecordEvidence(ctx, changed)
		if err != nil {
			return err
		}
		if inserted || !conflict {
			return errors.New("changed financial key not detected")
		}
		other := evidence
		other.ID = uuid.New()
		other.PaymentID = b.PaymentID
		inserted, conflict, err = r.Invoices.RecordEvidence(ctx, other)
		if err != nil {
			return err
		}
		if inserted || !conflict {
			return errors.New("operation allocated to another invoice")
		}
		separate := evidence
		separate.ID = uuid.New()
		separate.PaymentID = c.PaymentID
		separate.ConnectionID = "connection-2"
		inserted, conflict, err = r.Invoices.RecordEvidence(ctx, separate)
		if err == nil && (!inserted || conflict) {
			return errors.New("financial key not account scoped")
		}
		return err
	}))
	if h.count(t, "invoice_evidence") != 2 {
		t.Fatal("duplicate/changed evidence persisted")
	}
	confirmation := h.seedInvoice(t, "connection-3", "invoice_confirmation")
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		e := evidence
		e.ID = uuid.New()
		e.PaymentID = confirmation.PaymentID
		e.ConnectionID = "connection-3"
		e.EvidenceMode = "invoice_confirmation"
		if _, _, err := r.Invoices.RecordEvidence(ctx, e); err != nil {
			return err
		}
		e.ID = uuid.New()
		e.FinancialKey = "another-final-key"
		e.NormalizedHash = sha256.Sum256([]byte("different"))
		inserted, conflict, err := r.Invoices.RecordEvidence(ctx, e)
		if err == nil && (inserted || !conflict) {
			return errors.New("second invoice confirmation accepted")
		}
		return err
	}))
	_, err = h.pool.Exec(t.Context(), `UPDATE invoice_evidence SET amount_minor=1 WHERE id=$1`, evidence.ID)
	storageSQLState(t, err, "55000")
	_, err = h.pool.Exec(t.Context(), `DELETE FROM payments WHERE id=$1`, a.PaymentID)
	storageSQLState(t, err, "23503")
}

func TestFunctionalCommandUniquenessAndOutboxSeparation(t *testing.T) {
	h := storageDB(t)
	v := h.seedInvoice(t, "connection-1", "bank_credits")
	inbox := h.seedInbox(t)
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		for _, kind := range []string{"invoice.issue", "invoice.reconcile", "invoice.apply_notification"} {
			target := v.PaymentID
			if kind == "invoice.apply_notification" {
				target = inbox.ID
			}
			for i := 0; i < 2; i++ {
				if err := r.InvoiceCommands.Create(ctx, h.command(kind, target)); err != nil {
					return err
				}
			}
		}
		return r.Outbox.Create(ctx, h.callback(v))
	}))
	if h.count(t, "invoice_commands") != 3 || h.count(t, "outbox_events") != 1 {
		t.Fatal("duplicate commands or mixed queues")
	}
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		events, err := r.Outbox.FetchPending(ctx, 20, h.now)
		if err != nil {
			return err
		}
		if len(events) != 1 || events[0].AggregateType != "payment" || events[0].AggregateID != v.PaymentID || events[0].EventType != "payment.invoice.updated" {
			return errors.New("callback repository saw commands")
		}
		var envelope d.CallbackEnvelope
		if err := json.Unmarshal(events[0].Payload, &envelope); err != nil {
			return err
		}
		if envelope.PaymentID != v.PaymentID || envelope.EventID != events[0].ID {
			return errors.New("callback snapshot was not preserved")
		}
		commands, err := r.InvoiceCommands.Claim(ctx, h.now, 20, time.Minute)
		if err != nil {
			return err
		}
		for _, c := range commands {
			if err := r.InvoiceCommands.Finish(ctx, c, "completed", h.now, "", h.now); err != nil {
				return err
			}
		}
		return nil
	}))
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		for _, kind := range []string{"invoice.issue", "invoice.reconcile", "invoice.apply_notification"} {
			target := v.PaymentID
			if kind == "invoice.apply_notification" {
				target = inbox.ID
			}
			if err := r.InvoiceCommands.Create(ctx, h.command(kind, target)); err != nil {
				return err
			}
		}
		return nil
	}))
	if h.count(t, "invoice_commands") != 5 {
		t.Fatal("issue uniqueness must span terminal statuses; active apply/reconcile may be replaced")
	}
}

func TestFunctionalConcurrentClaimAndLeaseFencing(t *testing.T) {
	h := storageDB(t)
	for i := 0; i < 8; i++ {
		v := h.seedInbox(t)
		storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
			return r.InvoiceCommands.Create(ctx, h.command("invoice.apply_notification", v.ID))
		}))
	}
	type answer struct {
		commands []ports.InvoiceCommand
		err      error
	}
	results := make(chan answer, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var commands []ports.InvoiceCommand
			err := h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
				var err error
				commands, err = r.InvoiceCommands.Claim(ctx, h.now, 4, time.Minute)
				return err
			})
			results <- answer{commands, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	seen := map[uuid.UUID]bool{}
	var stale ports.InvoiceCommand
	for result := range results {
		storageMust(t, result.err)
		if len(result.commands) != 4 {
			t.Fatalf("claim size %d", len(result.commands))
		}
		for _, c := range result.commands {
			if seen[c.ID] {
				t.Fatal("two workers claimed same command")
			}
			seen[c.ID] = true
			if c.Attempts != 1 || c.LeaseToken == uuid.Nil || !c.LeaseExpiresAt.Equal(h.now.Add(time.Minute)) {
				t.Fatal("invalid claim lease")
			}
			stale = c
		}
	}
	now := h.now.Add(time.Minute)
	var reclaimed []ports.InvoiceCommand
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		var err error
		reclaimed, err = r.InvoiceCommands.Claim(ctx, now, 8, time.Minute)
		return err
	}))
	if len(reclaimed) != 8 {
		t.Fatal("expired leases not reclaimed")
	}
	var fresh ports.InvoiceCommand
	for _, c := range reclaimed {
		if c.Attempts != 2 || c.LeaseToken == stale.LeaseToken {
			t.Fatal("reclaim did not fence old worker")
		}
		if c.ID == stale.ID {
			fresh = c
		}
	}
	for _, action := range []string{"fence", "finish"} {
		err := h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
			if action == "fence" {
				return r.InvoiceCommands.Fence(ctx, stale)
			}
			return r.InvoiceCommands.Finish(ctx, stale, "completed", now, "", now)
		})
		if !errors.Is(err, ports.ErrLeaseLost) {
			t.Fatalf("stale %s accepted: %v", action, err)
		}
	}
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		return r.InvoiceCommands.Finish(ctx, fresh, "failed", now, "storage_unavailable", now)
	}))
	later := now.Add(time.Hour)
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		commands, err := r.InvoiceCommands.Claim(ctx, later, 20, time.Minute)
		if err != nil {
			return err
		}
		for _, c := range commands {
			if c.ID == fresh.ID {
				return errors.New("terminal failed command reclaimed")
			}
		}
		return nil
	}))
}

func TestFunctionalClaimPreservesCommandAudit(t *testing.T) {
	h := storageDB(t)
	inbox := h.seedInbox(t)
	command := h.command("invoice.apply_notification", inbox.ID)
	command.CreatedAt = h.now.Add(-time.Hour)
	command.UpdatedAt = command.CreatedAt
	command.NextAttemptAt = h.now.Add(-time.Minute)
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error { return r.InvoiceCommands.Create(ctx, command) }))
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		claimed, err := r.InvoiceCommands.Claim(ctx, h.now, 1, time.Minute)
		if err != nil {
			return err
		}
		if len(claimed) != 1 {
			return errors.New("due command not claimed")
		}
		got := claimed[0]
		if !got.CreatedAt.Equal(command.CreatedAt) || !got.UpdatedAt.Equal(h.now) {
			t.Errorf("claim lost persisted audit: created_at=%v updated_at=%v; want %v %v", got.CreatedAt, got.UpdatedAt, command.CreatedAt, h.now)
		}
		return nil
	}))
}

func TestFunctionalDueUnmatchedBoundedAndSkipLocked(t *testing.T) {
	h := storageDB(t)
	var due []d.Inbox
	for i := 0; i < 3; i++ {
		v := h.seedInbox(t)
		due = append(due, v)
		at := h.now.Add(time.Duration(i-1) * time.Second)
		storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
			return r.InvoiceInbox.SetState(ctx, v.ID, "unmatched", "", &at, h.now)
		}))
	}
	processed := h.seedInbox(t)
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		return r.InvoiceInbox.SetState(ctx, processed.ID, "processed", "", nil, h.now)
	}))
	// Hold the earliest due row while another transaction queries the bounded sweep.
	storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		locked, err := r.InvoiceInbox.DueUnmatched(ctx, h.now, 1)
		if err != nil {
			return err
		}
		if len(locked) != 1 || locked[0].ID != due[0].ID {
			return errors.New("due ordering/limit incorrect")
		}
		return h.tx.WithinTx(ctx, func(ctx context.Context, other ports.Repositories) error {
			remaining, err := other.InvoiceInbox.DueUnmatched(ctx, h.now, 20)
			if err != nil {
				return err
			}
			if len(remaining) != 1 || remaining[0].ID != due[1].ID {
				return errors.New("sweep included locked/future/processed inbox")
			}
			if remaining[0].Observation.ExternalInvoiceID != "external-1" || remaining[0].NextMatchAt == nil {
				return errors.New("sweep lost normalized payload or schedule")
			}
			return nil
		})
	}))
}

func TestFunctionalSafeMigrationDownUp(t *testing.T) {
	t.Run("empty_roundtrip", func(t *testing.T) {
		h := storageDB(t)
		before := h.outboxDefinition(t)
		_, err := h.migrations.Down(t.Context())
		storageMust(t, err)
		_, err = h.migrations.Down(t.Context())
		storageMust(t, err)
		if h.outboxDefinition(t) != before {
			t.Fatal("invoice Down changed outbox schema")
		}
		_, err = h.migrations.Up(t.Context())
		storageMust(t, err)
		if h.outboxDefinition(t) != before {
			t.Fatal("invoice Up changed outbox schema")
		}
		if h.count(t, "outbox_events") != 0 || h.count(t, "invoice_inbox") != 0 || h.count(t, "invoice_payments") != 0 {
			t.Fatal("empty migration roundtrip changed data")
		}
	})
	t.Run("inbox_guard", func(t *testing.T) {
		h := storageDB(t)
		h.seedInbox(t)
		_, err := h.migrations.Down(t.Context())
		if err == nil {
			t.Fatal("Down dropped populated inbox")
		}
		if h.count(t, "invoice_inbox") != 1 {
			t.Fatal("failed Down lost inbox")
		}
	})
	t.Run("command_guard", func(t *testing.T) {
		h := storageDB(t)
		v := h.seedInvoice(t, "connection-1", "bank_credits")
		storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
			return r.InvoiceCommands.Create(ctx, h.command("invoice.issue", v.PaymentID))
		}))
		_, err := h.migrations.Down(t.Context())
		if err == nil {
			t.Fatal("Down dropped command")
		}
		if h.count(t, "invoice_commands") != 1 {
			t.Fatal("failed Down lost command")
		}
	})
	t.Run("invoice_guard", func(t *testing.T) {
		h := storageDB(t)
		h.seedInvoice(t, "connection-1", "bank_credits")
		_, err := h.migrations.Down(t.Context())
		storageMust(t, err)
		_, err = h.migrations.Down(t.Context())
		if err == nil {
			t.Fatal("Down dropped invoice")
		}
		if h.count(t, "invoice_payments") != 1 {
			t.Fatal("failed Down lost invoice")
		}
	})
	t.Run("orphan_parent_guard", func(t *testing.T) {
		h := storageDB(t)
		v := h.record("connection-1", "bank_credits")
		storageMust(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error { return r.Payments.Create(ctx, h.parent(v)) }))
		_, err := h.migrations.Down(t.Context())
		storageMust(t, err)
		_, err = h.migrations.Down(t.Context())
		if err == nil {
			t.Fatal("Down ignored invoice parent")
		}
		if h.count(t, "payments") != 1 {
			t.Fatal("failed Down lost parent")
		}
	})
}

func (h *storageHarness) outboxDefinition(t *testing.T) string {
	t.Helper()
	var definition string
	storageMust(t, h.pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'columns',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.ordinal_position) FROM information_schema.columns c WHERE c.table_schema=current_schema() AND c.table_name='outbox_events'),
 'indexes',(SELECT jsonb_agg(indexdef ORDER BY indexname) FROM pg_indexes WHERE schemaname=current_schema() AND tablename='outbox_events'),
 'constraints',(SELECT jsonb_agg(pg_get_constraintdef(oid) ORDER BY conname) FROM pg_constraint WHERE conrelid='outbox_events'::regclass)
 )::text`).Scan(&definition))
	return definition
}
