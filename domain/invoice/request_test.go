package invoice

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func validRuleRequest() Request {
	return Request{RequestID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), OrderID: "DV-1042", AmountMinor: 100, Currency: "RUB", Description: "Оплата лота", AcceptUntil: time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC), Payer: Payer{Kind: "company", Name: "ООО Пример", INN: "7701234567", KPP: "770101001", LegalAddress: "Тестовый адрес"}, Lines: []Line{{ID: "lot", Name: "Набор из четырёх предметов", Quantity: 1, Unit: "set", UnitPriceMinor: 100, AmountMinor: 100, VAT: VAT{Kind: "none"}}}}
}

func TestRequestMoneyAndQuantityRules(t *testing.T) {
	cases := []struct {
		name                         string
		quantity, price, line, total int64
		valid                        bool
	}{
		{"indivisible_lot", 1, 100, 100, 100, true},
		{"zero_quantity", 0, 100, 0, 100, false},
		{"maximum_quantity_minus_one", 999999, 1, 999999, 999999, true},
		{"maximum_quantity", 1000000, 1, 1000000, 1000000, true},
		{"above_maximum_quantity", 1000001, 1, 1000001, 1000001, false},
		{"zero_price", 1, 0, 0, 0, false},
		{"line_product_mismatch", 2, 100, 100, 100, false},
		{"request_sum_mismatch", 1, 100, 100, 101, false},
		{"money_limit_minus_one", 1, MaxAmountMinor - 1, MaxAmountMinor - 1, MaxAmountMinor - 1, true},
		{"money_limit", 1, MaxAmountMinor, MaxAmountMinor, MaxAmountMinor, true},
		{"money_limit_plus_one", 1, MaxAmountMinor + 1, MaxAmountMinor + 1, MaxAmountMinor + 1, false},
		{"product_int64_overflow", 1000000, MaxAmountMinor, 1, 1, false},
		{"negative_price", 1, -1, -1, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRuleRequest()
			r.Lines[0].Quantity = tc.quantity
			r.Lines[0].UnitPriceMinor = tc.price
			r.Lines[0].AmountMinor = tc.line
			r.AmountMinor = tc.total
			if err := ValidateRequest(r); (err == nil) != tc.valid {
				t.Fatalf("ValidateRequest error=%v, valid=%v", err, tc.valid)
			}
		})
	}
}

func TestRequestLineCountAndSumOverflow(t *testing.T) {
	for _, count := range []int{0, 1, 99, 100, 101} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			r := validRuleRequest()
			r.Lines = nil
			for i := 0; i < count; i++ {
				line := validRuleRequest().Lines[0]
				line.ID = fmt.Sprint(i)
				r.Lines = append(r.Lines, line)
			}
			r.AmountMinor = int64(count) * 100
			err := ValidateRequest(r)
			if (err == nil) != (count >= 1 && count <= 100) {
				t.Fatalf("count=%d error=%v", count, err)
			}
		})
	}
	t.Run("sum_exceeds_json_integer_limit", func(t *testing.T) {
		r := validRuleRequest()
		r.Lines[0].UnitPriceMinor = MaxAmountMinor
		r.Lines[0].AmountMinor = MaxAmountMinor
		extra := validRuleRequest().Lines[0]
		extra.ID = "extra"
		r.Lines = append(r.Lines, extra)
		r.AmountMinor = MaxAmountMinor
		if ValidateRequest(r) == nil {
			t.Fatal("sum above limit accepted")
		}
	})
}

func TestRequestVATNoneAndZeroRateAreDistinct(t *testing.T) {
	zero := int32(0)
	above := int32(10001)
	cases := []struct {
		name  string
		vat   VAT
		valid bool
	}{
		{"none", VAT{Kind: "none"}, true}, {"included_zero", VAT{Kind: "included", RateBasisPoints: &zero}, true},
		{"none_with_zero_rate", VAT{Kind: "none", RateBasisPoints: &zero}, false}, {"none_with_amount", VAT{Kind: "none", AmountMinor: 1}, false},
		{"included_without_rate", VAT{Kind: "included"}, false}, {"rate_above_limit", VAT{Kind: "included", RateBasisPoints: &above}, false},
		{"vat_above_line", VAT{Kind: "included", RateBasisPoints: &zero, AmountMinor: 101}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRuleRequest()
			r.Lines[0].VAT = tc.vat
			if err := ValidateRequest(r); (err == nil) != tc.valid {
				t.Fatalf("error=%v valid=%v", err, tc.valid)
			}
		})
	}
}

func TestNewInvoiceDeadlineBoundary(t *testing.T) {
	now := validRuleRequest().AcceptUntil
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(delta.String(), func(t *testing.T) {
			err := ValidateNewDeadline(now.Add(delta), now)
			if (err == nil) != (delta > 0) {
				t.Fatalf("delta=%v error=%v", delta, err)
			}
		})
	}
}
