package invoice

import (
	"reflect"
	"testing"
	"time"
)

func settlementRecord() Record {
	r := validRuleRequest()
	return Record{PaymentID: r.RequestID, Request: r, EvidenceMode: "bank_credits", ExternalInvoiceID: "invoice-1", Connection: ConnectionSnapshot{ID: "connection-1", Beneficiary: Beneficiary{Account: "test-account"}}, IssuanceState: "issued", FundingState: "none", Revision: 2}
}
func credit(key string, amount int64) Observation {
	return Observation{Kind: "settlement", FinancialKey: key, CanonicalFinancialKey: true, EvidenceMode: "bank_credits", ExternalInvoiceID: "invoice-1", AmountMinor: amount, Currency: "RUB", RecipientAccount: "test-account", PayerINN: "7701234567", FinalIncoming: true}
}
func hasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func TestSettlementCoverage(t *testing.T) {
	for _, tc := range []struct {
		name            string
		amount          int64
		funding, status string
		paid, review    bool
	}{
		{"partial", 99, "partial", "pending", false, false}, {"exact", 100, "paid", "succeeded", true, false}, {"overpaid", 101, "overpaid", "succeeded", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := settlementRecord()
			now := r.Request.AcceptUntil.Add(-time.Hour)
			got, err := ApplySettlement(r, "pending", nil, credit("op-1", tc.amount), now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Record.CreditedMinor != tc.amount || got.Record.FundingState != tc.funding || got.Status != tc.status || got.ConfirmedPaid != tc.paid || hasReason(got.Record.ReviewReasons, "overpaid") != tc.review {
				t.Fatalf("unexpected projection: %+v", got)
			}
			if got.ReviewCode != "" {
				t.Fatal("valid incoming money remains processed even when overpayment requires invoice review")
			}
			if got.Record.Revision != r.Revision+1 {
				t.Fatal("new financial fact must increment revision once")
			}
		})
	}
}

func TestSettlementFinancialDedupAndChangedOperation(t *testing.T) {
	r := settlementRecord()
	now := r.Request.AcceptUntil.Add(-time.Hour)
	first := credit("op-1", 40)
	a, err := ApplySettlement(r, "pending", nil, first, now)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := first
	duplicate.OperationChannel = "reconciliation"
	duplicate.ExternalOperationID = "statement-id"
	b, err := ApplySettlement(a.Record, a.Status, []Observation{first}, duplicate, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if b.Changed || !reflect.DeepEqual(a.Record, b.Record) {
		t.Fatal("same canonical financial fact changed projection")
	}
	changed := first
	changed.AmountMinor = 60
	c, err := ApplySettlement(a.Record, a.Status, []Observation{first}, changed, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Record.CreditedMinor != 40 || !hasReason(c.Record.ReviewReasons, "operation_changed") {
		t.Fatalf("changed operation credited again: %+v", c)
	}
	d, err := ApplySettlement(a.Record, a.Status, []Observation{first}, credit("op-2", 60), now)
	if err != nil {
		t.Fatal(err)
	}
	if d.Record.CreditedMinor != 100 || !d.ConfirmedPaid {
		t.Fatalf("different operations must sum: %+v", d)
	}
}

func TestInvoiceConfirmationDoesNotSumSnapshots(t *testing.T) {
	r := settlementRecord()
	r.EvidenceMode = "invoice_confirmation"
	now := r.Request.AcceptUntil.Add(-time.Hour)
	obs := credit("delivery-1", 100)
	obs.EvidenceMode = "invoice_confirmation"
	a, err := ApplySettlement(r, "pending", nil, obs, now)
	if err != nil {
		t.Fatal(err)
	}
	replay := obs
	replay.FinancialKey = "delivery-2"
	b, err := ApplySettlement(a.Record, a.Status, []Observation{obs}, replay, now)
	if err != nil {
		t.Fatal(err)
	}
	if b.Changed || b.Record.CreditedMinor != 100 || b.Record.Revision != a.Record.Revision {
		t.Fatalf("confirmation delivery duplicated settlement: %+v", b)
	}
}

func TestSettlementWithAmbiguousReferencesCannotCreditByExternalInvoiceID(t *testing.T) {
	r := settlementRecord()
	r.EvidenceMode = "invoice_confirmation"
	o := credit("caller-key", 100)
	o.EvidenceMode = "invoice_confirmation"
	o.ExternalInvoiceID = r.ExternalInvoiceID
	o.References = []string{"INV-0123456789ABCDEF0123456789ABCDEF", "INV-FEDCBA9876543210FEDCBA9876543210"}
	got, err := ApplySettlement(r, "pending", nil, o, r.Request.AcceptUntil.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.CreditedMinor != 0 || got.Evidence != nil || !hasReason(got.Record.ReviewReasons, "ambiguous_reference") || got.Status != "pending" {
		t.Fatalf("ambiguous reference was applied: %+v", got)
	}
}

func TestLateSettlementIsRetained(t *testing.T) {
	for _, status := range []string{"pending", "canceled", "failed"} {
		for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
			t.Run(status+"/"+delta.String(), func(t *testing.T) {
				r := settlementRecord()
				now := r.Request.AcceptUntil.Add(delta)
				got, err := ApplySettlement(r, status, nil, credit("op-1", 100), now)
				if err != nil {
					t.Fatal(err)
				}
				review := delta >= 0 || status != "pending"
				if got.Record.CreditedMinor != 100 || !got.ConfirmedPaid || got.Status != "succeeded" || (len(got.Record.ReviewReasons) > 0) != review {
					t.Fatalf("late financial fact lost or deadline wrong: %+v", got)
				}
				if delta >= 0 && !hasReason(got.Record.ReviewReasons, "acceptance_deadline_elapsed") {
					t.Fatal("expired deadline must be recorded as review reason")
				}
				if got.Record.ConfirmationRecordedAt == nil || !got.Record.ConfirmationRecordedAt.Equal(now) {
					t.Fatal("first confirmation must use injected clock")
				}
				replay, err := ApplySettlement(got.Record, got.Status, []Observation{credit("op-1", 100)}, credit("op-1", 100), now.Add(time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Record.ConfirmationRecordedAt, replay.Record.ConfirmationRecordedAt) {
					t.Fatal("repeat overwrote first confirmation")
				}
			})
		}
	}
}

func TestIncompatibleSettlementDoesNotIncreaseCoverage(t *testing.T) {
	cases := []struct {
		name, reason string
		change       func(*Observation)
	}{
		{"mixed_mode", "ambiguous_reference", func(o *Observation) { o.EvidenceMode = "invoice_confirmation" }},
		{"currency", "currency_mismatch", func(o *Observation) { o.Currency = "USD" }},
		{"payer", "payer_mismatch", func(o *Observation) { o.PayerINN = "7707654321" }},
		{"unproven_operation_key", "ambiguous_reference", func(o *Observation) { o.CanonicalFinancialKey = false }},
		{"correction", "financial_correction", func(o *Observation) { o.Kind = "correction" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := settlementRecord()
			obs := credit("op-1", 100)
			tc.change(&obs)
			got, err := ApplySettlement(r, "pending", nil, obs, r.Request.AcceptUntil.Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if got.Record.CreditedMinor != 0 || got.ConfirmedPaid || !hasReason(got.Record.ReviewReasons, tc.reason) {
				t.Fatalf("incompatible evidence counted or incident missing: %+v", got)
			}
		})
	}
}

func TestSettlementWaitsForDurableFinancialDeduplication(t *testing.T) {
	r := settlementRecord()
	now := r.Request.AcceptUntil.Add(-time.Hour)
	prepared, err := PrepareSettlement(r, "pending", credit("op-1", 100), now)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Record.ReviewReasons == nil {
		t.Fatal("empty review reasons must remain an empty list")
	}
	if prepared.Evidence == nil || prepared.Record.CreditedMinor != 0 || prepared.Changed || prepared.Record.Revision != r.Revision {
		t.Fatalf("preparation applied uncommitted money: %+v", prepared)
	}
	for _, tc := range []struct {
		name               string
		inserted, conflict bool
		credited           int64
		changed            bool
		review             string
	}{
		{"new", true, false, 100, true, ""},
		{"repeat", false, false, 0, false, ""},
		{"conflict", false, true, 0, true, "operation_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CompleteSettlement(prepared, tc.inserted, tc.conflict, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Record.CreditedMinor != tc.credited || got.Changed != tc.changed || got.ReviewCode != tc.review {
				t.Fatalf("unexpected deduplication result: %+v", got)
			}
			wantRevision := r.Revision
			if tc.changed {
				wantRevision++
			}
			if got.Record.Revision != wantRevision {
				t.Fatalf("revision=%d want=%d", got.Record.Revision, wantRevision)
			}
		})
	}
}
