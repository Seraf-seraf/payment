package cloudpayments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
)

type webhookEnvelope struct {
	ContentType string `json:"content_type"`
	RawBody     []byte `json:"raw_body"`
}

func decodeEnvelope(body []byte) (webhookEnvelope, error) {
	var e webhookEnvelope
	if err := json.Unmarshal(body, &e); err != nil {
		return e, ErrInvalidNotification
	}
	if (e.ContentType != "application/json" && e.ContentType != "application/x-www-form-urlencoded") || len(e.RawBody) == 0 || !utf8.Valid(e.RawBody) {
		return e, ErrInvalidNotification
	}
	return e, nil
}

func (p *Provider) VerifyWebhook(_ context.Context, headers http.Header, body []byte) error {
	e, err := decodeEnvelope(body)
	if err != nil {
		return err
	}
	signatures := headers.Values("Content-HMAC")
	if len(signatures) != 1 {
		return ErrInvalidSignature
	}
	signature, err := base64.StdEncoding.DecodeString(signatures[0])
	if err != nil || len(signature) != sha256.Size {
		return ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(p.apiSecret))
	_, _ = mac.Write(e.RawBody)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return ErrInvalidSignature
	}
	return nil
}

var notificationFields = []string{"InvoiceId", "TransactionId", "Amount", "Currency", "TestMode", "OperationType", "Status", "ReasonCode"}

func notificationValues(e webhookEnvelope) (map[string]string, error) {
	fields := make(map[string]string, len(notificationFields))
	if e.ContentType == "application/x-www-form-urlencoded" {
		values, err := url.ParseQuery(string(e.RawBody))
		if err != nil {
			return nil, ErrInvalidNotification
		}
		for _, key := range notificationFields {
			v, ok := values[key]
			if !ok {
				continue
			}
			if len(v) != 1 || !utf8.ValidString(v[0]) {
				return nil, ErrInvalidNotification
			}
			fields[key] = v[0]
		}
		return fields, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(e.RawBody, &values); err != nil || values == nil {
		return nil, ErrInvalidNotification
	}
	for _, key := range notificationFields {
		v, ok := values[key]
		if !ok {
			continue
		}
		if len(v) > 0 && v[0] == '"' {
			if key == "TestMode" {
				return nil, ErrInvalidNotification
			}
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, ErrInvalidNotification
			}
			fields[key] = s
		} else {
			fields[key] = string(v)
		}
	}
	return fields, nil
}

func (p *Provider) ParseWebhook(ctx context.Context, body []byte) (providerdomain.WebhookEvent, error) {
	var event providerdomain.WebhookEvent
	e, err := decodeEnvelope(body)
	if err != nil {
		return event, err
	}
	f, err := notificationValues(e)
	if err != nil {
		return event, err
	}
	invoiceID, err := uuid.Parse(f["InvoiceId"])
	if err != nil || invoiceID == uuid.Nil {
		return event, ErrInvalidNotification
	}
	tx, err := positiveInteger(f["TransactionId"])
	if err != nil {
		return event, err
	}
	amount, err := parseAmountMinor(f["Amount"])
	if err != nil {
		return event, err
	}
	if f["Currency"] == "" || f["OperationType"] != "Payment" {
		return event, ErrInvalidNotification
	}
	var testMode bool
	switch f["TestMode"] {
	case "1":
		testMode = true
	case "0":
		testMode = false
	case "true":
		if e.ContentType != "application/json" {
			return event, ErrInvalidNotification
		}
		testMode = true
	case "false":
		if e.ContentType != "application/json" {
			return event, ErrInvalidNotification
		}
	default:
		return event, ErrInvalidNotification
	}
	status, hasStatus := f["Status"]
	reason, hasReason := f["ReasonCode"]
	prefix := ""
	switch {
	case hasStatus && !hasReason && status == "Completed":
		prefix = "pay:"
		event.Status = paymentdomain.StatusSucceeded
		event.EventType = "payment.succeeded"
	case hasReason && !hasStatus:
		if _, err := positiveInteger(reason); err != nil {
			return event, err
		}
		prefix = "fail:"
		event.Status = paymentdomain.StatusPending
		event.EventType = "payment.pending"
	default:
		return event, ErrInvalidNotification
	}
	if testMode != p.testMode {
		return event, ErrPaymentMismatch
	}
	expected, err := p.payments.FindByID(ctx, invoiceID)
	if errors.Is(err, paymentservice.ErrNotFound) {
		return event, ErrPaymentNotReady
	}
	if err != nil {
		return event, fmt.Errorf("cloudpayments payment lookup: %w", err)
	}
	if expected.ProviderPaymentID == "" {
		return event, ErrPaymentNotReady
	}
	if expected.ProviderName != "cloudpayments" || expected.ProviderPaymentID != invoiceID.String() || expected.AmountMinor != amount || expected.Currency != f["Currency"] {
		return event, ErrPaymentMismatch
	}
	event.ProviderPaymentID = invoiceID.String()
	event.ProviderEventID = prefix + strconv.FormatInt(tx, 10)
	return event, nil
}

func positiveInteger(value string) (int64, error) {
	if !decimalDigits(value) {
		return 0, ErrInvalidNotification
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return 0, ErrInvalidNotification
	}
	return n, nil
}
func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseAmountMinor(value string) (int64, error) {
	whole, fraction, hasFraction := strings.Cut(value, ".")
	if !decimalDigits(whole) || (hasFraction && !decimalDigits(fraction)) {
		return 0, ErrInvalidNotification
	}
	rubles, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || rubles > math.MaxInt64/100 {
		return 0, ErrInvalidNotification
	}
	var kopecks int64
	for i, c := range fraction {
		if i >= 2 {
			if c != '0' {
				return 0, ErrInvalidNotification
			}
			continue
		}
		kopecks = kopecks*10 + int64(c-'0')
	}
	if len(fraction) == 1 {
		kopecks *= 10
	}
	if rubles*100 > math.MaxInt64-kopecks {
		return 0, ErrInvalidNotification
	}
	amount := rubles*100 + kopecks
	if amount <= 0 {
		return 0, ErrInvalidNotification
	}
	return amount, nil
}
