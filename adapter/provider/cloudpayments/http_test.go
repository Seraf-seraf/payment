package cloudpayments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type webhookHandlerFunc func(context.Context, string, http.Header, []byte) error

func (f webhookHandlerFunc) Handle(c context.Context, n string, h http.Header, b []byte) error {
	return f(c, n, h, b)
}

func TestCloudPaymentsHTTPResponseAndEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   int
	}{
		{"success", nil, 200, 0}, {"signature", ErrInvalidSignature, 401, 1}, {"notification", ErrInvalidNotification, 400, 1},
		{"mismatch", ErrPaymentMismatch, 409, 1}, {"not ready", ErrPaymentNotReady, 503, 1}, {"database", errors.New("database failure"), 503, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte("Description=%D0%9E%D0%BF%D0%BB%D0%B0%D1%82%D0%B0+%2B+%26&Amount=10.01")
			called := false
			next := webhookHandlerFunc(func(ctx context.Context, n string, h http.Header, b []byte) error {
				called = true
				var e testWebhookEnvelope
				if err := json.Unmarshal(b, &e); err != nil {
					t.Fatal(err)
				}
				if n != "cloudpayments" || e.ContentType != "application/x-www-form-urlencoded" || !bytes.Equal(e.RawBody, raw) || h.Get("Content-HMAC") != "signature" {
					t.Errorf("invalid envelope/header/provider")
				}
				return tc.err
			})
			req := httptest.NewRequest("POST", "/", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
			req.Header.Set("Content-HMAC", "signature")
			rec := httptest.NewRecorder()
			HandleWebhook(rec, req, next, nil)
			var body struct {
				Code *int `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !called || rec.Code != tc.status || body.Code == nil || *body.Code != tc.code {
				t.Fatalf("called=%v response=%d %s", called, rec.Code, rec.Body.String())
			}
		})
	}
}

type signalingWriter struct {
	header  http.Header
	written chan struct{}
	code    int
	body    bytes.Buffer
}

func (w *signalingWriter) Header() http.Header { return w.header }
func (w *signalingWriter) WriteHeader(n int) {
	w.code = n
	select {
	case w.written <- struct{}{}:
	default:
	}
}
func (w *signalingWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(200)
	}
	return w.body.Write(b)
}

func TestCloudPaymentsHTTPWaitsForHandlerBeforeAcknowledgement(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	next := webhookHandlerFunc(func(context.Context, string, http.Header, []byte) error {
		close(started)
		<-release
		return errors.New("commit failed")
	})
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := &signalingWriter{header: make(http.Header), written: make(chan struct{}, 2)}
	go func() { HandleWebhook(w, req, next, nil); close(done) }()
	select {
	case <-started:
	case <-done:
		t.Fatal("handler returned before next was invoked")
	}
	select {
	case <-w.written:
		t.Error("ACK written before processing completed")
	default:
	}
	close(release)
	<-done
	if w.code != 503 || strings.Contains(w.body.String(), `"code":0`) {
		t.Fatalf("response=%d %s", w.code, w.body.String())
	}
}

func TestCloudPaymentsHTTPBodyLimitAndContentType(t *testing.T) {
	for _, size := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			called := false
			next := webhookHandlerFunc(func(context.Context, string, http.Header, []byte) error { called = true; return nil })
			raw := "{}" + strings.Repeat(" ", size-2)
			req := httptest.NewRequest("POST", "/", strings.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			HandleWebhook(rec, req, next, nil)
			if size <= 1<<20 {
				if !called || rec.Code != 200 {
					t.Fatalf("size %d: called=%v status=%d", size, called, rec.Code)
				}
			} else if called || rec.Code != 413 || strings.TrimSpace(rec.Body.String()) != `{"code":1}` {
				t.Fatalf("oversize: called=%v status=%d", called, rec.Code)
			}
		})
	}
	for _, ct := range []string{"text/plain", "application/json; charset=windows-1251", "", "application/json; broken"} {
		t.Run(ct, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", ct)
			rec := httptest.NewRecorder()
			HandleWebhook(rec, req, webhookHandlerFunc(func(context.Context, string, http.Header, []byte) error { t.Error("unexpected next"); return nil }), nil)
			if rec.Code != 415 || strings.TrimSpace(rec.Body.String()) != `{"code":1}` {
				t.Fatalf("status=%d", rec.Code)
			}
		})
	}
}

func TestCloudPaymentsHTTPSafeErrorLogging(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"email":"private@example.test"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-HMAC", "private-signature")
	rec := httptest.NewRecorder()
	HandleWebhook(rec, req, webhookHandlerFunc(func(context.Context, string, http.Header, []byte) error {
		return errors.New("secret private@example.test private-signature")
	}), logger)
	if rec.Code != 503 {
		t.Fatalf("status=%d", rec.Code)
	}
	for _, s := range []string{"private@example.test", "private-signature", "secret"} {
		if strings.Contains(output.String()+rec.Body.String(), s) {
			t.Errorf("leaked %s", s)
		}
	}
	if strings.Count(strings.TrimSpace(output.String()), "\n") != 0 || !strings.Contains(output.String(), "cloudpayments") {
		t.Errorf("expected one structured log: %s", output.String())
	}
}
