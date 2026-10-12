package invoice

import (
	"context"
	"errors"
	"testing"
	"time"

	d "github.com/Seraf-seraf/payment/domain/invoice"
	merchant "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

// The transaction commits normally; only its acknowledgement is lost.
type lostCreateReplyTx struct {
	base ports.TransactionManager
	lost bool
}

func (tx *lostCreateReplyTx) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	if err := tx.base.WithinTx(ctx, fn); err != nil {
		return err
	}
	if !tx.lost {
		tx.lost = true
		return errors.New("commit acknowledgement lost")
	}
	return nil
}

func TestApplicationCreateReplayAfterLostCommitReply(t *testing.T) {
	repos := newMemoryRepositories()
	m := merchant.Merchant{ID: uuid.New(), CallbackURL: "https://example.test/callback"}
	tx := &lostCreateReplyTx{base: repos}
	s := NewService(repos.invoices, fixedConnectionRegistry{merchantID: m.ID}, tx, fixedNow)
	req := validRequest()
	if _, err := s.CreateInvoicePayment(t.Context(), req, m); err == nil {
		t.Fatal("lost acknowledgement was not injected")
	}
	if len(repos.payments.created) != 1 || len(repos.invoices.records) != 1 || len(repos.commands.commands) != 1 || len(repos.outbox.events) != 1 {
		t.Fatal("successful commit did not persist all side effects")
	}
	original, err := repos.invoices.FindByMerchantAndRequestID(t.Context(), m.ID, req.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateInvoicePayment(t.Context(), req, m)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Created || replay.Invoice.PaymentID != original.PaymentID || replay.Invoice.InvoiceNumber != original.InvoiceNumber || replay.Invoice.Reference != original.Reference {
		t.Fatalf("lost reply replay changed identity: %+v", replay)
	}
	if len(repos.payments.created) != 1 || len(repos.commands.commands) != 1 || len(repos.outbox.events) != 1 {
		t.Fatal("replay emitted duplicate side effects")
	}
}

func TestApplicationCreateHashConflictIncludesAllInvoiceTerms(t *testing.T) {
	cases := []struct {
		name string
		edit func(*d.Request)
	}{
		{"payer_name", func(r *d.Request) { r.Payer.Name += " другое" }},
		{"payer_inn", func(r *d.Request) { r.Payer.INN = "7707654321" }},
		{"payer_kpp", func(r *d.Request) { r.Payer.KPP = "770701001" }},
		{"payer_address", func(r *d.Request) { r.Payer.LegalAddress += " другой" }},
		{"payer_email", func(r *d.Request) { r.Payer.Email = "other@example.test" }},
		{"deadline", func(r *d.Request) { r.AcceptUntil = r.AcceptUntil.Add(time.Second) }},
		{"zero_vat_vs_none", func(r *d.Request) { rate := int32(0); r.Lines[0].VAT = d.VAT{Kind: "included", RateBasisPoints: &rate} }},
		{"line_name", func(r *d.Request) { r.Lines[0].Name += " другой" }},
		{"line_order", func(r *d.Request) { r.Lines[0], r.Lines[1] = r.Lines[1], r.Lines[0] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos := newMemoryRepositories()
			m := merchant.Merchant{ID: uuid.New(), CallbackURL: "https://example.test/callback"}
			s := NewService(repos.invoices, fixedConnectionRegistry{merchantID: m.ID}, repos, fixedNow)
			req := validRequest()
			req.Lines = append(req.Lines, d.Line{ID: "second", Name: "Услуга", Quantity: 1, Unit: "service", UnitPriceMinor: 1, AmountMinor: 1, VAT: d.VAT{Kind: "none"}})
			req.AmountMinor++
			first, err := s.CreateInvoicePayment(t.Context(), req, m)
			if err != nil {
				t.Fatal(err)
			}
			changed := req
			changed.Lines = append([]d.Line(nil), req.Lines...)
			tc.edit(&changed)
			if _, err := s.CreateInvoicePayment(t.Context(), changed, m); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("changed term must conflict: %v", err)
			}
			stored, err := repos.invoices.FindByMerchantAndRequestID(t.Context(), m.ID, req.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.RequestHash != first.Invoice.RequestHash || stored.PaymentID != first.Invoice.PaymentID || len(repos.payments.created) != 1 || len(repos.commands.commands) != 1 || len(repos.outbox.events) != 1 {
				t.Fatal("hash conflict mutated accepted intention")
			}
		})
	}
}
