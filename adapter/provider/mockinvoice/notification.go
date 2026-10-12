package mockinvoice

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/ports"
	"io"
	"mime"
	"strings"
	"time"
)

type NotificationDecoder struct {
	secret   []byte
	mode     string
	strategy string
	aliases  map[string]string
}

func NewNotificationDecoder(secret, mode, strategy string, aliases map[string]string) (*NotificationDecoder, error) {
	if secret == "" || mode != "bank_credits" && mode != "invoice_confirmation" || strategy != "" && strategy != "shared_operation_id" && strategy != "mapped_operation_id" {
		return nil, ports.ErrInvoiceNotificationMalformed
	}
	saved := make(map[string]string, len(aliases))
	for key, value := range aliases {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return nil, ports.ErrInvoiceNotificationMalformed
		}
		saved[key] = value
	}
	return &NotificationDecoder{secret: []byte(secret), mode: mode, strategy: strategy, aliases: saved}, nil
}
func (d *NotificationDecoder) Acknowledgement() ports.InvoiceAcknowledgement {
	return ports.InvoiceAcknowledgement{Status: 200, ContentType: "application/json", Body: []byte(`{"status":"ok"}`)}
}

type mockNotification struct {
	EventID            string     `json:"event_id,omitempty"`
	Kind               string     `json:"type"`
	ExternalInvoiceID  string     `json:"external_invoice_id,omitempty"`
	Reference          string     `json:"reference,omitempty"`
	OperationID        string     `json:"operation_id,omitempty"`
	OperationChannel   string     `json:"operation_channel,omitempty"`
	AmountMinor        int64      `json:"amount_minor,omitempty"`
	Currency           string     `json:"currency,omitempty"`
	RecipientAccount   string     `json:"recipient_account,omitempty"`
	PayerINN           string     `json:"payer_inn,omitempty"`
	FinalIncoming      bool       `json:"final_incoming,omitempty"`
	ProviderOccurredAt *time.Time `json:"provider_occurred_at,omitempty"`
}

func (dec *NotificationDecoder) VerifyAndDecode(envelope ports.InvoiceNotificationEnvelope) (ports.VerifiedInvoiceNotification, error) {
	var signature string
	for key, values := range envelope.Headers {
		if strings.EqualFold(key, "X-Mock-Signature") && len(values) == 1 {
			signature = values[0]
		}
	}
	provided, err := hex.DecodeString(signature)
	if err != nil || len(provided) != sha256.Size {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationSignature
	}
	mac := hmac.New(sha256.New, dec.secret)
	_, _ = mac.Write(envelope.Body)
	if !hmac.Equal(provided, mac.Sum(nil)) {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationSignature
	}
	mediaType, _, err := mime.ParseMediaType(envelope.ContentType)
	if err != nil || mediaType != "application/json" {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
	}
	var body mockNotification
	decoder := json.NewDecoder(bytes.NewReader(envelope.Body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
	}
	if body.Kind != "status_hint" && body.Kind != "settlement" && body.Kind != "financial_correction" || len(body.EventID) > 200 || len(body.ExternalInvoiceID) > 200 || len(body.OperationID) > 200 || body.OperationChannel != "" && body.OperationChannel != "webhook" && body.OperationChannel != "statement" {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
	}
	reference, references, ok := normalizeReference(body.Reference)
	if !ok {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
	}
	if body.Kind == "settlement" && (body.AmountMinor < 1 || body.AmountMinor > d.MaxAmountMinor || !validCurrency(body.Currency) || body.RecipientAccount == "" || body.PayerINN == "") {
		return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
	}
	o := d.Observation{Kind: body.Kind, ExternalInvoiceID: body.ExternalInvoiceID, Reference: reference, References: references, ExternalOperationID: body.OperationID, OperationChannel: body.OperationChannel, EvidenceMode: dec.mode, AmountMinor: body.AmountMinor, Currency: body.Currency, RecipientAccount: body.RecipientAccount, PayerINN: body.PayerINN, FinalIncoming: body.FinalIncoming, ProviderOccurredAt: body.ProviderOccurredAt}
	if dec.mode == "bank_credits" && body.OperationID != "" {
		canonical := ""
		switch dec.strategy {
		case "shared_operation_id":
			canonical = body.OperationID
		case "mapped_operation_id":
			channel := body.OperationChannel
			if channel == "" {
				channel = "webhook"
			}
			canonical = dec.aliases[channel+":"+body.OperationID]
		}
		if canonical != "" {
			o.FinancialKey = "bank:" + canonical
			o.CanonicalFinancialKey = true
		}
	}
	if body.ProviderOccurredAt != nil {
		v := body.ProviderOccurredAt.UTC()
		o.ProviderOccurredAt = &v
	}
	eventKey := body.EventID
	if eventKey == "" {
		semantic := o
		if semantic.CanonicalFinancialKey {
			semantic.ExternalOperationID = ""
			semantic.OperationChannel = ""
		}
		data, err := json.Marshal(semantic)
		if err != nil {
			return ports.VerifiedInvoiceNotification{}, ports.ErrInvoiceNotificationMalformed
		}
		hash := sha256.Sum256(data)
		eventKey = "semantic:" + hex.EncodeToString(hash[:])
	}
	return ports.VerifiedInvoiceNotification{EventKey: eventKey, RawBodyHash: sha256.Sum256(envelope.Body), Observation: o, VerificationKind: "mock_hmac_sha256"}, nil
}
func validReference(s string) bool {
	if len(s) != 36 || !strings.HasPrefix(s, "INV-") {
		return false
	}
	for _, c := range s[4:] {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// normalizeReference preserves the exact single-reference contract. Multiple
// references are accepted only when every whitespace-delimited token is valid;
// distinct references are retained in normalized form for review.
func normalizeReference(s string) (string, []string, bool) {
	if s == "" || validReference(s) {
		return s, nil, true
	}
	tokens := strings.Fields(s)
	if len(tokens) < 2 {
		return "", nil, false
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if !validReference(token) {
			return "", nil, false
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		unique = append(unique, token)
	}
	if len(unique) == 1 {
		return unique[0], nil, true
	}
	return "", unique, true
}
func validCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, c := range s {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}
