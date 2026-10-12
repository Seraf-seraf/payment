package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	outbox "github.com/Seraf-seraf/payment/domain/outbox"
	payment "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
	"log/slog"
	"net/url"
	"slices"
	"sync"
	"time"
)

const scheduleInterval = 60 * time.Second
const recoveryBatch = 20

type WorkerConfig struct {
	PollInterval             time.Duration
	BatchSize                int
	MaxAttempts              int
	CommandLease             time.Duration
	ProviderTimeout          time.Duration
	ReconcileInterval        time.Duration
	ExpiredReconcileInterval time.Duration
}
type Worker struct {
	tx           ports.TransactionManager
	registry     ports.InvoiceRuntimeRegistry
	now          func() time.Time
	cfg          WorkerConfig
	log          *slog.Logger
	cancel       context.CancelFunc
	done         chan struct{}
	mu           sync.Mutex
	nextSchedule time.Time
}

func NewWorker(tx ports.TransactionManager, registry ports.InvoiceRuntimeRegistry, now func() time.Time, cfg WorkerConfig, log *slog.Logger) (*Worker, error) {
	if cfg.CommandLease == 0 {
		cfg.CommandLease = 60 * time.Second
	}
	if cfg.ProviderTimeout == 0 {
		cfg.ProviderTimeout = 10 * time.Second
	}
	if cfg.ReconcileInterval == 0 {
		cfg.ReconcileInterval = 10 * time.Minute
	}
	if cfg.ExpiredReconcileInterval == 0 {
		cfg.ExpiredReconcileInterval = 24 * time.Hour
	}
	if cfg.ProviderTimeout <= 0 || cfg.CommandLease <= cfg.ProviderTimeout || cfg.ReconcileInterval <= 0 || cfg.ExpiredReconcileInterval <= 0 {
		return nil, errors.New("invalid invoice worker timing")
	}
	if tx == nil || registry == nil || now == nil || cfg.BatchSize < 1 || cfg.BatchSize > 100 || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 100 || cfg.PollInterval <= 0 {
		return nil, errors.New("invalid invoice worker configuration")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{tx: tx, registry: registry, now: now, cfg: cfg, log: log}, nil
}
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(w.cfg.PollInterval)
		defer ticker.Stop()
		for {
			if err := w.RunOnce(runCtx); err != nil && !errors.Is(err, context.Canceled) {
				w.log.Error("invoice worker cycle failed", slog.String("error_code", "invoice_cycle_failed"))
			}
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	done, cancel := w.done, w.cancel
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *Worker) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := w.now().UTC()
	if !now.Before(w.nextSchedule) {
		if err := w.schedule(ctx, now); err != nil {
			return err
		}
		w.nextSchedule = now.Add(scheduleInterval)
	}
	var commands []ports.InvoiceCommand
	if err := w.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		var err error
		commands, err = r.InvoiceCommands.Claim(ctx, now, w.cfg.BatchSize, w.cfg.CommandLease)
		return err
	}); err != nil {
		return err
	}
	var cycleErr error
	for _, c := range commands {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		switch c.CommandType {
		case "invoice.issue":
			err = w.issue(ctx, c, false)
		case "invoice.reconcile":
			err = w.issue(ctx, c, true)
		case "invoice.apply_notification":
			err = w.applyNotification(ctx, c)
		default:
			err = errors.New("unsupported invoice command")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, ports.ErrLeaseLost) {
			// A failed result commit leaves durable dispatch intact; the next claim must Recover.
			retryErr := w.retry(ctx, c, "invoice_infrastructure_failed")
			cycleErr = errors.Join(cycleErr, err, retryErr)
		}
	}
	return cycleErr
}
func backoff(attempt int) time.Duration {
	if attempt >= 8 {
		return 60 * time.Second
	}
	return time.Duration(attempt*attempt) * time.Second
}
func (w *Worker) reconcileAt(now, deadline time.Time) time.Time {
	if now.Before(deadline) {
		return now.Add(w.cfg.ReconcileInterval)
	}
	return now.Add(w.cfg.ExpiredReconcileInterval)
}
func (w *Worker) retry(ctx context.Context, c ports.InvoiceCommand, code string) error {
	now := w.now().UTC()
	status := "pending"
	if c.Attempts >= w.cfg.MaxAttempts {
		status = "failed"
	}
	return w.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.InvoiceCommands.Fence(ctx, c); err != nil {
			return err
		}
		if c.CommandType == "invoice.issue" || c.CommandType == "invoice.reconcile" {
			v, err := r.Invoices.Lock(ctx, c.PaymentID)
			if err != nil {
				return err
			}
			if v.IssuanceState == "dispatched" {
				p, err := r.Invoices.LockPayment(ctx, v.PaymentID)
				if err != nil {
					return err
				}
				v.IssuanceState = "unknown"
				addReview(&v, "issuance_unknown")
				next := w.reconcileAt(now, v.Request.AcceptUntil)
				v.NextReconcileAt = &next
				if err := persistProjection(ctx, r, &v, &p, "review_required", now); err != nil {
					return err
				}
			}
		}
		return r.InvoiceCommands.Finish(ctx, c, status, now.Add(backoff(c.Attempts)), code, now)
	})
}
func (w *Worker) schedule(ctx context.Context, now time.Time) error {
	return w.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		ids, err := r.Invoices.Due(ctx, now, recoveryBatch)
		if err != nil {
			return err
		}
		// The due rows are locked in this short transaction; no provider is called here.
		for _, id := range ids {
			v, err := r.Invoices.Lock(ctx, id)
			if err != nil {
				return err
			}
			if stopReconcile(v) {
				v.NextReconcileAt = nil
			} else {
				if err := r.InvoiceCommands.Create(ctx, newCommand("invoice.reconcile", id, uuid.Nil, now)); err != nil {
					return err
				}
				next := w.reconcileAt(now, v.Request.AcceptUntil)
				v.NextReconcileAt = &next
			}
			v.UpdatedAt = now
			if err := r.Invoices.Save(ctx, v); err != nil {
				return err
			}
		}
		inboxes, err := r.InvoiceInbox.DueUnmatched(ctx, now, recoveryBatch)
		if err != nil {
			return err
		}
		for _, inbox := range inboxes {
			if err := w.wake(ctx, r, inbox, now); err != nil {
				return err
			}
		}
		return nil
	})
}
func stopReconcile(v d.Record) bool {
	return v.FundingState == "paid" && len(v.ReviewReasons) == 0 || v.IssuanceState == "rejected" && v.CreditedMinor == 0 || v.IssuanceState == "expired_before_issue" && v.DispatchedAt == nil
}
func newCommand(kind string, paymentID, inboxID uuid.UUID, now time.Time) ports.InvoiceCommand {
	return ports.InvoiceCommand{ID: uuid.New(), CommandType: kind, PaymentID: paymentID, InboxID: inboxID, Status: "pending", NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
}
func (w *Worker) wake(ctx context.Context, r ports.Repositories, inbox d.Inbox, now time.Time) error {
	if len(inbox.Observation.References) > 1 {
		return r.InvoiceInbox.SetState(ctx, inbox.ID, "review", "ambiguous_reference", nil, now)
	}
	ids, err := r.Invoices.Match(ctx, inbox.ConnectionID, inbox.Observation.ExternalInvoiceID, inbox.Observation.Reference)
	if err != nil {
		return err
	}
	if len(ids) > 1 {
		return r.InvoiceInbox.SetState(ctx, inbox.ID, "review", "ambiguous_reference", nil, now)
	}
	next := now.Add(scheduleInterval)
	if len(ids) == 1 {
		if err := r.InvoiceCommands.Create(ctx, newCommand("invoice.apply_notification", uuid.Nil, inbox.ID, now)); err != nil {
			return err
		}
	}
	return r.InvoiceInbox.SetState(ctx, inbox.ID, "unmatched", "", &next, now)
}
func (w *Worker) issue(ctx context.Context, c ports.InvoiceCommand, reconcile bool) error {
	var v d.Record
	var finished bool
	var firstDispatch bool
	err := w.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.InvoiceCommands.Fence(ctx, c); err != nil {
			return err
		}
		var err error
		v, err = r.Invoices.Lock(ctx, c.PaymentID)
		if err != nil {
			return err
		}
		now := w.now().UTC()
		if (reconcile && stopReconcile(v)) || !reconcile && (v.IssuanceState == "issued" || v.IssuanceState == "rejected" || v.IssuanceState == "expired_before_issue") {
			finished = true
			return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
		}
		if v.DispatchedAt == nil {
			if !now.Before(v.Request.AcceptUntil) {
				p, err := r.Invoices.LockPayment(ctx, v.PaymentID)
				if err != nil {
					return err
				}
				v.IssuanceState = "expired_before_issue"
				v.NextReconcileAt = nil
				if v.CreditedMinor == 0 {
					p.Status = payment.StatusCanceled
				}
				if err := persistProjection(ctx, r, &v, &p, "deadline_elapsed", now); err != nil {
					return err
				}
				finished = true
				return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
			}
			if reconcile {
				finished = true
				v.NextReconcileAt = nil
				v.UpdatedAt = now
				if err := r.Invoices.Save(ctx, v); err != nil {
					return err
				}
				return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
			}
			firstDispatch = true
			v.DispatchedAt = &now
			v.IssuanceState = "dispatched"
			next := w.reconcileAt(now, v.Request.AcceptUntil)
			v.NextReconcileAt = &next
			v.UpdatedAt = now
			return r.Invoices.Save(ctx, v)
		}
		return nil
	})
	if err != nil || finished {
		return err
	}
	connection, provider, ok := w.registry.ForConnection(v.Connection.ID)
	if !ok || provider == nil || connection.MerchantID != v.MerchantID || connection.ProviderKind != v.Connection.ProviderKind || connection.EvidenceMode != v.EvidenceMode || connection.Beneficiary.Account != v.Connection.Beneficiary.Account {
		return errors.New("invoice connection unavailable")
	}
	callCtx, cancel := context.WithTimeout(ctx, w.cfg.ProviderTimeout)
	defer cancel()
	result := d.IssueResult{Outcome: d.OutcomeUnknown}
	var observations []d.Observation
	calledIssue := firstDispatch || !reconcile && v.Connection.Capabilities.CreationReplay == "provider_idempotent"
	if calledIssue {
		result, err = provider.Issue(callCtx, v)
		if err != nil {
			result = d.IssueResult{Outcome: d.OutcomeUnknown}
		}
	} else if v.ExternalInvoiceID == "" || v.IssuanceState == "unknown" || v.IssuanceState == "dispatched" {
		var recovered d.Recovery
		recovered, err = provider.Recover(callCtx, v)
		if err != nil {
			return err
		}
		if recovered.Found {
			result = recovered.Invoice
			observations = recovered.Observations
		} else {
			result = d.IssueResult{Outcome: d.OutcomeUnknown}
		}
	} else {
		observations, err = provider.Reconcile(callCtx, v)
		if err != nil {
			return err
		}
		result = d.IssueResult{Outcome: d.Issued, ExternalInvoiceID: v.ExternalInvoiceID, Artifact: v.Artifact, ProviderExpiresAt: v.ProviderExpiresAt}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return w.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.InvoiceCommands.Fence(ctx, c); err != nil {
			return err
		}
		current, err := r.Invoices.Lock(ctx, c.PaymentID)
		if err != nil {
			return err
		}
		p, err := r.Invoices.LockPayment(ctx, current.PaymentID)
		if err != nil {
			return err
		}
		now := w.now().UTC()
		reason := "issued"
		changed := false
		switch result.Outcome {
		case d.Issued:
			if result.ExternalInvoiceID == "" {
				result.Outcome = d.OutcomeUnknown
				break
			}
			if current.ExternalInvoiceID != "" && current.ExternalInvoiceID != result.ExternalInvoiceID {
				changed = addReview(&current, "invoice_terms_mismatch")
				break
			}
			changed = current.IssuanceState != "issued" || current.ExternalInvoiceID == ""
			current.ExternalInvoiceID = result.ExternalInvoiceID
			current.Artifact = result.Artifact
			current.ProviderExpiresAt = result.ProviderExpiresAt
			current.IssuanceState = "issued"
			changed = removeReview(&current, "issuance_unknown") || changed
			if !validArtifact(current) {
				changed = addReview(&current, "invoice_terms_mismatch") || changed
			}
			if p.Status == payment.StatusCreating && current.CreditedMinor == 0 {
				p.Status = payment.StatusPending
			}
		case d.DefinitelyRejected:
			// A Recover result cannot prove that an earlier POST will never create an object.
			if !calledIssue {
				result.Outcome = d.OutcomeUnknown
				break
			}
			changed = current.IssuanceState != "rejected"
			current.IssuanceState = "rejected"
			changed = removeReview(&current, "issuance_unknown") || changed
			reason = "issue_failed"
			if current.CreditedMinor == 0 {
				p.Status = payment.StatusFailed
			}
		default:
			result.Outcome = d.OutcomeUnknown
		}
		if result.Outcome == d.OutcomeUnknown && current.IssuanceState != "issued" {
			changed = current.IssuanceState != "unknown" || changed
			current.IssuanceState = "unknown"
			changed = addReview(&current, "issuance_unknown") || changed
			reason = "review_required"
		}
		if stopReconcile(current) {
			current.NextReconcileAt = nil
		} else {
			next := w.reconcileAt(now, current.Request.AcceptUntil)
			current.NextReconcileAt = &next
		}
		if changed {
			if err := persistProjection(ctx, r, &current, &p, reason, now); err != nil {
				return err
			}
		} else {
			current.UpdatedAt = now
			if err := r.Invoices.Save(ctx, current); err != nil {
				return err
			}
		}
		if current.ExternalInvoiceID != "" {
			inboxes, err := r.InvoiceInbox.Unmatched(ctx, current.Connection.ID, current.ExternalInvoiceID, current.Reference, recoveryBatch)
			if err != nil {
				return err
			}
			for _, inbox := range inboxes {
				if err := w.wake(ctx, r, inbox, now); err != nil {
					return err
				}
			}
		}
		for _, observation := range observations {
			if err := enqueueObservation(ctx, r, current.Connection.ID, observation, now); err != nil {
				return err
			}
		}
		status := "completed"
		code := ""
		if result.Outcome == d.OutcomeUnknown && !reconcile && current.Connection.Capabilities.CreationReplay == "provider_idempotent" && c.Attempts < w.cfg.MaxAttempts {
			status = "pending"
			code = "issuance_unknown"
		}
		return r.InvoiceCommands.Finish(ctx, c, status, now.Add(backoff(c.Attempts)), code, now)
	})
}
func validArtifact(v d.Record) bool {
	if v.ProviderExpiresAt != nil && v.ProviderExpiresAt.Before(v.Request.AcceptUntil) {
		return false
	}
	if v.Artifact == nil || v.Artifact.NextAction == nil {
		return false
	}
	a := v.Artifact.NextAction
	switch a.Type {
	case "bank_transfer":
		return a.Purpose != "" && a.Beneficiary != nil && a.Beneficiary.Name != "" && a.Beneficiary.INN != "" && a.Beneficiary.KPP != "" && a.Beneficiary.Account != "" && a.Beneficiary.BankBIK != "" && a.Beneficiary.BankName != "" && a.Beneficiary.CorrespondentAccount != ""
	case "redirect":
		u, err := url.Parse(a.URL)
		return err == nil && u.Scheme == "https" && u.Host != "" && a.ValidUntil != nil && !a.ValidUntil.Before(v.Request.AcceptUntil)
	}
	return false
}
func addReview(v *d.Record, reason string) bool {
	if slices.Contains(v.ReviewReasons, reason) {
		return false
	}
	v.ReviewReasons = append(v.ReviewReasons, reason)
	slices.Sort(v.ReviewReasons)
	return true
}
func removeReview(v *d.Record, reason string) bool {
	if !slices.Contains(v.ReviewReasons, reason) {
		return false
	}
	v.ReviewReasons = slices.DeleteFunc(v.ReviewReasons, func(s string) bool { return s == reason })
	return true
}
func persistProjection(ctx context.Context, r ports.Repositories, v *d.Record, p *payment.Payment, reason string, now time.Time) error {
	v.Revision++
	v.UpdatedAt = now
	return saveProjection(ctx, r, v, p, reason, now)
}

func saveProjection(ctx context.Context, r ports.Repositories, v *d.Record, p *payment.Payment, reason string, now time.Time) error {
	if len(v.ReviewReasons) > 0 {
		reason = "review_required"
	}
	if stopReconcile(*v) {
		v.NextReconcileAt = nil
	}
	if err := r.Invoices.Save(ctx, *v); err != nil {
		return err
	}
	if err := r.Invoices.SavePayment(ctx, *p, now); err != nil {
		return err
	}
	eventID := uuid.New()
	payload, err := json.Marshal(d.CallbackEnvelope{
		SchemaVersion: "invoice-integration/1", EventID: eventID, EventType: eventType, MerchantID: v.MerchantID, PaymentID: v.PaymentID, OrderID: v.Request.OrderID, PaymentMethod: "legal_entity_invoice", Revision: v.Revision, Reason: reason, OccurredAt: now, Status: string(p.Status), IssuanceState: v.IssuanceState, FundingState: v.FundingState, AmountMinor: v.Request.AmountMinor, CreditedMinor: v.CreditedMinor, Currency: v.Request.Currency, ConfirmedPaid: v.CreditedMinor >= v.Request.AmountMinor, RequiresReview: len(v.ReviewReasons) > 0, ReviewReasons: v.ReviewReasons, AcceptUntil: v.Request.AcceptUntil, PaymentConfirmedAt: v.PaymentConfirmedAt, ConfirmationRecordedAt: v.ConfirmationRecordedAt,
	})
	if err != nil {
		return err
	}
	return r.Outbox.Create(ctx, outbox.Event{ID: eventID, AggregateType: "payment", AggregateID: v.PaymentID, EventType: eventType, Payload: payload, Status: outbox.StatusPending, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now})
}
func enqueueObservation(ctx context.Context, r ports.Repositories, connectionID string, o d.Observation, now time.Time) error {
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(data)
	inbox := d.Inbox{ID: uuid.New(), ConnectionID: connectionID, EventKey: "readback:" + hex.EncodeToString(hash[:]), RawBodyHash: hash, Observation: o, VerificationKind: "authenticated_readback", ReceivedAt: now}
	created, err := r.InvoiceInbox.Create(ctx, inbox)
	if err != nil {
		return err
	}
	if !created {
		return nil
	}
	return r.InvoiceCommands.Create(ctx, newCommand("invoice.apply_notification", uuid.Nil, inbox.ID, now))
}
