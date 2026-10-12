//go:build functional

package invoice_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	"github.com/Seraf-seraf/payment/adapter/notification/httpcallback"
	pg "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	invoiceservice "github.com/Seraf-seraf/payment/service/invoice"
	outboxservice "github.com/Seraf-seraf/payment/service/outbox"
	"github.com/google/uuid"
)

const callbackTestSecret = "callback-test-secret"
const callbackTestEncryptionKey = "callback-test-encryption-key"

func TestFunctionalInvoiceCallbackLostAckRetriesStoredSignedSnapshot(t *testing.T) {
	h := newHarnessWithoutInvoice(t, "bank_credits", "recover_only")
	merchant := merchantdomain.Merchant{ID: h.registry.connection.MerchantID, CallbackURL: "https://merchant.example/callback", SharedSecret: callbackTestSecret}
	api := httpapi.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), fixedInvoiceAPIAuthenticator{merchant: merchant}, nil,
		invoiceservice.NewService(pg.NewInvoiceRepository(h.pool), h.registry, h.tx, func() time.Time { return h.now }), nil, time.Minute)

	requestID := uuid.New()
	body := []byte(`{"request_id":"` + requestID.String() + `","order_id":"DV-CALLBACK","amount_minor":10000,"currency":"RUB","description":"Оплата заказа DV-CALLBACK","accept_until":"2026-10-13T06:00:00Z","payer":{"kind":"company","name":"ООО Пример","inn":"7701234567","kpp":"770101001","legal_address":"Тестовый адрес"},"lines":[{"id":"lot","name":"Неделимый лот","quantity":1,"unit":"set","unit_price_minor":10000,"amount_minor":10000,"vat":{"kind":"none","rate_basis_points":null,"amount_minor":0}}]}`)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/invoice-payments", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", "invoice-test-api-key")
	request.Header.Set("Idempotency-Key", requestID.String())
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	request.Header.Set("X-Timestamp", timestamp)
	request.Header.Set("X-Signature", crypto.HMACSHA256Hex(callbackTestSecret, []byte(timestamp+"."+string(body))))
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("invoice HTTP create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		PaymentID uuid.UUID `json:"payment_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.PaymentID == uuid.Nil {
		t.Fatal("invoice HTTP create omitted payment_id")
	}

	h.run(t)
	projection, err := invoiceservice.NewService(pg.NewInvoiceRepository(h.pool), h.registry, h.tx, func() time.Time { return h.now }).GetInvoicePayment(t.Context(), created.PaymentID, merchant)
	must(t, err)
	h.record = projection.Invoice
	webhook, _ := h.notificationRouter(t, "shared_operation_id", nil, nil)
	postInvoiceNotification(t, webhook, notificationPayload(t, h.credit("callback-credit", 10000), "callback-event", "callback-operation", "webhook"), http.StatusOK)
	h.run(t)
	paid := h.load(t)
	if paid.FundingState != "paid" || paid.CreditedMinor != paid.Request.AmountMinor {
		t.Fatalf("financial apply did not confirm payment: %+v", paid)
	}

	var eventID uuid.UUID
	var payload []byte
	var revision int64
	must(t, h.pool.QueryRow(t.Context(), `SELECT id,payload,(payload->>'revision')::bigint FROM outbox_events WHERE aggregate_id=$1 AND event_type='payment.invoice.updated' ORDER BY (payload->>'revision')::bigint DESC LIMIT 1`, paid.PaymentID).Scan(&eventID, &payload, &revision))
	var snapshot d.CallbackEnvelope
	must(t, json.Unmarshal(payload, &snapshot))
	if snapshot.Status != "succeeded" || snapshot.Revision != revision || snapshot.ConfirmedPaid != true {
		t.Fatalf("stored callback snapshot=%+v", snapshot)
	}
	_, err = h.pool.Exec(t.Context(), `UPDATE payments SET status='failed',updated_at=$2 WHERE id=$1`, paid.PaymentID, h.now.Add(time.Second))
	must(t, err)
	_, err = h.pool.Exec(t.Context(), `UPDATE outbox_events SET status='sent' WHERE aggregate_id=$1 AND id<>$2`, paid.PaymentID, eventID)
	must(t, err)

	type delivery struct {
		body      []byte
		eventID   string
		revision  int64
		timestamp string
		signature string
	}
	var mu sync.Mutex
	var deliveries []delivery
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read callback body: %v", readErr)
			return
		}
		signature := r.Header.Get("X-Signature")
		requestTimestamp := r.Header.Get("X-Timestamp")
		if want := crypto.HMACSHA256Hex(callbackTestSecret, []byte(requestTimestamp+"."+string(gotBody))); !hmac.Equal([]byte(signature), []byte(want)) {
			t.Errorf("callback signature does not match exact body")
		}
		var event d.CallbackEnvelope
		if err := json.Unmarshal(gotBody, &event); err != nil {
			t.Errorf("decode callback body: %v", err)
		}
		mu.Lock()
		deliveries = append(deliveries, delivery{body: gotBody, eventID: event.EventID.String(), revision: event.Revision, timestamp: requestTimestamp, signature: signature})
		count := len(deliveries)
		mu.Unlock()
		if count == 1 {
			// The receiver read and recorded the callback but drops the connection before ACK.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("callback server cannot simulate a lost ACK")
				return
			}
			connection, _, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Errorf("hijack callback response: %v", hijackErr)
				return
			}
			_ = connection.Close()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callbackServer.Close()
	worker := newTestOutboxWorker(t, h, callbackServer.URL, callbackServer.Client(), 3)
	if err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	var storedStatus string
	var attempts int
	var retryPayload []byte
	must(t, h.pool.QueryRow(t.Context(), `SELECT status,attempts,payload FROM outbox_events WHERE id=$1`, eventID).Scan(&storedStatus, &attempts, &retryPayload))
	if storedStatus != "pending" || attempts != 1 || !bytes.Equal(retryPayload, payload) {
		t.Fatalf("lost-ACK state status=%s attempts=%d payload_same=%v", storedStatus, attempts, bytes.Equal(retryPayload, payload))
	}
	var currentPaymentStatus string
	must(t, h.pool.QueryRow(t.Context(), `SELECT status FROM payments WHERE id=$1`, paid.PaymentID).Scan(&currentPaymentStatus))
	if currentPaymentStatus != "failed" {
		t.Fatalf("payment mutation was not persisted: %s", currentPaymentStatus)
	}

	h.now = h.now.Add(time.Second)
	if err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.pool.QueryRow(t.Context(), `SELECT status,attempts,payload FROM outbox_events WHERE id=$1`, eventID).Scan(&storedStatus, &attempts, &retryPayload); err != nil {
		t.Fatal(err)
	}
	if storedStatus != "sent" || attempts != 1 || !bytes.Equal(retryPayload, payload) {
		t.Fatalf("ACKed retry state status=%s attempts=%d payload_same=%v", storedStatus, attempts, bytes.Equal(retryPayload, payload))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 2 {
		t.Fatalf("callback deliveries=%d, want 2", len(deliveries))
	}
	for _, delivered := range deliveries {
		if !bytes.Equal(delivered.body, payload) || delivered.eventID != eventID.String() || delivered.revision != revision {
			t.Fatalf("retry changed immutable event: %+v", delivered)
		}
	}
	if deliveries[0].timestamp == deliveries[1].timestamp || deliveries[0].signature == deliveries[1].signature {
		t.Fatal("retry did not refresh callback timestamp/signature")
	}
}

func TestFunctionalInvoiceCallbackTerminalFailureRetainsOutboxRow(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	calls := 0
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer callbackServer.Close()
	worker := newTestOutboxWorker(t, h, callbackServer.URL, callbackServer.Client(), 1)
	if err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	var status, payload string
	var attempts int
	must(t, h.pool.QueryRow(t.Context(), `SELECT id,status,payload,attempts FROM outbox_events WHERE aggregate_id=$1 ORDER BY created_at,id LIMIT 1`, h.record.PaymentID).Scan(&id, &status, &payload, &attempts))
	if status != "failed" || attempts != 1 || payload == "" {
		t.Fatalf("terminal outbox row status=%s attempts=%d payload=%q", status, attempts, payload)
	}
	if err := worker.ProcessBatch(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	must(t, h.pool.QueryRow(t.Context(), `SELECT count(*) FROM outbox_events WHERE id=$1 AND status='failed'`, id).Scan(&count))
	if count != 1 || calls != 1 {
		t.Fatalf("terminal event deleted or retried: rows=%d callback_calls=%d", count, calls)
	}
}

type fixedInvoiceAPIAuthenticator struct{ merchant merchantdomain.Merchant }

func (a fixedInvoiceAPIAuthenticator) AuthenticateAPIKey(_ context.Context, key string) (merchantdomain.Merchant, error) {
	if key != "invoice-test-api-key" {
		return merchantdomain.Merchant{}, errors.New("unknown test API key")
	}
	return a.merchant, nil
}

func newTestOutboxWorker(t *testing.T, h *harness, callbackURL string, client *http.Client, maxAttempts int) *outboxservice.Worker {
	t.Helper()
	encryptedSecret, err := crypto.EncryptSecret(callbackTestEncryptionKey, callbackTestSecret)
	must(t, err)
	_, err = h.pool.Exec(t.Context(), `UPDATE merchants SET callback_url=$2,shared_secret_encrypted=$3 WHERE id=$1`, h.record.MerchantID, callbackURL, encryptedSecret)
	must(t, err)
	return outboxservice.NewWorker(slog.New(slog.NewTextHandler(io.Discard, nil)), outboxservice.Config{
		BatchSize: 20, MaxAttempts: maxAttempts,
	}, pg.NewMerchantRepository(h.pool, callbackTestEncryptionKey), pg.NewPaymentRepository(h.pool), pg.NewOutboxRepository(h.pool),
		httpcallback.NewWithClient(client, func() time.Time { return h.now }), h.tx, func() time.Time { return h.now })
}
