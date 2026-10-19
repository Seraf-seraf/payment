package cloudpayments

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"testing"

	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
)

func TestCloudPaymentsReceiptMapsEnumsAndPreservesValues(t *testing.T) {
	var payload map[string]any
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload = decodeJSONMap(t, r.Body)
		requireCardResponse(w)
	}))
	req := baseCreateRequest()
	req.CustomerEmail = ""
	req.Receipt = providerdomain.Receipt{
		Email: "receipt@example.test", Phone: "+7 900 000-00-00", Taxation: "osn",
		Items: []providerdomain.ReceiptItem{
			{Name: "Первая позиция", PriceMinor: 1200, Quantity: 1, AmountMinor: 1111, PaymentMethod: "full_prepayment", PaymentObject: "commodity", Tax: "none"},
			{Name: "Вторая", PriceMinor: 250, Quantity: 2.5, AmountMinor: 601, PaymentMethod: "prepayment", PaymentObject: "excise", Tax: "vat0"},
			{Name: "Третья", PriceMinor: 999, Quantity: 0.001, AmountMinor: 17, PaymentMethod: "advance", PaymentObject: "job", Tax: "vat5"},
			{Name: "Четвёртая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "service", Tax: "vat7"},
			{Name: "Пятая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "partial_payment", PaymentObject: "gambling_bet", Tax: "vat10"},
			{Name: "Шестая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "credit", PaymentObject: "gambling_prize", Tax: "vat22"},
			{Name: "Седьмая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "credit_payment", PaymentObject: "lottery", Tax: "vat105"},
			{Name: "Восьмая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "lottery_prize", Tax: "vat107"},
			{Name: "Девятая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "intellectual_activity", Tax: "vat110"},
			{Name: "Десятая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "payment", Tax: "vat122"},
			{Name: "Одиннадцатая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "agent_commission", Tax: "none"},
			{Name: "Двенадцатая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "composite", Tax: "vat0"},
			{Name: "Тринадцатая", PriceMinor: 100, Quantity: 1, AmountMinor: 100, PaymentMethod: "full_payment", PaymentObject: "another", Tax: "vat5"},
		},
	}
	if _, err := provider.CreatePayment(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	jsonData, ok := payload["JsonData"].(map[string]any)
	if !ok {
		t.Fatalf("JsonData = %#v", payload["JsonData"])
	}
	cloudpayments, ok := jsonData["cloudpayments"].(map[string]any)
	if !ok {
		t.Fatalf("JsonData.cloudpayments = %#v", jsonData["cloudpayments"])
	}
	receipt, ok := cloudpayments["CustomerReceipt"].(map[string]any)
	if !ok {
		t.Fatalf("CustomerReceipt = %#v", cloudpayments["CustomerReceipt"])
	}
	if receipt["email"] != "receipt@example.test" || receipt["phone"] != "+7 900 000-00-00" {
		t.Errorf("receipt contacts = %#v", receipt)
	}
	if receipt["taxationSystem"] != json.Number("0") {
		t.Errorf("taxationSystem = %#v, want zero", receipt["taxationSystem"])
	}
	amounts, ok := receipt["amounts"].(map[string]any)
	if !ok || amounts["electronic"] != json.Number("10.01") {
		t.Errorf("amounts = %#v", receipt["amounts"])
	}
	items, ok := receipt["items"].([]any)
	if !ok || len(items) != len(req.Receipt.Items) {
		t.Fatalf("items = %#v", receipt["items"])
	}
	methods := []string{"1", "2", "3", "4", "5", "6", "7", "4", "4", "4", "4", "4", "4"}
	objects := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13"}
	vats := []any{nil, json.Number("0"), json.Number("5"), json.Number("7"), json.Number("10"), json.Number("22"), json.Number("105"), json.Number("107"), json.Number("110"), json.Number("122"), nil, json.Number("0"), json.Number("5")}
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("items[%d] = %#v", i, raw)
		}
		want := req.Receipt.Items[i]
		if item["label"] != want.Name || item["price"] != json.Number(strconv.FormatFloat(float64(want.PriceMinor)/100, 'f', 2, 64)) || item["amount"] != json.Number(strconv.FormatFloat(float64(want.AmountMinor)/100, 'f', 2, 64)) {
			t.Errorf("items[%d] values = %#v", i, item)
		}
		quantity := strconv.FormatFloat(want.Quantity, 'f', -1, 64)
		if item["quantity"] != quantity || item["method"] != json.Number(methods[i]) || item["object"] != json.Number(objects[i]) {
			t.Errorf("items[%d] quantity/method/object = %#v", i, item)
		}
		if item["vat"] != vats[i] {
			t.Errorf("items[%d].vat = %#v, want %#v", i, item["vat"], vats[i])
		}
	}
}

func TestCloudPaymentsReceiptSupportsAllTaxationAndPhoneOnly(t *testing.T) {
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := decodeJSONMap(t, r.Body)
		if _, exists := payload["Email"]; exists {
			t.Error("missing customer email must not be invented")
		}
		jsonData := payload["JsonData"].(map[string]any)
		cloudpayments := jsonData["cloudpayments"].(map[string]any)
		receipt := cloudpayments["CustomerReceipt"].(map[string]any)
		if _, exists := receipt["email"]; exists {
			t.Error("empty receipt email must be omitted")
		}
		if receipt["phone"] != "+7 900 000-00-00" {
			t.Errorf("receipt phone = %#v", receipt["phone"])
		}
		if receipt["taxationSystem"] != json.Number("5") {
			t.Errorf("taxationSystem = %#v, want 5", receipt["taxationSystem"])
		}
		items := receipt["items"].([]any)
		item := items[0].(map[string]any)
		if _, exists := item["method"]; exists {
			t.Error("empty method must not be invented")
		}
		if _, exists := item["object"]; exists {
			t.Error("empty object must not be invented")
		}
		requireCardResponse(w)
	}))
	req := baseCreateRequest()
	req.CustomerEmail = ""
	req.Receipt = providerdomain.Receipt{
		Phone: "+7 900 000-00-00", Taxation: "patent",
		Items: []providerdomain.ReceiptItem{{Name: "Phone only", PriceMinor: 100, Quantity: 1, AmountMinor: 100, Tax: "none"}},
	}
	if _, err := provider.CreatePayment(t.Context(), req); err != nil {
		t.Fatal(err)
	}

	taxationCodes := map[string]string{"osn": "0", "usn_income": "1", "usn_income_outcome": "2", "esn": "4", "patent": "5"}
	for taxation, want := range taxationCodes {
		req.Receipt.Taxation = taxation
		var actual json.Number
		provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload := decodeJSONMap(t, r.Body)
			jsonData := payload["JsonData"].(map[string]any)
			cloudpayments := jsonData["cloudpayments"].(map[string]any)
			receipt := cloudpayments["CustomerReceipt"].(map[string]any)
			actual = receipt["taxationSystem"].(json.Number)
			requireCardResponse(w)
		}))
		if _, err := provider.CreatePayment(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		if string(actual) != want {
			t.Errorf("taxation %q encoded as %s, want %s", taxation, actual, want)
		}
	}
}

func TestCloudPaymentsReceiptRejectsUnknownEnumsAndInvalidQuantityBeforeHTTP(t *testing.T) {
	calls := 0
	provider, _ := newTestProvider(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		requireCardResponse(w)
	}))
	for _, tc := range []struct {
		name   string
		change func(*providerdomain.Receipt)
	}{
		{"taxation", func(r *providerdomain.Receipt) { r.Taxation = "unknown" }},
		{"tax", func(r *providerdomain.Receipt) { r.Items[0].Tax = "unknown" }},
		{"payment method", func(r *providerdomain.Receipt) { r.Items[0].PaymentMethod = "unknown" }},
		{"payment object", func(r *providerdomain.Receipt) { r.Items[0].PaymentObject = "unknown" }},
		{"zero quantity", func(r *providerdomain.Receipt) { r.Items[0].Quantity = 0 }},
		{"negative quantity", func(r *providerdomain.Receipt) { r.Items[0].Quantity = -1 }},
		{"NaN quantity", func(r *providerdomain.Receipt) { r.Items[0].Quantity = math.NaN() }},
		{"infinite quantity", func(r *providerdomain.Receipt) { r.Items[0].Quantity = math.Inf(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseCreateRequest()
			req.Receipt = testReceipt()
			tc.change(&req.Receipt)
			if _, err := provider.CreatePayment(t.Context(), req); err == nil {
				t.Fatal("CreatePayment returned no error")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid receipt requests made %d API calls", calls)
	}
}
