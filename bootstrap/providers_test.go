package bootstrap

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	merchant "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
	"reflect"
	"testing"
	"time"
)

type invoiceMerchants struct {
	ports.MerchantRepository
	id  uuid.UUID
	err error
}

func (r invoiceMerchants) FindByID(_ context.Context, id uuid.UUID) (merchant.Merchant, error) {
	if r.err != nil {
		return merchant.Merchant{}, r.err
	}
	if id != r.id {
		return merchant.Merchant{}, errors.New("unknown merchant")
	}
	return merchant.Merchant{ID: id}, nil
}
func invoiceConfig(t *testing.T) (config.Config, invoiceMerchants) {
	t.Helper()
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAYMENT_TEST_INVOICE_WEBHOOK_SECRET", "invoice-secret")
	id := uuid.New()
	cfg.App.Env = "test"
	cfg.Invoices.Enabled = true
	cfg.Invoices.Connections = []config.InvoiceConnection{{ID: uuid.NewString(), MerchantID: id.String(), ProviderKind: "mock_invoice", EvidenceMode: "bank_credits", CreationReplay: "provider_idempotent", WebhookSecretEnv: "PAYMENT_TEST_INVOICE_WEBHOOK_SECRET", OperationIDStrategy: "shared_operation_id", Beneficiary: config.InvoiceBeneficiary{Name: "Local mock beneficiary", INN: "7707654321", KPP: "770701001", Account: "00000000000000000000", BankBIK: "000000000", BankName: "Local mock bank", CorrespondentAccount: "00000000000000000000"}}}
	return cfg, invoiceMerchants{id: id}
}
func TestBuildInvoiceProviderRegistryBindsLocalMockToMerchant(t *testing.T) {
	cfg, merchants := invoiceConfig(t)
	for _, env := range []string{"local", "test"} {
		cfg.App.Env = env
		registry, err := buildInvoiceProviderRegistry(t.Context(), cfg, merchants)
		if err != nil {
			t.Fatal(err)
		}
		connection, ok := registry.ForMerchant(merchants.id)
		if !ok || connection.ID != cfg.Invoices.Connections[0].ID || connection.Beneficiary.Account != cfg.Invoices.Connections[0].Beneficiary.Account || connection.Capabilities.CreationReplay != "provider_idempotent" || connection.Capabilities.MaxAcceptDuration != 120*time.Hour {
			t.Fatalf("unexpected connection %+v", connection)
		}
		if _, ok := registry.ForMerchant(uuid.New()); ok {
			t.Fatal("wrong merchant matched")
		}
	}
}
func TestBuildInvoiceProviderRegistryRejectsInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*config.Config, *invoiceMerchants)
	}{
		{"unknown env", func(c *config.Config, _ *invoiceMerchants) { c.App.Env = "staging" }},
		{"empty env", func(c *config.Config, _ *invoiceMerchants) { c.App.Env = "" }},
		{"production mock", func(c *config.Config, _ *invoiceMerchants) { c.App.Env = "production" }},
		{"unknown provider", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].ProviderKind = "bank" }},
		{"unknown merchant", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].MerchantID = uuid.NewString() }},
		{"merchant lookup failure", func(_ *config.Config, r *invoiceMerchants) { r.err = errors.New("database unavailable") }},
		{"invalid connection UUID", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].ID = "local-invoice" }},
		{"nil merchant UUID", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].MerchantID = uuid.Nil.String() }},
		{"invalid evidence mode", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].EvidenceMode = "paid" }},
		{"mock nonidempotent override", func(c *config.Config, _ *invoiceMerchants) {
			c.Invoices.Connections[0].CreationReplay = "non_idempotent"
		}},
		{"duplicate connection", func(c *config.Config, _ *invoiceMerchants) {
			other := c.Invoices.Connections[0]
			other.MerchantID = uuid.NewString()
			c.Invoices.Connections = append(c.Invoices.Connections, other)
		}},
		{"duplicate merchant", func(c *config.Config, _ *invoiceMerchants) {
			other := c.Invoices.Connections[0]
			other.ID = uuid.NewString()
			c.Invoices.Connections = append(c.Invoices.Connections, other)
		}},
		{"missing invoice secret no acquiring fallback", func(c *config.Config, _ *invoiceMerchants) {
			c.Providers.Mock.WebhookSecret = "acquiring-secret"
			c.Invoices.Connections[0].WebhookSecretEnv = "PAYMENT_TEST_UNSET_INVOICE_SECRET"
		}},
		{"no secret env name", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].WebhookSecretEnv = "" }},
		{"invalid beneficiary", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections[0].Beneficiary.Account = "123" }},
		{"lease equals timeout", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.CommandLease = c.Invoices.ProviderTimeout }},
		{"lease below timeout", func(c *config.Config, _ *invoiceMerchants) {
			c.Invoices.CommandLease = c.Invoices.ProviderTimeout - time.Nanosecond
		}},
		{"no connections", func(c *config.Config, _ *invoiceMerchants) { c.Invoices.Connections = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r := invoiceConfig(t)
			t.Setenv("PAYMENT_TEST_UNSET_INVOICE_SECRET", "")
			tc.change(&cfg, &r)
			if _, err := buildInvoiceProviderRegistry(t.Context(), cfg, r); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
func TestInvoiceRegistryDisabledDoesNotReadSecretOrMerchant(t *testing.T) {
	cfg, _ := invoiceConfig(t)
	cfg.Invoices.Enabled = false
	cfg.Invoices.Connections[0].WebhookSecretEnv = ""
	registry, err := buildInvoiceProviderRegistry(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.ForMerchant(uuid.MustParse(cfg.Invoices.Connections[0].MerchantID)); ok {
		t.Fatal("disabled invoices registered")
	}
}

func TestInvoiceSecretRotationKeepsConnectionAndSafeSnapshot(t *testing.T) {
	cfg, r := invoiceConfig(t)
	first, err := buildInvoiceProviderRegistry(t.Context(), cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(cfg.Invoices.Connections[0].WebhookSecretEnv, "rotated-invoice-secret")
	second, err := buildInvoiceProviderRegistry(t.Context(), cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := first.ForMerchant(r.id)
	b, _ := second.ForMerchant(r.id)
	if !reflect.DeepEqual(a.Snapshot(), b.Snapshot()) {
		t.Fatal("credentials changed connection snapshot")
	}
	body := []byte(`{"type":"status_hint","external_invoice_id":"invoice"}`)
	_, decoder, _ := second.DecoderForConnection(a.ID)
	mac := hmac.New(sha256.New, []byte("rotated-invoice-secret"))
	_, _ = mac.Write(body)
	envelope := ports.InvoiceNotificationEnvelope{Body: body, ContentType: "application/json", Headers: map[string][]string{"X-Mock-Signature": {hex.EncodeToString(mac.Sum(nil))}}}
	if _, err := decoder.VerifyAndDecode(envelope); err != nil {
		t.Fatal(err)
	}
	_, oldDecoder, _ := first.DecoderForConnection(a.ID)
	if _, err := oldDecoder.VerifyAndDecode(envelope); err == nil {
		t.Fatal("old decoder accepted rotated secret")
	}
}
