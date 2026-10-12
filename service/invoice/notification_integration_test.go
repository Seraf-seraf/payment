//go:build functional

package invoice_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/Seraf-seraf/payment/adapter/httpapi"
	mock "github.com/Seraf-seraf/payment/adapter/provider/mockinvoice"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/ports"
	service "github.com/Seraf-seraf/payment/service/invoice"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type notificationRegistry struct {
	connection d.Connection
	decoder    ports.InvoiceNotificationDecoder
}

func (r notificationRegistry) DecoderForConnection(id string) (d.Connection, ports.InvoiceNotificationDecoder, bool) {
	return r.connection, r.decoder, id == r.connection.ID
}
func notificationEnvelope(body []byte) ports.InvoiceNotificationEnvelope {
	mac := hmac.New(sha256.New, []byte("invoice-test-secret"))
	mac.Write(body)
	return ports.InvoiceNotificationEnvelope{Body: body, ContentType: "application/json", Headers: map[string][]string{"X-Mock-Signature": {hex.EncodeToString(mac.Sum(nil))}}}
}
func notificationPayload(t *testing.T, o d.Observation, event, operation, channel string) []byte {
	t.Helper()
	body, err := json.Marshal(struct {
		EventID            string     `json:"event_id,omitempty"`
		Kind               string     `json:"type"`
		ExternalInvoiceID  string     `json:"external_invoice_id,omitempty"`
		Reference          string     `json:"reference,omitempty"`
		OperationID        string     `json:"operation_id,omitempty"`
		Channel            string     `json:"operation_channel,omitempty"`
		AmountMinor        int64      `json:"amount_minor,omitempty"`
		Currency           string     `json:"currency,omitempty"`
		RecipientAccount   string     `json:"recipient_account,omitempty"`
		PayerINN           string     `json:"payer_inn,omitempty"`
		FinalIncoming      bool       `json:"final_incoming,omitempty"`
		ProviderOccurredAt *time.Time `json:"provider_occurred_at,omitempty"`
	}{event, o.Kind, o.ExternalInvoiceID, o.Reference, operation, channel, o.AmountMinor, o.Currency, o.RecipientAccount, o.PayerINN, o.FinalIncoming, o.ProviderOccurredAt})
	must(t, err)
	return body
}
func (h *harness) notificationRouter(t *testing.T, strategy string, aliases map[string]string, tx ports.TransactionManager) (http.Handler, ports.InvoiceNotificationDecoder) {
	t.Helper()
	decoder, err := mock.NewNotificationDecoder("invoice-test-secret", h.record.EvidenceMode, strategy, aliases)
	must(t, err)
	if tx == nil {
		tx = h.tx
	}
	router := httpapi.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, time.Minute, httpapi.InvoiceWebhookDependencies{Notifications: service.NewNotificationService(tx, func() time.Time { return h.now }), Decoders: notificationRegistry{h.registry.connection, decoder}})
	return router, decoder
}
func postInvoiceNotification(t *testing.T, router http.Handler, body []byte, want int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/provider-invoice-webhooks/test-connection", strings.NewReader(string(body)))
	envelope := notificationEnvelope(body)
	req.Header.Set("Content-Type", envelope.ContentType)
	req.Header.Set("X-Mock-Signature", envelope.Headers["X-Mock-Signature"][0])
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Fatalf("webhook status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
	}
	if want == 200 && rec.Body.String() != `{"status":"ok"}` {
		t.Fatal("ACK is not the decoder acknowledgement")
	}
}
func (h *harness) count(t *testing.T, query string) int {
	t.Helper()
	var count int
	must(t, h.pool.QueryRow(t.Context(), query).Scan(&count))
	return count
}

func TestFunctionalInvoiceWebhookDurableAcceptanceAndFinancialTransportDedup(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	router, _ := h.notificationRouter(t, "shared_operation_id", nil, nil)
	o := h.credit("unused-core-key", 10000)
	body := notificationPayload(t, o, "event-1", "bank-operation-1", "webhook")
	postInvoiceNotification(t, router, body, 200)
	if h.count(t, "SELECT count(*) FROM invoice_inbox") != 1 || h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.apply_notification'") != 1 || h.load(t).CreditedMinor != 0 {
		t.Fatal("ACK preceded durable inbox/command, or HTTP applied money")
	}
	postInvoiceNotification(t, router, body, 200)
	if h.count(t, "SELECT count(*) FROM invoice_inbox") != 1 || h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.apply_notification'") != 1 {
		t.Fatal("exact transport replay created another inbox/command")
	}
	h.run(t)
	h.assertStatus(t, "succeeded")
	before := h.load(t).Revision
	callbacks := h.count(t, "SELECT count(*) FROM outbox_events")
	postInvoiceNotification(t, router, notificationPayload(t, o, "event-2", "bank-operation-1", "webhook"), 200)
	h.run(t)
	if h.load(t).Revision != before || h.count(t, "SELECT count(*) FROM outbox_events") != callbacks || h.count(t, "SELECT count(*) FROM invoice_evidence") != 1 {
		t.Fatal("different delivery ID counted the same financial operation twice")
	}
	changed := o
	changed.AmountMinor = 11000
	postInvoiceNotification(t, router, notificationPayload(t, changed, "event-2", "bank-operation-1", "webhook"), 200)
	h.run(t)
	if h.load(t).CreditedMinor != 10000 || h.count(t, "SELECT count(*) FROM invoice_inbox") != 3 {
		t.Fatal("changed content under existing event ID was swallowed or credited")
	}
	if h.count(t, "SELECT count(*) FROM invoice_inbox WHERE state='review'") != 1 {
		t.Fatal("changed operation did not create durable incident")
	}
}

func TestFunctionalInvoiceWebhookDifferentStatementIDsRequireCanonicalProof(t *testing.T) {
	for _, mapped := range []bool{true, false} {
		t.Run(map[bool]string{true: "proven aliases", false: "no proof"}[mapped], func(t *testing.T) {
			h := newHarness(t, "bank_credits", "recover_only")
			h.run(t)
			aliases := map[string]string{}
			if mapped {
				aliases = map[string]string{"webhook:webhook-op": "canonical-credit", "statement:statement-op": "canonical-credit"}
			}
			router, decoder := h.notificationRouter(t, "mapped_operation_id", aliases, nil)
			o := h.credit("unused-key", 4000)
			webhook := notificationPayload(t, o, "delivery", "webhook-op", "webhook")
			statement := notificationPayload(t, o, "", "statement-op", "statement")
			postInvoiceNotification(t, router, webhook, 200)
			h.run(t)
			before := h.load(t).Revision
			normalized, err := decoder.VerifyAndDecode(notificationEnvelope(statement))
			must(t, err)
			h.registry.provider.(*providerStub).read = func(ctx context.Context, v d.Record) ([]d.Observation, error) {
				return []d.Observation{normalized.Observation}, nil
			}
			h.now = h.now.Add(10 * time.Minute)
			h.run(t)
			h.run(t)
			if mapped {
				if h.load(t).CreditedMinor != 4000 || h.load(t).Revision != before || h.count(t, "SELECT count(*) FROM invoice_evidence") != 1 {
					t.Fatal("webhook/statement aliases did not share one financial fact")
				}
			} else {
				if h.load(t).CreditedMinor != 0 || h.count(t, "SELECT count(*) FROM invoice_evidence") != 0 || h.count(t, "SELECT count(*) FROM invoice_inbox WHERE state='review'") != 2 {
					t.Fatal("operation without proven cross-channel key was automatically credited or lost")
				}
			}
		})
	}
}

func TestFunctionalInvoiceWebhookStatusHintSchedulesReconcileOnlyAfterBinding(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	router, _ := h.notificationRouter(t, "shared_operation_id", nil, nil)
	_, err := h.pool.Exec(t.Context(), "UPDATE invoice_commands SET next_attempt_at=$1 WHERE command_type='invoice.issue'", h.now.Add(time.Hour))
	must(t, err)
	hint := d.Observation{Kind: "status_hint", ExternalInvoiceID: "hint-before-binding", Reference: h.record.Reference}
	postInvoiceNotification(t, router, notificationPayload(t, hint, "hint", "", ""), 200)
	h.run(t)
	if h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.reconcile'") != 0 || h.count(t, "SELECT count(*) FROM invoice_inbox WHERE state='unmatched'") != 1 {
		t.Fatal("hint initiated reconcile before unambiguous payment binding")
	}
	must(t, h.tx.WithinTx(t.Context(), func(ctx context.Context, r ports.Repositories) error {
		v, err := r.Invoices.Lock(ctx, h.record.PaymentID)
		if err != nil {
			return err
		}
		v.ExternalInvoiceID = hint.ExternalInvoiceID
		v.IssuanceState = "issued"
		dispatched := h.now
		v.DispatchedAt = &dispatched
		v.UpdatedAt = h.now
		return r.Invoices.Save(ctx, v)
	}))
	h.now = h.now.Add(time.Minute)
	h.run(t)
	if h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.reconcile' AND status='pending'") != 1 || h.registry.provider.(*providerStub).reads != 0 || h.load(t).CreditedMinor != 0 {
		t.Fatal("hint was read back in apply-command or increased funding")
	}
	h.run(t)
	if h.registry.provider.(*providerStub).reads != 1 || h.load(t).CreditedMinor != 0 {
		t.Fatal("reconcile did not process hint independently")
	}
}

type failApplyCreationTx struct{ base ports.TransactionManager }

func (f failApplyCreationTx) WithinTx(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return f.base.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		r.InvoiceCommands = failApplyCreationCommands{r.InvoiceCommands}
		return fn(ctx, r)
	})
}

type failApplyCreationCommands struct{ ports.InvoiceCommandRepository }

func (f failApplyCreationCommands) Create(ctx context.Context, c ports.InvoiceCommand) error {
	if c.CommandType == "invoice.apply_notification" {
		return errors.New("injected apply creation failure")
	}
	return f.InvoiceCommandRepository.Create(ctx, c)
}
func TestFunctionalInvoiceWebhookAcceptanceFailureRollsBackInboxBeforeACK(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	router, _ := h.notificationRouter(t, "shared_operation_id", nil, failApplyCreationTx{h.tx})
	postInvoiceNotification(t, router, notificationPayload(t, h.credit("unused", 10000), "delivery", "operation", ""), 503)
	if h.count(t, "SELECT count(*) FROM invoice_inbox") != 0 || h.count(t, "SELECT count(*) FROM invoice_commands WHERE command_type='invoice.apply_notification'") != 0 {
		t.Fatal("failed durable acceptance left partial inbox/command")
	}
}

func TestFunctionalInvoiceWebhookMismatchOverpaidAndLateFactsAreRetained(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	router, _ := h.notificationRouter(t, "shared_operation_id", nil, nil)
	badCurrency := h.credit("unused", 10000)
	badCurrency.Currency = "USD"
	postInvoiceNotification(t, router, notificationPayload(t, badCurrency, "currency", "op-currency", ""), 200)
	h.run(t)
	badPayer := h.credit("unused", 10000)
	badPayer.PayerINN = "7700000000"
	postInvoiceNotification(t, router, notificationPayload(t, badPayer, "payer", "op-payer", ""), 200)
	h.run(t)
	if h.load(t).CreditedMinor != 0 || h.count(t, "SELECT count(*) FROM invoice_evidence WHERE eligible=false") != 2 || h.count(t, "SELECT count(*) FROM invoice_inbox WHERE state='review'") != 2 {
		t.Fatal("mismatching financial facts disappeared or were counted")
	}
	h.now = h.record.Request.AcceptUntil
	_, err := h.pool.Exec(t.Context(), "UPDATE payments SET status='canceled' WHERE id=$1", h.record.PaymentID)
	must(t, err)
	postInvoiceNotification(t, router, notificationPayload(t, h.credit("unused", 12000), "late-overpay", "late-operation", ""), 200)
	h.run(t)
	v := h.load(t)
	h.assertStatus(t, "succeeded")
	if v.CreditedMinor != 12000 || v.FundingState != "overpaid" || len(v.ReviewReasons) < 4 {
		t.Fatalf("late overpayment lost or not reviewed=%+v", v)
	}
	before := v.Revision
	postInvoiceNotification(t, router, notificationPayload(t, h.credit("unused", 12000), "late-repeat", "late-operation", ""), 200)
	h.run(t)
	if h.load(t).Revision != before {
		t.Fatal("repeat overpayment generated second success")
	}
	var confirmed, review bool
	must(t, h.pool.QueryRow(t.Context(), "SELECT (payload->>'confirmed_paid')::boolean,(payload->>'requires_review')::boolean FROM outbox_events WHERE aggregate_id=$1 ORDER BY (payload->>'revision')::bigint DESC LIMIT 1", h.record.PaymentID).Scan(&confirmed, &review))
	if !confirmed || !review {
		t.Fatal("late overpayment callback omitted covered+review")
	}
}

func TestFunctionalInvoiceWebhookPaymentAfterFailureIsNotDiscarded(t *testing.T) {
	h := newHarness(t, "bank_credits", "recover_only")
	h.run(t)
	router, _ := h.notificationRouter(t, "shared_operation_id", nil, nil)
	_, err := h.pool.Exec(t.Context(), "UPDATE payments SET status='failed' WHERE id=$1", h.record.PaymentID)
	must(t, err)
	postInvoiceNotification(t, router, notificationPayload(t, h.credit("unused", 10000), "after-failure", "real-credit", ""), 200)
	h.run(t)
	h.assertStatus(t, "succeeded")
	v := h.load(t)
	if v.CreditedMinor != 10000 || len(v.ReviewReasons) == 0 || v.ConfirmationRecordedAt == nil {
		t.Fatal("financial confirmation after failed state was discarded or did not require review")
	}
}
