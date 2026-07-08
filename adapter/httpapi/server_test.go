package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

func TestCreatePaymentRejectsInvalidSignature(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, fixedMerchantAuth{}, noopPayments{}, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", body())
	rec := httptest.NewRecorder()

	server.CreatePayment(rec, req, CreatePaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(time.Now().Unix(), 10),
		XSignature: "bad",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCreatePaymentRejectsOldTimestamp(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, fixedMerchantAuth{}, noopPayments{}, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", body())
	rec := httptest.NewRecorder()
	timestamp := time.Now().Add(-10 * time.Minute).Unix()
	signature := crypto.HMACSHA256Hex("secret", []byte("0."+bodyString))

	server.CreatePayment(rec, req, CreatePaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(timestamp, 10),
		XSignature: signature,
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestCreatePaymentAcceptsValidSignature(t *testing.T) {
	t.Parallel()

	payments := noopPayments{}
	server := NewServer(nil, fixedMerchantAuth{}, payments, nil, 5*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/payments", body())
	rec := httptest.NewRecorder()
	timestamp := time.Now().Unix()
	signature := crypto.HMACSHA256Hex("secret", []byte(formatMessage(timestamp)))

	server.CreatePayment(rec, req, CreatePaymentParams{
		XAPIKey:    "api-key",
		XTimestamp: strconv.FormatInt(timestamp, 10),
		XSignature: signature,
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

const bodyString = `{"order_id":"order-1","amount_minor":1000,"currency":"RUB"}`

func body() *strings.Reader {
	return strings.NewReader(bodyString)
}

func formatMessage(timestamp int64) string {
	return strconv.FormatInt(timestamp, 10) + "." + bodyString
}

type fixedMerchantAuth struct{}

func (fixedMerchantAuth) AuthenticateAPIKey(context.Context, string) (merchantdomain.Merchant, error) {
	return merchantdomain.Merchant{ID: uuid.New(), SharedSecret: "secret", ProviderName: "mock", IsActive: true}, nil
}

type noopPayments struct{}

func (noopPayments) CreatePayment(context.Context, ports.CreatePaymentRequest) (ports.CreatePaymentResult, error) {
	return ports.CreatePaymentResult{
		Created: true,
		Payment: paymentdomain.Payment{
			ID:              uuid.New(),
			MerchantOrderID: "order-1",
			AmountMinor:     1000,
			Currency:        "RUB",
			Status:          paymentdomain.StatusPending,
			PaymentURL:      "https://pay.test",
		},
	}, nil
}

func (noopPayments) GetPayment(context.Context, uuid.UUID) (paymentdomain.Payment, error) {
	return paymentdomain.Payment{}, nil
}
