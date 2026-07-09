package tbank

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/ports"
)

const (
	defaultAPIURL = "https://securepay.tinkoff.ru/v2"
)

// Options задает параметры подключения к T-Bank acquiring API.
type Options struct {
	APIURL          string
	TerminalKey     string
	Password        string
	NotificationURL string
	HTTPClient      *http.Client
}

// Provider реализует интеграцию с T-Bank acquiring API.
type Provider struct {
	apiURL          string
	terminalKey     string
	password        string
	notificationURL string
	httpClient      *http.Client
}

var _ ports.PaymentProvider = (*Provider)(nil)

// New создает T-Bank provider и валидирует обязательные параметры.
func New(options Options) (*Provider, error) {
	if options.TerminalKey == "" {
		return nil, errors.New("tbank terminal key is required")
	}
	if options.Password == "" {
		return nil, errors.New("tbank password is required")
	}
	apiURL := strings.TrimRight(options.APIURL, "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Provider{
		apiURL:          apiURL,
		terminalKey:     options.TerminalKey,
		password:        options.Password,
		notificationURL: options.NotificationURL,
		httpClient:      httpClient,
	}, nil
}

// Name возвращает имя провайдера.
func (p *Provider) Name() string {
	return "tbank"
}

// CreatePayment создает платеж через T-Bank Init API.
func (p *Provider) CreatePayment(ctx context.Context, req providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error) {
	payload := map[string]any{
		"TerminalKey": p.terminalKey,
		"Amount":      req.AmountMinor,
		"OrderId":     req.OrderID,
		"Description": trimDescription(req.Description),
	}
	if p.notificationURL != "" {
		payload["NotificationURL"] = p.notificationURL
	}
	if req.MerchantSuccess != "" {
		payload["SuccessURL"] = req.MerchantSuccess
	}
	if req.MerchantFail != "" {
		payload["FailURL"] = req.MerchantFail
	}
	if req.CustomerEmail != "" {
		payload["DATA"] = map[string]string{"Email": req.CustomerEmail}
	}
	if len(req.Receipt.Items) > 0 {
		payload["Receipt"] = tbankReceipt(req.Receipt)
	}
	payload["Token"] = makeToken(payload, p.password)

	body, err := json.Marshal(payload)
	if err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+"/Init", bytes.NewReader(body))
	if err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("tbank init returned status %d", httpResp.StatusCode)
	}

	var initResp initResponse
	if err := json.Unmarshal(respBody, &initResp); err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}
	if !initResp.Success {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("tbank init failed: code=%s message=%s details=%s", initResp.ErrorCode, initResp.Message, initResp.Details)
	}
	if initResp.PaymentID == "" || initResp.PaymentURL == "" {
		return providerdomain.CreatePaymentResult{}, errors.New("tbank init returned empty payment id or payment url")
	}

	return providerdomain.CreatePaymentResult{
		ProviderPaymentID: initResp.PaymentID,
		PaymentURL:        initResp.PaymentURL,
	}, nil
}

// VerifyWebhook проверяет token уведомления T-Bank.
func (p *Provider) VerifyWebhook(_ context.Context, _ http.Header, rawBody []byte) error {
	payload, err := parseNotification(rawBody)
	if err != nil {
		return err
	}
	token := getString(payload, "Token")
	if token == "" {
		return errors.New("tbank notification token is required")
	}
	if !strings.EqualFold(token, makeToken(payload, p.password)) {
		return errors.New("invalid tbank notification token")
	}
	return nil
}

// ParseWebhook преобразует уведомление T-Bank в нормализованное событие.
func (p *Provider) ParseWebhook(_ context.Context, rawBody []byte) (providerdomain.WebhookEvent, error) {
	payload, err := parseNotification(rawBody)
	if err != nil {
		return providerdomain.WebhookEvent{}, err
	}
	paymentID := getString(payload, "PaymentId")
	if paymentID == "" {
		paymentID = getString(payload, "PaymentID")
	}
	status := getString(payload, "Status")
	if paymentID == "" || status == "" {
		return providerdomain.WebhookEvent{}, errors.New("invalid tbank notification payload")
	}
	return providerdomain.WebhookEvent{
		ProviderEventID:   paymentID + ":" + status,
		ProviderPaymentID: paymentID,
		EventType:         "payment." + strings.ToLower(status),
		Status:            mapStatus(status),
	}, nil
}

type initResponse struct {
	Success    bool   `json:"Success"`
	ErrorCode  string `json:"ErrorCode"`
	Message    string `json:"Message"`
	Details    string `json:"Details"`
	PaymentID  string `json:"PaymentId"`
	PaymentURL string `json:"PaymentURL"`
}

type receipt struct {
	Email    string        `json:"Email,omitempty"`
	Phone    string        `json:"Phone,omitempty"`
	Taxation string        `json:"Taxation"`
	Items    []receiptItem `json:"Items"`
}

type receiptItem struct {
	Name          string  `json:"Name"`
	Price         int64   `json:"Price"`
	Quantity      float64 `json:"Quantity"`
	Amount        int64   `json:"Amount"`
	PaymentMethod string  `json:"PaymentMethod,omitempty"`
	PaymentObject string  `json:"PaymentObject,omitempty"`
	Tax           string  `json:"Tax"`
}

func tbankReceipt(value providerdomain.Receipt) receipt {
	items := make([]receiptItem, 0, len(value.Items))
	for _, item := range value.Items {
		items = append(items, receiptItem{
			Name:          trimReceiptItemName(item.Name),
			Price:         item.PriceMinor,
			Quantity:      item.Quantity,
			Amount:        item.AmountMinor,
			PaymentMethod: item.PaymentMethod,
			PaymentObject: item.PaymentObject,
			Tax:           item.Tax,
		})
	}

	return receipt{
		Email:    value.Email,
		Phone:    value.Phone,
		Taxation: value.Taxation,
		Items:    items,
	}
}

func makeToken(values map[string]any, password string) string {
	tokenValues := make(map[string]string, len(values)+1)
	for key, value := range values {
		if key == "Token" || isNested(value) {
			continue
		}
		tokenValues[key] = scalarString(value)
	}
	tokenValues["Password"] = password

	keys := make([]string, 0, len(tokenValues))
	for key := range tokenValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for _, key := range keys {
		builder.WriteString(tokenValues[key])
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

func parseNotification(rawBody []byte) (map[string]any, error) {
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err == nil {
		return values, nil
	}

	form, err := url.ParseQuery(string(rawBody))
	if err != nil {
		return nil, err
	}
	values = make(map[string]any, len(form))
	for key, value := range form {
		if len(value) > 0 {
			values[key] = value[0]
		}
	}
	if len(values) == 0 {
		return nil, errors.New("empty tbank notification payload")
	}
	return values, nil
}

func mapStatus(status string) paymentdomain.Status {
	switch strings.ToUpper(status) {
	case "CONFIRMED":
		return paymentdomain.StatusSucceeded
	case "AUTHORIZED":
		return paymentdomain.StatusPending
	case "REJECTED", "DEADLINE_EXPIRED":
		return paymentdomain.StatusFailed
	case "CANCELED":
		return paymentdomain.StatusCanceled
	case "REFUNDED", "PARTIAL_REFUNDED":
		return paymentdomain.StatusRefunded
	default:
		return paymentdomain.StatusPending
	}
}

func trimDescription(value string) string {
	if len([]rune(value)) <= 140 {
		return value
	}
	runes := []rune(value)
	return string(runes[:140])
}

func trimReceiptItemName(value string) string {
	if len([]rune(value)) <= 128 {
		return value
	}
	runes := []rune(value)
	return string(runes[:128])
}

func isNested(value any) bool {
	switch value.(type) {
	case map[string]any, map[string]string, []any, []map[string]any, receipt, receiptItem, []receiptItem:
		return true
	default:
		return false
	}
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

func getString(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	return scalarString(value)
}
