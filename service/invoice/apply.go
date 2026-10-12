package invoice

import (
	"context"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	payment "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
	"time"
)

func (w *Worker) applyNotification(ctx context.Context, c ports.InvoiceCommand) error {
	return w.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		if err := r.InvoiceCommands.Fence(ctx, c); err != nil {
			return err
		}
		inbox, err := r.InvoiceInbox.Get(ctx, c.InboxID)
		if err != nil {
			return err
		}
		now := w.now().UTC()
		if inbox.State == "processed" || inbox.State == "review" {
			return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
		}
		if len(inbox.Observation.References) > 1 {
			if err := r.InvoiceInbox.SetState(ctx, inbox.ID, "review", "ambiguous_reference", nil, now); err != nil {
				return err
			}
			return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
		}
		ids, err := r.Invoices.Match(ctx, inbox.ConnectionID, inbox.Observation.ExternalInvoiceID, inbox.Observation.Reference)
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			state, code := "unmatched", ""
			var next *time.Time
			if len(ids) > 1 {
				state, code = "review", "ambiguous_reference"
			} else {
				due := now.Add(scheduleInterval)
				next = &due
			}
			if err := r.InvoiceInbox.SetState(ctx, inbox.ID, state, code, next, now); err != nil {
				return err
			}
			return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
		}
		target, err := r.Invoices.Lock(ctx, ids[0])
		if err != nil {
			return err
		}
		if inbox.Observation.Kind == "status_hint" {
			if err := r.InvoiceCommands.Create(ctx, newCommand("invoice.reconcile", target.PaymentID, uuid.Nil, now)); err != nil {
				return err
			}
			if err := r.InvoiceInbox.SetState(ctx, inbox.ID, "processed", "", nil, now); err != nil {
				return err
			}
		} else {
			p, err := r.Invoices.LockPayment(ctx, target.PaymentID)
			if err != nil {
				return err
			}
			if err := applyEvidence(ctx, r, &target, &p, inbox, now); err != nil {
				return err
			}
		}
		return r.InvoiceCommands.Finish(ctx, c, "completed", now, "", now)
	})
}

// Financial facts from inbox and read-back share this application and deduplication path.
func applyEvidence(ctx context.Context, r ports.Repositories, v *d.Record, p *payment.Payment, inbox d.Inbox, now time.Time) error {
	result, err := d.PrepareSettlement(*v, string(p.Status), inbox.Observation, now)
	if err != nil {
		return err
	}
	inserted, conflict := false, false
	if result.Evidence != nil {
		evidence := *result.Evidence
		evidence.ID = uuid.New()
		evidence.VerificationKind = inbox.VerificationKind
		inserted, conflict, err = r.Invoices.RecordEvidence(ctx, evidence)
		if err != nil {
			return err
		}
	}
	result, err = d.CompleteSettlement(result, inserted, conflict, now)
	if err != nil {
		return err
	}
	if result.Changed {
		*v = result.Record
		p.Status = payment.Status(result.Status)
		if err := saveProjection(ctx, r, v, p, "receipt_recorded", now); err != nil {
			return err
		}
	}
	state := "processed"
	if result.ReviewCode != "" {
		state = "review"
	}
	return r.InvoiceInbox.SetState(ctx, inbox.ID, state, result.ReviewCode, nil, now)
}
