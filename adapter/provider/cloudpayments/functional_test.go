//go:build functional

package cloudpayments_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	cp "github.com/Seraf-seraf/payment/adapter/provider/cloudpayments"
	pg "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	"github.com/Seraf-seraf/payment/bootstrap"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/internal/testpostgres"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	webhookservice "github.com/Seraf-seraf/payment/service/webhook"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(testpostgres.Run(m)) }

type cpAuth struct{ merchant merchantdomain.Merchant }

func (a cpAuth) AuthenticateAPIKey(_ context.Context, key string) (merchantdomain.Merchant, error) {
	m := a.merchant
	if key == "other" {
		m.ID = uuid.New()
	}
	return m, nil
}

type harness struct {
	pool     *pgxpool.Pool
	payments *pg.PaymentRepository
	router   http.Handler
	calls    atomic.Int32
	merchant merchantdomain.Merchant
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func newHarness(t *testing.T, beforeReply func(string)) *harness {
	t.Helper()
	pool, _ := testpostgres.Schema(t, "../../../migrations")
	h := &harness{pool: pool, payments: pg.NewPaymentRepository(pool), merchant: merchantdomain.Merchant{ID: uuid.New(), SharedSecret: "merchant-secret", ProviderName: "cloudpayments", IsActive: true}}
	_, err := pool.Exec(t.Context(), `INSERT INTO merchants(id,name,api_key_hash,shared_secret_encrypted,callback_url,provider_name) VALUES($1,'test','api','secret','https://merchant.test/callback','cloudpayments')`, h.merchant.ID)
	must(t, err)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		var body struct {
			InvoiceID string `json:"InvoiceId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if beforeReply != nil {
			beforeReply(body.InvoiceID)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/orders/create" {
			_, _ = io.WriteString(w, `{"Success":true,"Model":{"Id":"page","Url":"https://pay.test/page"}}`)
		} else if r.URL.Path == "/payments/qr/sbp/link" {
			_, _ = io.WriteString(w, `{"Success":true,"Model":{"TransactionId":42,"QrUrl":"https://qr.test/link"}}`)
		} else {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(api.Close)
	provider, err := cp.New(cp.Options{APIURL: api.URL, PublicID: "public", APISecret: "cp-secret", TestMode: true, Timeout: 5 * time.Second, Payments: h.payments})
	must(t, err)
	registry := bootstrap.NewProviderRegistry(provider)
	tx := pg.NewTransactionManager(pool, "")
	handler := webhookservice.NewService(registry, h.payments, pg.NewWebhookRepository(pool), pg.NewOutboxRepository(pool), tx)
	payments := paymentservice.NewService(h.payments, registry, func() time.Time { return time.Now().UTC() })
	h.router = httpapi.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), cpAuth{h.merchant}, payments, nil, handler, time.Minute)
	return h
}

func (h *harness) merchantRequest(t *testing.T, method, path string, body []byte, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", key)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("X-Timestamp", ts)
	req.Header.Set("X-Signature", crypto.HMACSHA256Hex("merchant-secret", append([]byte(ts+"."), body...)))
	if method == "POST" {
		req.Header.Set("Idempotency-Key", "stable-key")
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}
func createBody(method string) []byte {
	return []byte(fmt.Sprintf(`{"order_id":"order-42","amount_minor":1001,"currency":"RUB","payment_method":%q,"receipt":{"email":"buyer@example.test","taxation":"osn","items":[{"name":"Service","price_minor":1001,"quantity":1,"amount_minor":1001,"tax":"none"}]}}`, method))
}
func (h *harness) create(t *testing.T, method string) uuid.UUID {
	t.Helper()
	r := h.merchantRequest(t, "POST", "/api/v1/payments", createBody(method), "own")
	if r.Code != 201 {
		t.Fatalf("create=%d %s", r.Code, r.Body.String())
	}
	var result struct {
		PaymentID  uuid.UUID `json:"payment_id"`
		Status     string    `json:"status"`
		PaymentURL string    `json:"payment_url"`
	}
	must(t, json.Unmarshal(r.Body.Bytes(), &result))
	want := "https://pay.test/page"
	if method == "sbp" {
		want = "https://qr.test/link"
	}
	if result.Status != "pending" || result.PaymentURL != want {
		t.Fatalf("create=%s", r.Body.String())
	}
	p, err := h.payments.FindByID(t.Context(), result.PaymentID)
	must(t, err)
	if p.ProviderPaymentID != result.PaymentID.String() {
		t.Fatalf("provider ID=%s", p.ProviderPaymentID)
	}
	if h.count(t, "SELECT count(*) FROM outbox_events") != 0 {
		t.Fatal("creation emitted outbox")
	}
	return result.PaymentID
}
func (h *harness) count(t *testing.T, query string) int {
	t.Helper()
	var n int
	must(t, h.pool.QueryRow(t.Context(), query).Scan(&n))
	return n
}
func notification(id uuid.UUID, tx int64, fail bool) []byte {
	f := url.Values{"InvoiceId": {id.String()}, "TransactionId": {strconv.FormatInt(tx, 10)}, "Amount": {"10.01"}, "Currency": {"RUB"}, "TestMode": {"1"}, "OperationType": {"Payment"}, "Description": {"Оплата + &"}}
	if fail {
		f.Set("ReasonCode", "5001")
	} else {
		f.Set("Status", "Completed")
	}
	return []byte(f.Encode())
}
func signature(raw []byte) string {
	mac := hmac.New(sha256.New, []byte("cp-secret"))
	_, _ = mac.Write(raw)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
func (h *harness) webhook(raw []byte, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/v1/provider-webhooks/cloudpayments", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Content-HMAC", sig)
	r := httptest.NewRecorder()
	h.router.ServeHTTP(r, req)
	return r
}
func expectACK(t *testing.T, r *httptest.ResponseRecorder) {
	t.Helper()
	if r.Code != 200 || strings.TrimSpace(r.Body.String()) != `{"code":0}` {
		t.Fatalf("webhook=%d %s", r.Code, r.Body.String())
	}
}
func (h *harness) expectStatus(t *testing.T, id uuid.UUID, status paymentdomain.Status) {
	t.Helper()
	p, err := h.payments.FindByID(t.Context(), id)
	must(t, err)
	if p.Status != status {
		t.Fatalf("status=%s want=%s", p.Status, status)
	}
}

func TestFunctionalCloudPaymentsCreatePayAndConcurrentReplay(t *testing.T) {
	for _, method := range []string{"card", "sbp"} {
		t.Run(method, func(t *testing.T) {
			h := newHarness(t, nil)
			id := h.create(t, method)
			replay := h.merchantRequest(t, "POST", "/api/v1/payments", createBody(method), "own")
			if replay.Code != 200 || h.calls.Load() != 1 {
				t.Fatalf("replay=%d calls=%d", replay.Code, h.calls.Load())
			}
			var replayResult struct {
				PaymentID uuid.UUID `json:"payment_id"`
			}
			must(t, json.Unmarshal(replay.Body.Bytes(), &replayResult))
			if replayResult.PaymentID != id || h.count(t, "SELECT count(*) FROM payments") != 1 {
				t.Fatal("idempotent replay changed payment record")
			}
			raw := notification(id, 9007199254740993, false)
			expectACK(t, h.webhook(raw, signature(raw)))
			expectACK(t, h.webhook(raw, signature(raw)))
			var wg sync.WaitGroup
			start := make(chan struct{})
			responses := make([]*httptest.ResponseRecorder, 8)
			for i := range responses {
				wg.Go(func() { <-start; responses[i] = h.webhook(raw, signature(raw)) })
			}
			close(start)
			wg.Wait()
			for _, r := range responses {
				expectACK(t, r)
			}
			h.expectStatus(t, id, paymentdomain.StatusSucceeded)
			if h.count(t, "SELECT count(*) FROM webhook_events WHERE processed_at IS NOT NULL AND signature_valid") != 1 || h.count(t, "SELECT count(*) FROM outbox_events WHERE event_type='payment.succeeded'") != 1 {
				t.Fatal("duplicate webhook/outbox or unprocessed event")
			}
			var stored []byte
			must(t, h.pool.QueryRow(t.Context(), "SELECT raw_payload FROM webhook_events").Scan(&stored))
			var e struct {
				ContentType string `json:"content_type"`
				RawBody     []byte `json:"raw_body"`
			}
			must(t, json.Unmarshal(stored, &e))
			if !bytes.Equal(e.RawBody, raw) || e.ContentType != "application/x-www-form-urlencoded" {
				t.Fatal("JSONB lost raw form bytes")
			}
			own := h.merchantRequest(t, "GET", "/api/v1/payments/"+id.String(), nil, "own")
			if own.Code != 200 || !strings.Contains(own.Body.String(), `"status":"succeeded"`) {
				t.Fatalf("GET=%d %s", own.Code, own.Body.String())
			}
			foreign := h.merchantRequest(t, "GET", "/api/v1/payments/"+id.String(), nil, "other")
			if foreign.Code != 404 {
				t.Fatalf("foreign GET=%d", foreign.Code)
			}
		})
	}
}

func TestFunctionalCloudPaymentsFailThenPayAndLateFail(t *testing.T) {
	h := newHarness(t, nil)
	id := h.create(t, "card")
	raw := notification(id, 1, true)
	expectACK(t, h.webhook(raw, signature(raw)))
	h.expectStatus(t, id, paymentdomain.StatusPending)
	raw = notification(id, 2, false)
	expectACK(t, h.webhook(raw, signature(raw)))
	h.expectStatus(t, id, paymentdomain.StatusSucceeded)
	raw = notification(id, 3, true)
	expectACK(t, h.webhook(raw, signature(raw)))
	h.expectStatus(t, id, paymentdomain.StatusSucceeded)
	if h.count(t, "SELECT count(*) FROM webhook_events WHERE processed_at IS NOT NULL") != 3 || h.count(t, "SELECT count(*) FROM outbox_events WHERE event_type='payment.pending'") != 1 || h.count(t, "SELECT count(*) FROM outbox_events WHERE event_type='payment.succeeded'") != 1 || h.count(t, "SELECT count(*) FROM outbox_events") != 2 {
		t.Fatal("late Fail created rollback event or missing expected outbox")
	}
}

func TestFunctionalCloudPaymentsRejectionAndTransactionRollback(t *testing.T) {
	h := newHarness(t, nil)
	id := h.create(t, "card")
	raw := notification(id, 1, false)
	for _, tc := range []struct {
		raw []byte
		sig string
	}{{raw, "bad"}, {[]byte(strings.Replace(string(raw), "Amount=10.01", "Amount=10.02", 1)), ""}, {[]byte(strings.Replace(string(raw), "Currency=RUB", "Currency=USD", 1)), ""}, {[]byte(strings.Replace(string(raw), "TestMode=1", "TestMode=0", 1)), ""}} {
		sig := tc.sig
		if sig == "" {
			sig = signature(tc.raw)
		}
		r := h.webhook(tc.raw, sig)
		if r.Code == 200 || strings.Contains(r.Body.String(), `"code":0`) {
			t.Fatalf("invalid ACK=%d %s", r.Code, r.Body.String())
		}
		h.expectStatus(t, id, paymentdomain.StatusPending)
		if h.count(t, "SELECT count(*) FROM webhook_events") != 0 || h.count(t, "SELECT count(*) FROM outbox_events") != 0 {
			t.Fatal("invalid webhook wrote data")
		}
	}
	_, err := h.pool.Exec(t.Context(), `CREATE FUNCTION reject_cp_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test outbox failure'; END $$; CREATE TRIGGER reject_cp_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION reject_cp_outbox()`)
	must(t, err)
	r := h.webhook(raw, signature(raw))
	if r.Code != 503 || strings.Contains(r.Body.String(), `"code":0`) {
		t.Fatalf("failed commit ACK=%d %s", r.Code, r.Body.String())
	}
	h.expectStatus(t, id, paymentdomain.StatusPending)
	if h.count(t, "SELECT count(*) FROM webhook_events") != 0 || h.count(t, "SELECT count(*) FROM outbox_events") != 0 {
		t.Fatal("transaction was not rolled back")
	}
	_, err = h.pool.Exec(t.Context(), `DROP TRIGGER reject_cp_outbox ON outbox_events; DROP FUNCTION reject_cp_outbox()`)
	must(t, err)
	expectACK(t, h.webhook(raw, signature(raw)))
	h.expectStatus(t, id, paymentdomain.StatusSucceeded)
	if h.count(t, "SELECT count(*) FROM webhook_events") != 1 || h.count(t, "SELECT count(*) FROM outbox_events") != 1 {
		t.Fatal("replay after rollback did not apply once")
	}
}

func TestFunctionalCloudPaymentsPayBeforeCreationSaved(t *testing.T) {
	started := make(chan uuid.UUID, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	h := newHarness(t, func(id string) { started <- uuid.MustParse(id); <-release })
	t.Cleanup(unblock)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- h.merchantRequest(t, "POST", "/api/v1/payments", createBody("card"), "own") }()
	id := <-started
	raw := notification(id, 1, false)
	r := h.webhook(raw, signature(raw))
	if r.Code != 503 || strings.Contains(r.Body.String(), `"code":0`) {
		t.Fatalf("early ACK=%d %s", r.Code, r.Body.String())
	}
	h.expectStatus(t, id, paymentdomain.StatusCreating)
	if h.count(t, "SELECT count(*) FROM webhook_events") != 0 || h.count(t, "SELECT count(*) FROM outbox_events") != 0 {
		t.Fatal("early Pay changed data")
	}
	unblock()
	if r := <-done; r.Code != 201 {
		t.Fatalf("create=%d %s", r.Code, r.Body.String())
	}
	expectACK(t, h.webhook(raw, signature(raw)))
	h.expectStatus(t, id, paymentdomain.StatusSucceeded)
}
