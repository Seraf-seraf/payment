package httpcallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
)

// Sender отправляет HTTP callback мерчанту.
type Sender struct {
	client *http.Client
	now    func() time.Time
}

var _ ports.CallbackSender = (*Sender)(nil)

// New создает HTTP callback sender с заданным timeout.
func New(timeout time.Duration) *Sender {
	return &Sender{
		client: &http.Client{Timeout: timeout},
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// NewWithClient создает HTTP callback sender с пользовательским HTTP client.
func NewWithClient(client *http.Client, now func() time.Time) *Sender {
	return &Sender{client: client, now: now}
}

// Send отправляет подписанное уведомление о платеже на callback_url мерчанта.
func (s *Sender) Send(ctx context.Context, merchant merchantdomain.Merchant, payment paymentdomain.Payment, event outboxdomain.Event) error {
	if merchant.CallbackURL == "" {
		return errors.New("merchant callback url is empty")
	}
	payload := map[string]any{
		"event_id":     event.ID,
		"event_type":   event.EventType,
		"payment_id":   payment.ID,
		"order_id":     payment.MerchantOrderID,
		"status":       payment.Status,
		"amount_minor": payment.AmountMinor,
		"currency":     payment.Currency,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	timestamp := fmt.Sprintf("%d", s.now().Unix())
	signature := crypto.HMACSHA256Hex(merchant.SharedSecret, []byte(timestamp+"."+string(body)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, merchant.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Signature", signature)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("callback returned status %d", resp.StatusCode)
	}
	return nil
}
