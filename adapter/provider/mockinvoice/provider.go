package mockinvoice

import (
	"context"
	"fmt"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"time"
)

type Provider struct{ capabilities d.Capabilities }

func New() *Provider {
	return &Provider{capabilities: d.Capabilities{SupportedUnits: []string{"piece", "set", "service"}, SupportsVATNone: true, SupportedVATRatesBPS: []int32{0, 500, 700, 1000, 2000, 2200}, MaxAcceptDuration: 5 * 24 * time.Hour, CreationReplay: "provider_idempotent"}}
}

func (p *Provider) Capabilities() d.Capabilities { return p.capabilities }
func (p *Provider) Issue(ctx context.Context, v d.Record) (d.IssueResult, error) {
	if err := ctx.Err(); err != nil {
		return d.IssueResult{Outcome: d.OutcomeUnknown}, err
	}
	beneficiary := v.Connection.Beneficiary
	return d.IssueResult{Outcome: d.Issued, ExternalInvoiceID: "mock_invoice_" + v.PaymentID.String(), Artifact: &d.Artifact{NextAction: &d.NextAction{Type: "bank_transfer", Purpose: fmt.Sprintf("Оплата счёта %d %s", v.InvoiceNumber, v.Reference), Beneficiary: &beneficiary}, Documents: []d.Document{}}}, nil
}
func (p *Provider) Recover(ctx context.Context, v d.Record) (d.Recovery, error) {
	// The local adapter has no external side effect: the deterministic invoice is recoverable.
	result, err := p.Issue(ctx, v)
	return d.Recovery{Found: err == nil, Invoice: result}, err
}
func (p *Provider) Reconcile(ctx context.Context, v d.Record) ([]d.Observation, error) {
	return nil, ctx.Err()
}
