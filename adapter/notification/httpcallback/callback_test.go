package httpcallback

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/google/uuid"
)

func TestSendSignsCallback(t *testing.T) {
	t.Parallel()

	var gotSignature string
	var gotBody []byte
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotSignature = req.Header.Get("X-Signature")
		var err error
		gotBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	now := func() time.Time { return time.Unix(100, 0).UTC() }
	sender := NewWithClient(client, now)
	merchant := merchantdomain.Merchant{CallbackURL: "https://merchant.test/callback", SharedSecret: "secret"}
	payment := paymentdomain.Payment{ID: uuid.New(), MerchantOrderID: "order-1", Status: paymentdomain.StatusSucceeded, AmountMinor: 1000, Currency: "RUB"}

	if err := sender.Send(t.Context(), merchant, payment, outboxdomain.Event{ID: uuid.New(), EventType: "payment.succeeded"}); err != nil {
		t.Fatal(err)
	}
	expected := crypto.HMACSHA256Hex("secret", []byte("100."+string(gotBody)))
	if gotSignature != expected {
		t.Fatalf("signature = %q, want %q", gotSignature, expected)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
