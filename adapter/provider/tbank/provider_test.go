package tbank

import (
	"testing"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
)

func TestMakeToken(t *testing.T) {
	t.Parallel()

	token := makeToken(map[string]any{
		"TerminalKey": "MerchantTerminalKey",
		"Amount":      19200,
		"OrderId":     "00000",
		"Description": "Подарочная карта на 1000 рублей",
		"DATA":        map[string]string{"Email": "a@test.com"},
	}, "11111111111111")

	if token != "72dd466f8ace0a37a1f740ce5fb78101712bc0665d91a8108c7c8a0ccd426db2" {
		t.Fatalf("token = %q", token)
	}
}

func TestParseWebhook(t *testing.T) {
	t.Parallel()

	provider, err := New(Options{
		TerminalKey: "terminal",
		Password:    "password",
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := map[string]any{
		"TerminalKey": "terminal",
		"OrderId":     "order-1",
		"Success":     true,
		"Status":      "CONFIRMED",
		"PaymentId":   "123456",
		"ErrorCode":   "0",
		"Amount":      1000,
	}
	token := makeToken(payload, "password")
	rawBody := []byte(`{"TerminalKey":"terminal","OrderId":"order-1","Success":true,"Status":"CONFIRMED","PaymentId":"123456","ErrorCode":"0","Amount":1000,"Token":"` + token + `"}`)

	if err := provider.VerifyWebhook(t.Context(), nil, rawBody); err != nil {
		t.Fatal(err)
	}

	event, err := provider.ParseWebhook(t.Context(), rawBody)
	if err != nil {
		t.Fatal(err)
	}
	if event.ProviderEventID != "123456:CONFIRMED" {
		t.Fatalf("ProviderEventID = %q", event.ProviderEventID)
	}
	if event.ProviderPaymentID != "123456" {
		t.Fatalf("ProviderPaymentID = %q", event.ProviderPaymentID)
	}
	if event.Status != paymentdomain.StatusSucceeded {
		t.Fatalf("Status = %q", event.Status)
	}
}
