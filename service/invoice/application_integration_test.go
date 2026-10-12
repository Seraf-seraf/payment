//go:build functional

package invoice_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	mock "github.com/Seraf-seraf/payment/adapter/provider/mockinvoice"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/ports"
	service "github.com/Seraf-seraf/payment/service/invoice"
	"github.com/google/uuid"
)

func TestFunctionalApplicationDefinitiveRejectionResolvesIssuanceUnknown(t *testing.T) {
	h := newHarness(t, "bank_credits", "provider_idempotent")
	provider := h.registry.provider.(*providerStub)
	var first d.Record
	provider.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		if provider.issues == 1 {
			first = v
			return d.IssueResult{Outcome: d.OutcomeUnknown}, context.DeadlineExceeded
		}
		if v.PaymentID != first.PaymentID || v.Reference != first.Reference || v.RequestHash != first.RequestHash || v.InvoiceNumber != first.InvoiceNumber {
			t.Fatal("safe replay changed invoice identity")
		}
		return d.IssueResult{Outcome: d.DefinitelyRejected, ErrorCode: "invalid_terms"}, nil
	}
	h.run(t)
	if v := h.load(t); v.IssuanceState != "unknown" || !slices.Contains(v.ReviewReasons, "issuance_unknown") {
		t.Fatalf("first uncertain result: %+v", v)
	}
	h.now = h.now.Add(time.Second)
	h.run(t)
	v := h.load(t)
	if v.IssuanceState != "rejected" || v.ExternalInvoiceID != "" || v.CreditedMinor != 0 {
		t.Fatalf("definitive rejection lost: %+v", v)
	}
	h.assertStatus(t, "failed")
	if slices.Contains(v.ReviewReasons, "issuance_unknown") {
		t.Error("proven definitive rejection left issuance_unknown review")
	}
	if provider.issues != 2 || provider.recoveries != 0 {
		t.Fatalf("provider calls: Issue=%d Recover=%d", provider.issues, provider.recoveries)
	}
	var status string
	var attempts int
	must(t, h.pool.QueryRow(t.Context(), `SELECT status,attempts FROM invoice_commands WHERE command_type='invoice.issue' AND payment_id=$1`, v.PaymentID).Scan(&status, &attempts))
	if status != "completed" || attempts != 2 {
		t.Fatalf("issue command: status=%s attempts=%d", status, attempts)
	}
	rows, err := h.pool.Query(t.Context(), `SELECT payload FROM outbox_events WHERE aggregate_id=$1 ORDER BY created_at,id`, v.PaymentID)
	must(t, err)
	defer rows.Close()
	var last d.CallbackEnvelope
	for rows.Next() {
		var payload []byte
		must(t, rows.Scan(&payload))
		var event d.CallbackEnvelope
		must(t, json.Unmarshal(payload, &event))
		if event.Revision > last.Revision {
			last = event
		}
	}
	must(t, rows.Err())
	if last.Revision != v.Revision || last.Status != "failed" || last.IssuanceState != "rejected" || last.Reason != "issue_failed" || last.RequiresReview {
		t.Errorf("definitive rejection callback retained uncertainty: %+v", last)
	}
}

// The production transaction commits dispatch, then the process loses execution.
type crashAfterDispatchTx struct {
	base    ports.TransactionManager
	cancel  context.CancelFunc
	crashed bool
}

func (tx *crashAfterDispatchTx) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	dispatched := false
	err := tx.base.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		r.Invoices = &observeDispatchRepository{InvoiceRepository: r.Invoices, dispatched: &dispatched}
		return fn(ctx, r)
	})
	if err == nil && dispatched && !tx.crashed {
		tx.crashed = true
		tx.cancel()
		return context.Canceled
	}
	return err
}

type observeDispatchRepository struct {
	ports.InvoiceRepository
	dispatched *bool
}

func (r *observeDispatchRepository) Save(ctx context.Context, v d.Record) error {
	err := r.InvoiceRepository.Save(ctx, v)
	if err == nil && v.IssuanceState == "dispatched" && v.DispatchedAt != nil {
		*r.dispatched = true
	}
	return err
}
func TestFunctionalApplicationCrashAfterDispatchUsesRecoverWithoutSecondPOST(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	provider := h.registry.provider.(*providerStub)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tx := &crashAfterDispatchTx{base: h.tx, cancel: cancel}
	crashedWorker, err := service.NewWorker(tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	if err := crashedWorker.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("crash boundary: %v", err)
	}
	v := h.load(t)
	if v.DispatchedAt == nil || v.IssuanceState != "dispatched" || provider.issues != 0 {
		t.Fatalf("crash did not occur after durable dispatch and before POST: %+v Issue=%d", v, provider.issues)
	}
	provider.recover = func(ctx context.Context, v d.Record) (d.Recovery, error) { return d.Recovery{Found: false}, nil }
	h.now = h.now.Add(time.Minute)
	h.run(t)
	v = h.load(t)
	if provider.issues != 0 || provider.recoveries != 1 || v.IssuanceState != "unknown" || !slices.Contains(v.ReviewReasons, "issuance_unknown") {
		t.Fatalf("crash recovery blindly issued: Issue=%d Recover=%d invoice=%+v", provider.issues, provider.recoveries, v)
	}
	h.assertStatus(t, "creating")
	if v.NextReconcileAt == nil {
		t.Fatal("unknown recovery lost reconcile schedule")
	}
}

func TestFunctionalApplicationEarlyWebhookBeforeIssueReplyKeepsFinancialSuccess(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	provider := h.registry.provider.(*providerStub)
	provider.issue = func(ctx context.Context, v d.Record) (d.IssueResult, error) {
		// A second worker applies a webhook while the first waits on the provider.
		h.inbox(t, h.credit("early-during-issue", 10000))
		second, err := service.NewWorker(h.tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
		must(t, err)
		must(t, second.RunOnce(ctx))
		h.assertStatus(t, "succeeded")
		return mock.New().Issue(ctx, v)
	}
	h.run(t)
	v := h.load(t)
	if v.IssuanceState != "issued" || v.CreditedMinor != 10000 || v.FundingState != "paid" || v.Artifact == nil || v.Artifact.NextAction == nil || v.Artifact.NextAction.Type != "bank_transfer" || v.Artifact.NextAction.URL != "" {
		t.Fatalf("Issue reply overwrote early money or required a URL: %+v", v)
	}
	h.assertStatus(t, "succeeded")
	if h.count(t, "SELECT count(*) FROM invoice_evidence") != 1 {
		t.Fatal("early financial fact duplicated")
	}
	var url *string
	must(t, h.pool.QueryRow(t.Context(), `SELECT payment_url FROM payments WHERE id=$1`, v.PaymentID).Scan(&url))
	if url != nil {
		t.Fatal("invoice populated acquiring payment_url")
	}
}

// Run an actual binding transaction after an actual Match saw no binding,
// before apply commits its unmatched state. No repository result is fabricated.
type afterMissingMatchTx struct {
	base ports.TransactionManager
	hook func()
	done bool
}

func (tx *afterMissingMatchTx) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return tx.base.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		r.Invoices = &afterMissingMatchRepository{InvoiceRepository: r.Invoices, owner: tx}
		return fn(ctx, r)
	})
}

type afterMissingMatchRepository struct {
	ports.InvoiceRepository
	owner *afterMissingMatchTx
}

func (r *afterMissingMatchRepository) Match(ctx context.Context, connection, externalID, reference string) ([]uuid.UUID, error) {
	ids, err := r.InvoiceRepository.Match(ctx, connection, externalID, reference)
	if err == nil && len(ids) == 0 && !r.owner.done {
		r.owner.done = true
		r.owner.hook()
	}
	return ids, err
}
func TestFunctionalApplicationBindingRacesUnmatchedCommitWithoutLostWakeup(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	observation := h.credit("binding-race-credit", 10000)
	observation.Reference = ""
	observation.ExternalInvoiceID = "mock_invoice_" + h.record.PaymentID.String()
	inboxID := h.inbox(t, observation)
	_, err := h.pool.Exec(t.Context(), `UPDATE invoice_commands SET next_attempt_at=$1 WHERE command_type='invoice.issue'`, h.now.Add(time.Hour))
	must(t, err)
	tx := &afterMissingMatchTx{base: h.tx}
	tx.hook = func() {
		_, err := h.pool.Exec(t.Context(), `UPDATE invoice_commands SET next_attempt_at=$1 WHERE command_type='invoice.issue'`, h.now)
		must(t, err)
		bindingWorker, err := service.NewWorker(h.tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
		must(t, err)
		must(t, bindingWorker.RunOnce(t.Context()))
		if h.load(t).ExternalInvoiceID != observation.ExternalInvoiceID {
			t.Fatal("race did not commit binding")
		}
	}
	applyWorker, err := service.NewWorker(tx, h.registry, func() time.Time { return h.now }, service.WorkerConfig{PollInterval: time.Second, BatchSize: 20, MaxAttempts: 3}, nil)
	must(t, err)
	must(t, applyWorker.RunOnce(t.Context()))
	var state string
	must(t, h.pool.QueryRow(t.Context(), `SELECT state FROM invoice_inbox WHERE id=$1`, inboxID).Scan(&state))
	if !tx.done || state != "unmatched" {
		t.Fatalf("required race did not occur: hook=%v state=%s", tx.done, state)
	}
	h.now = h.now.Add(time.Minute)
	h.run(t)
	must(t, h.pool.QueryRow(t.Context(), `SELECT state FROM invoice_inbox WHERE id=$1`, inboxID).Scan(&state))
	if state != "processed" || h.load(t).CreditedMinor != 10000 {
		t.Fatal("bounded recovery sweep lost concurrent unmatched")
	}
	h.assertStatus(t, "succeeded")
	var attempts int
	must(t, h.pool.QueryRow(t.Context(), `SELECT count(*) FROM invoice_commands WHERE inbox_id=$1`, inboxID).Scan(&attempts))
	if attempts != 2 {
		t.Fatalf("unmatched should complete and requeue, commands=%d", attempts)
	}
}
