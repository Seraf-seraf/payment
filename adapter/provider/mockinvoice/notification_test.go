package mockinvoice

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/Seraf-seraf/payment/ports"
	"strings"
	"testing"
)

func signedEnvelope(body string, secret string) ports.InvoiceNotificationEnvelope {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return ports.InvoiceNotificationEnvelope{Body: []byte(body), ContentType: "application/json", Headers: map[string][]string{"X-Mock-Signature": {hex.EncodeToString(mac.Sum(nil))}}}
}
func TestInvoiceDecoderSignatureAndSemanticEventKey(t *testing.T) {
	decoder, err := NewNotificationDecoder("test-secret", "bank_credits", "mapped_operation_id", map[string]string{"webhook:w-1": "canonical-1", "statement:s-1": "canonical-1"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"type":"settlement","operation_id":"w-1","amount_minor":100,"currency":"RUB","recipient_account":"account","payer_inn":"7701234567","final_incoming":true}`
	reordered := `{"currency":"RUB","amount_minor":100,"operation_id":"w-1","type":"settlement","payer_inn":"7701234567","final_incoming":true,"recipient_account":"account"}`
	first, err := decoder.VerifyAndDecode(signedEnvelope(body, "test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := decoder.VerifyAndDecode(signedEnvelope(reordered, "test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if first.EventKey != second.EventKey || first.RawBodyHash == second.RawBodyHash {
		t.Fatal("semantic key depends on JSON order, or raw body hash was normalized")
	}
	if !first.Observation.CanonicalFinancialKey || first.Observation.FinancialKey != "bank:canonical-1" {
		t.Fatal("server-side canonical mapping not applied")
	}
	if _, err := decoder.VerifyAndDecode(signedEnvelope(body, "other-secret")); !errors.Is(err, ports.ErrInvoiceNotificationSignature) {
		t.Fatalf("wrong secret error=%v", err)
	}
	tampered := signedEnvelope(body, "test-secret")
	tampered.Body = append(tampered.Body, ' ')
	if _, err := decoder.VerifyAndDecode(tampered); !errors.Is(err, ports.ErrInvoiceNotificationSignature) {
		t.Fatal("signature did not cover exact bytes")
	}
	if _, err := NewNotificationDecoder("", "bank_credits", "", nil); err == nil {
		t.Fatal("empty secret accepted")
	}
}
func TestInvoiceDecoderDoesNotTrustCallerFinancialKey(t *testing.T) {
	decoder, err := NewNotificationDecoder("test-secret", "bank_credits", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"type":"settlement","operation_id":"unproven-id","amount_minor":100,"currency":"RUB","recipient_account":"account","payer_inn":"7701234567","final_incoming":true}`
	decoded, err := decoder.VerifyAndDecode(signedEnvelope(body, "test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Observation.CanonicalFinancialKey || decoded.Observation.FinancialKey != "" || decoded.Observation.ExternalOperationID != "unproven-id" {
		t.Fatal("unproven operation ID granted automatic credit or was discarded")
	}
	forged := `{"type":"settlement","operation_id":"unproven-id","amount_minor":100,"currency":"RUB","recipient_account":"account","payer_inn":"7701234567","final_incoming":true,"canonical_financial_key":true}`
	if _, err := decoder.VerifyAndDecode(signedEnvelope(forged, "test-secret")); !errors.Is(err, ports.ErrInvoiceNotificationMalformed) {
		t.Fatal("caller supplied canonical-key proof was accepted")
	}
}

func TestInvoiceDecoderRetainsMultipleReferencesForReview(t *testing.T) {
	decoder, err := NewNotificationDecoder("test-secret", "bank_credits", "shared_operation_id", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := "INV-0123456789ABCDEF0123456789ABCDEF"
	second := "INV-FEDCBA9876543210FEDCBA9876543210"
	for _, tc := range []struct {
		name      string
		reference string
		want      string
		ambiguous bool
		wantError bool
	}{
		{name: "single remains exact", reference: first, want: first},
		{name: "repeated reference is not ambiguous", reference: first + " " + first, want: first},
		{name: "multiple references retained", reference: first + "  " + second, want: first + " " + second, ambiguous: true},
		{name: "unknown token rejected", reference: first + " OTHER", wantError: true},
		{name: "malformed token rejected", reference: first + " INV-0123456789abcdef0123456789abcdef", wantError: true},
		{name: "single with surrounding whitespace remains invalid", reference: " " + first, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"type":"settlement","reference":"` + tc.reference + `","amount_minor":100,"currency":"RUB","recipient_account":"account","payer_inn":"7701234567","final_incoming":true}`
			got, err := decoder.VerifyAndDecode(signedEnvelope(body, "test-secret"))
			if tc.wantError {
				if !errors.Is(err, ports.ErrInvoiceNotificationMalformed) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.ambiguous {
				if got.Observation.Reference != "" || strings.Join(got.Observation.References, " ") != tc.want {
					t.Fatalf("reference=%q references=%q", got.Observation.Reference, got.Observation.References)
				}
				return
			}
			if got.Observation.Reference != tc.want || len(got.Observation.References) != 0 {
				t.Fatalf("reference=%q references=%q", got.Observation.Reference, got.Observation.References)
			}
		})
	}
}

func TestMockInvoiceNotificationsKeepConfiguredEvidenceMode(t *testing.T) {
	for _, mode := range []string{"invoice_confirmation", "bank_credits"} {
		t.Run(mode, func(t *testing.T) {
			decoder, err := NewNotificationDecoder("invoice-secret", mode, "shared_operation_id", nil)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"type":"settlement","external_invoice_id":"invoice","operation_id":"operation","amount_minor":1000,"currency":"RUB","recipient_account":"00000000000000000000","payer_inn":"7701234567","final_incoming":true}`)
			mac := hmac.New(sha256.New, []byte("invoice-secret"))
			_, _ = mac.Write(body)
			n, err := decoder.VerifyAndDecode(ports.InvoiceNotificationEnvelope{Body: body, ContentType: "application/json", Headers: map[string][]string{"X-Mock-Signature": {hex.EncodeToString(mac.Sum(nil))}}})
			if err != nil {
				t.Fatal(err)
			}
			if n.Observation.EvidenceMode != mode {
				t.Fatal("evidence mode changed")
			}
			if mode == "bank_credits" && (!n.Observation.CanonicalFinancialKey || n.Observation.FinancialKey != "bank:operation") {
				t.Fatal("bank operation proof missing")
			}
			if mode == "invoice_confirmation" && n.Observation.FinancialKey != "" {
				t.Fatal("confirmation mixed with bank credits")
			}
		})
	}
}
