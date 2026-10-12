package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	mock "github.com/Seraf-seraf/payment/adapter/provider/mockinvoice"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/ports"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testInvoiceDecoders struct {
	decoder ports.InvoiceNotificationDecoder
}

func (r testInvoiceDecoders) DecoderForConnection(id string) (d.Connection, ports.InvoiceNotificationDecoder, bool) {
	return d.Connection{ID: "test-connection"}, r.decoder, id == "test-connection"
}

type acceptNotification struct {
	calls int
	err   error
}

func (s *acceptNotification) AcceptInvoiceNotification(ctx context.Context, c d.Connection, n ports.VerifiedInvoiceNotification) error {
	s.calls++
	return s.err
}
func invoiceSignedRequest(body, secret string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/provider-invoice-webhooks/test-connection", strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Mock-Signature", hex.EncodeToString(mac.Sum(nil)))
	return req
}
func invoiceTestRouter(t *testing.T, accept *acceptNotification) http.Handler {
	t.Helper()
	decoder, err := mock.NewNotificationDecoder("invoice-secret", "bank_credits", "shared_operation_id", nil)
	if err != nil {
		t.Fatal(err)
	}
	return NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, time.Minute, InvoiceWebhookDependencies{Notifications: accept, Decoders: testInvoiceDecoders{decoder}})
}
func TestInvoiceWebhookOnlyAcknowledgesDurableAcceptance(t *testing.T) {
	body := `{"type":"status_hint","external_invoice_id":"external-1"}`
	for _, tc := range []struct {
		name, secret, body string
		err                error
		want, calls        int
	}{
		{name: "verified", secret: "invoice-secret", body: body, want: 200, calls: 1},
		{name: "bad signature", secret: "acquiring-secret", body: body, want: 401},
		{name: "malformed", secret: "invoice-secret", body: `{"type":"paid"}`, want: 400},
		{name: "infrastructure", secret: "invoice-secret", body: body, err: errors.New("storage failure"), want: 503, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accept := &acceptNotification{err: tc.err}
			rec := httptest.NewRecorder()
			invoiceTestRouter(t, accept).ServeHTTP(rec, invoiceSignedRequest(tc.body, tc.secret))
			if rec.Code != tc.want || accept.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, accept.calls, rec.Body.String())
			}
			if rec.Code == 200 && rec.Body.String() != `{"status":"ok"}` {
				t.Fatal("ACK changed according to business state")
			}
		})
	}
}
func TestInvoiceRoutesBoundBodyBeforeOpenAPIValidation(t *testing.T) {
	for _, path := range []string{"/api/v1/invoice-payments", "/api/v1/provider-invoice-webhooks/test-connection"} {
		for _, unknownLength := range []bool{false, true} {
			t.Run(path+"chunked="+strconv.FormatBool(unknownLength), func(t *testing.T) {
				accept := &acceptNotification{}
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(strings.Repeat("x", int(maxInvoiceBodyBytes+1))))
				if unknownLength {
					req.ContentLength = -1
				}
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				invoiceTestRouter(t, accept).ServeHTTP(rec, req)
				if rec.Code != 413 || accept.calls != 0 {
					t.Fatalf("status=%d calls=%d", rec.Code, accept.calls)
				}
			})
		}
	}
	body := `{"type":"status_hint","external_invoice_id":"external-1"}`
	body += strings.Repeat(" ", int(maxInvoiceBodyBytes)-len(body))
	accept := &acceptNotification{}
	rec := httptest.NewRecorder()
	invoiceTestRouter(t, accept).ServeHTTP(rec, invoiceSignedRequest(body, "invoice-secret"))
	if rec.Code != 200 || accept.calls != 1 {
		t.Fatalf("exact 1MiB boundary: status=%d calls=%d", rec.Code, accept.calls)
	}
}
