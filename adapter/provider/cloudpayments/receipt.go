package cloudpayments

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"

	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
)

var taxationSystemCodes = map[string]string{
	"osn": "0", "usn_income": "1", "usn_income_outcome": "2", "esn": "4", "patent": "5",
}

var taxCodes = map[string]any{
	"none":   nil,
	"vat0":   json.Number("0"),
	"vat5":   json.Number("5"),
	"vat7":   json.Number("7"),
	"vat10":  json.Number("10"),
	"vat22":  json.Number("22"),
	"vat105": json.Number("105"),
	"vat107": json.Number("107"),
	"vat110": json.Number("110"),
	"vat122": json.Number("122"),
}

var paymentMethodCodes = map[string]string{
	"full_prepayment": "1", "prepayment": "2", "advance": "3", "full_payment": "4",
	"partial_payment": "5", "credit": "6", "credit_payment": "7",
}

var paymentObjectCodes = map[string]string{
	"commodity": "1", "excise": "2", "job": "3", "service": "4", "gambling_bet": "5",
	"gambling_prize": "6", "lottery": "7", "lottery_prize": "8", "intellectual_activity": "9",
	"payment": "10", "agent_commission": "11", "composite": "12", "another": "13",
}

func customerReceipt(receipt providerdomain.Receipt, amountMinor int64) (map[string]any, error) {
	if len(receipt.Items) == 0 {
		return nil, nil
	}
	taxationSystem, ok := taxationSystemCodes[receipt.Taxation]
	if !ok {
		return nil, errors.New("unsupported receipt taxation")
	}
	items := make([]map[string]any, 0, len(receipt.Items))
	for _, item := range receipt.Items {
		if item.Name == "" || item.PriceMinor <= 0 || item.AmountMinor <= 0 || math.IsNaN(item.Quantity) || math.IsInf(item.Quantity, 0) || item.Quantity <= 0 {
			return nil, errors.New("invalid receipt item")
		}
		vat, ok := taxCodes[item.Tax]
		if !ok {
			return nil, errors.New("unsupported receipt tax")
		}
		mapped := map[string]any{
			"label":    item.Name,
			"price":    moneyNumber(item.PriceMinor),
			"quantity": strconv.FormatFloat(item.Quantity, 'f', -1, 64),
			"amount":   moneyNumber(item.AmountMinor),
			"vat":      vat,
		}
		if item.PaymentMethod != "" {
			code, exists := paymentMethodCodes[item.PaymentMethod]
			if !exists {
				return nil, errors.New("unsupported receipt payment method")
			}
			mapped["method"] = json.Number(code)
		}
		if item.PaymentObject != "" {
			code, exists := paymentObjectCodes[item.PaymentObject]
			if !exists {
				return nil, errors.New("unsupported receipt payment object")
			}
			mapped["object"] = json.Number(code)
		}
		items = append(items, mapped)
	}
	result := map[string]any{
		"taxationSystem": json.Number(taxationSystem),
		"items":          items,
		"amounts":        map[string]any{"electronic": moneyNumber(amountMinor)},
	}
	if receipt.Email != "" {
		result["email"] = receipt.Email
	}
	if receipt.Phone != "" {
		result["phone"] = receipt.Phone
	}
	return result, nil
}
