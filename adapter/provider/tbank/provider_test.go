package tbank

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
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

func TestCreatePaymentSendsReceipt(t *testing.T) {
	t.Parallel()

	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Success":true,"PaymentId":"123456","PaymentURL":"https://pay.test/123456"}`))
	}))
	defer server.Close()

	provider, err := New(Options{
		APIURL:      server.URL,
		TerminalKey: "terminal",
		Password:    "password",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = provider.CreatePayment(t.Context(), providerdomain.CreatePaymentRequest{
		PaymentID:     "payment-id",
		OrderID:       "order-1",
		AmountMinor:   1500,
		Description:   "Test service",
		CustomerEmail: "customer@example.com",
		Receipt: providerdomain.Receipt{
			Email:    "customer@example.com",
			Taxation: "osn",
			Items: []providerdomain.ReceiptItem{
				{
					Name:          "Test service",
					PriceMinor:    1500,
					Quantity:      1,
					AmountMinor:   1500,
					PaymentMethod: "full_payment",
					PaymentObject: "service",
					Tax:           "none",
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := <-requests
	receipt, ok := payload["Receipt"].(map[string]any)
	if !ok {
		t.Fatalf("Receipt = %#v", payload["Receipt"])
	}
	if receipt["Email"] != "customer@example.com" {
		t.Fatalf("Receipt.Email = %#v", receipt["Email"])
	}
	if receipt["Taxation"] != "osn" {
		t.Fatalf("Receipt.Taxation = %#v", receipt["Taxation"])
	}

	items, ok := receipt["Items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("Receipt.Items = %#v", receipt["Items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("Receipt.Items[0] = %#v", items[0])
	}
	if item["Name"] != "Test service" {
		t.Fatalf("Receipt.Items[0].Name = %#v", item["Name"])
	}
	if item["Amount"] != float64(1500) || item["Price"] != float64(1500) || item["Quantity"] != float64(1) {
		t.Fatalf("Receipt.Items[0] amounts = %#v", item)
	}
	if item["PaymentMethod"] != "full_payment" || item["PaymentObject"] != "service" || item["Tax"] != "none" {
		t.Fatalf("Receipt.Items[0] fiscal fields = %#v", item)
	}

	expectedToken := makeToken(map[string]any{
		"TerminalKey": "terminal",
		"Amount":      1500,
		"OrderId":     "order-1",
		"Description": "Test service",
		"DATA":        map[string]string{"Email": "customer@example.com"},
	}, "password")
	if payload["Token"] != expectedToken {
		t.Fatalf("Token = %#v, want %q", payload["Token"], expectedToken)
	}
}
