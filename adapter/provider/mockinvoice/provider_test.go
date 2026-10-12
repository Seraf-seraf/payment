package mockinvoice

import (
	"context"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/google/uuid"
	"reflect"
	"testing"
)

func TestMockInvoiceIssueAndRecoverAreDeterministic(t *testing.T) {
	provider := New()
	v := d.Record{PaymentID: uuid.New(), InvoiceNumber: 42, Reference: "INV-00000000000000000000000000000000", Connection: d.ConnectionSnapshot{Beneficiary: d.Beneficiary{Name: "Mock recipient", Account: "00000000000000000000"}}}
	first, err := provider.Issue(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Issue(t.Context(), v)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || first.Outcome != d.Issued || first.ExternalInvoiceID != "mock_invoice_"+v.PaymentID.String() {
		t.Fatal("issue is not deterministic")
	}
	if first.Artifact == nil || first.Artifact.NextAction == nil || first.Artifact.NextAction.Type != "bank_transfer" || first.Artifact.NextAction.URL != "" || first.Artifact.NextAction.Purpose == "" || len(first.Artifact.Documents) != 0 || first.Artifact.Documents == nil || *first.Artifact.NextAction.Beneficiary != v.Connection.Beneficiary {
		t.Fatalf("unexpected artifact %+v", first.Artifact)
	}
	recovered, err := provider.Recover(t.Context(), v)
	if err != nil || !recovered.Found || !reflect.DeepEqual(recovered.Invoice, first) {
		t.Fatalf("recover: %+v %v", recovered, err)
	}
	observations, err := provider.Reconcile(t.Context(), v)
	if err != nil || len(observations) != 0 {
		t.Fatal("mock invented settlement")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := provider.Issue(ctx, v); err == nil {
		t.Fatal("canceled issue accepted")
	}
}
