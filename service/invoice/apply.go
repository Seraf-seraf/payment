package invoice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	o := inbox.Observation
	changed := false
	eligible := true
	reviewCode := ""
	reject := func(reason string) { eligible = false; reviewCode = reason; changed = addReview(v, reason) || changed }
	if o.Kind == "financial_correction" {
		reject("financial_correction")
	}
	if o.Kind != "settlement" || !o.FinalIncoming || o.EvidenceMode != v.EvidenceMode || o.AmountMinor < 1 || o.AmountMinor > d.MaxAmountMinor || o.RecipientAccount == "" || v.Connection.Beneficiary.Account == "" || o.RecipientAccount != v.Connection.Beneficiary.Account {
		if reviewCode == "" {
			reject("ambiguous_reference")
		}
	}
	if o.Currency != v.Request.Currency {
		reject("currency_mismatch")
	}
	if o.PayerINN != v.Request.Payer.INN {
		reject("payer_mismatch")
	}
	if v.EvidenceMode == "invoice_confirmation" {
		if o.ExternalInvoiceID == "" || o.ExternalInvoiceID != v.ExternalInvoiceID {
			reject("ambiguous_reference")
			o.FinancialKey = ""
		} else {
			o.FinancialKey = "invoice:" + o.ExternalInvoiceID + ":final_settlement"
		}
	} else if o.FinancialKey == "" || !o.CanonicalFinancialKey {
		reject("ambiguous_reference")
		o.FinancialKey = ""
	}
	// Invalid or correction observations remain durable incidents, never invented positive evidence.
	newFact := false
	if o.Kind == "settlement" && o.FinalIncoming && o.EvidenceMode == v.EvidenceMode && o.AmountMinor > 0 && o.AmountMinor <= d.MaxAmountMinor && o.FinancialKey != "" && len(o.Currency) == 3 {
		data, err := json.Marshal(struct {
			Kind               string
			FinancialKey       string
			EvidenceMode       string
			AmountMinor        int64
			Currency           string
			RecipientAccount   string
			PayerINN           string
			FinalIncoming      bool
			ProviderOccurredAt *time.Time
		}{o.Kind, o.FinancialKey, o.EvidenceMode, o.AmountMinor, o.Currency, o.RecipientAccount, o.PayerINN, o.FinalIncoming, o.ProviderOccurredAt})
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		inserted, conflict, err := r.Invoices.RecordEvidence(ctx, d.Evidence{ID: uuid.New(), PaymentID: v.PaymentID, ConnectionID: v.Connection.ID, FinancialKey: o.FinancialKey, NormalizedHash: hash, EvidenceMode: v.EvidenceMode, AmountMinor: o.AmountMinor, Currency: o.Currency, Eligible: eligible, RecordedAt: now, ProviderOccurredAt: o.ProviderOccurredAt, VerificationKind: inbox.VerificationKind})
		if err != nil {
			return err
		}
		if conflict {
			reject("operation_changed")
		} else if inserted {
			newFact = true
			if eligible {
				if v.CreditedMinor > d.MaxAmountMinor-o.AmountMinor {
					return errors.New("invoice credited amount overflow")
				}
				// invoice_confirmation has a single final evidence, guarded by its partial UNIQUE.
				v.CreditedMinor += o.AmountMinor
			}
		}
	}
	if newFact || changed {
		switch {
		case v.CreditedMinor == 0:
			v.FundingState = "none"
		case v.CreditedMinor < v.Request.AmountMinor:
			v.FundingState = "partial"
		case v.CreditedMinor == v.Request.AmountMinor:
			v.FundingState = "paid"
		default:
			v.FundingState = "overpaid"
			changed = addReview(v, "overpaid") || changed
		}
		if v.CreditedMinor >= v.Request.AmountMinor {
			if !now.Before(v.Request.AcceptUntil) || p.Status == payment.StatusCanceled || p.Status == payment.StatusFailed {
				changed = addReview(v, "acceptance_deadline_elapsed") || changed
			}
			p.Status = payment.StatusSucceeded
			if v.ConfirmationRecordedAt == nil {
				v.ConfirmationRecordedAt = &now
				v.PaymentConfirmedAt = o.ProviderOccurredAt
			}
		}
		if err := persistProjection(ctx, r, v, p, "receipt_recorded", now); err != nil {
			return err
		}
	}
	state := "processed"
	if !eligible || reviewCode != "" {
		state = "review"
	}
	return r.InvoiceInbox.SetState(ctx, inbox.ID, state, reviewCode, nil, now)
}
