package cloudpayments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	providerdomain "github.com/Seraf-seraf/payment/domain/provider"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
)

const defaultAPIURL = "https://api.cloudpayments.ru"

const maxAPIResponseBytes = 1 << 20

var (
	ErrInvalidRequest      = errors.New("invalid cloudpayments payment request")
	ErrInvalidResponse     = errors.New("invalid cloudpayments API response")
	ErrInvalidSignature    = errors.New("invalid cloudpayments webhook signature")
	ErrInvalidNotification = errors.New("invalid cloudpayments webhook notification")
	ErrPaymentMismatch     = errors.New("cloudpayments webhook payment mismatch")
	ErrPaymentNotReady     = errors.New("cloudpayments payment is not ready for webhook")
)

// Options задаёт параметры адаптера CloudPayments.
type Options struct {
	APIURL     string
	PublicID   string
	APISecret  string
	TestMode   bool
	Timeout    time.Duration
	HTTPClient *http.Client
	Payments   ports.PaymentRepository
}

// Provider реализует адаптер платёжного провайдера CloudPayments.
type Provider struct {
	apiURL    *url.URL
	publicID  string
	apiSecret string
	testMode  bool
	timeout   time.Duration
	client    *http.Client
	payments  ports.PaymentRepository
}

var _ ports.PaymentProvider = (*Provider)(nil)

// New создаёт адаптер CloudPayments.
func New(options Options) (*Provider, error) {
	if options.APIURL == "" {
		options.APIURL = defaultAPIURL
	}
	apiURL, err := url.Parse(options.APIURL)
	if err != nil || apiURL == nil || (apiURL.Scheme != "http" && apiURL.Scheme != "https") || apiURL.Host == "" || apiURL.User != nil || apiURL.RawQuery != "" || apiURL.ForceQuery || apiURL.Fragment != "" || strings.Contains(options.APIURL, "#") {
		return nil, errors.New("invalid cloudpayments API URL")
	}
	if options.PublicID == "" || options.APISecret == "" || options.Timeout <= 0 || options.Payments == nil {
		return nil, errors.New("invalid cloudpayments options")
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: options.Timeout}
	}
	return &Provider{apiURL: apiURL, publicID: options.PublicID, apiSecret: options.APISecret, testMode: options.TestMode, timeout: options.Timeout, client: client, payments: options.Payments}, nil
}

func (p *Provider) Name() string { return "cloudpayments" }

func (p *Provider) CreatePayment(ctx context.Context, req providerdomain.CreatePaymentRequest) (providerdomain.CreatePaymentResult, error) {
	method, err := normalizePaymentMethod(req.PaymentMethod)
	if err != nil || req.Currency != "RUB" || req.AmountMinor <= 0 {
		return providerdomain.CreatePaymentResult{}, ErrInvalidRequest
	}
	paymentID, err := parsePaymentID(req.PaymentID)
	if err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}
	description := req.Description
	if description == "" {
		description = "Оплата заказа " + req.OrderID
	}
	payload := map[string]any{
		"Amount":      moneyNumber(req.AmountMinor),
		"Currency":    req.Currency,
		"Description": description,
		"InvoiceId":   paymentID,
	}
	if email := paymentEmail(req); email != "" {
		payload["Email"] = email
	}
	receipt, err := customerReceipt(req.Receipt, req.AmountMinor)
	if err != nil {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	if receipt != nil {
		payload["JsonData"] = map[string]any{"cloudpayments": map[string]any{"CustomerReceipt": receipt}}
	}
	endpoint := "/orders/create"
	if method == "sbp" {
		endpoint = "/payments/qr/sbp/link"
		payload["PublicId"] = p.publicID
		payload["Scheme"] = "charge"
		payload["IsTest"] = p.testMode
	} else {
		payload["RequireConfirmation"] = false
		payload["SendEmail"] = false
		payload["SendSms"] = false
		payload["SendViber"] = false
	}
	if req.MerchantSuccess != "" {
		payload["SuccessRedirectUrl"] = req.MerchantSuccess
	}
	if method == "card" && req.MerchantFail != "" {
		payload["FailRedirectUrl"] = req.MerchantFail
	}
	requestID := "cloudpayments:" + paymentID + ":" + method
	responseBody, err := p.doJSON(ctx, endpoint, requestID, payload)
	if err != nil {
		return providerdomain.CreatePaymentResult{}, err
	}
	var response struct {
		Success bool `json:"Success"`
		Model   struct {
			ID            string `json:"Id"`
			URL           string `json:"Url"`
			TransactionID int64  `json:"TransactionId"`
			QRURL         string `json:"QrUrl"`
		} `json:"Model"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("%w: decoding response: %w", ErrInvalidResponse, err)
	}
	if !response.Success {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("%w: provider reported failure", ErrInvalidResponse)
	}
	paymentURL := response.Model.URL
	if method == "sbp" {
		if response.Model.TransactionID <= 0 {
			return providerdomain.CreatePaymentResult{}, fmt.Errorf("%w: transaction identifier is missing", ErrInvalidResponse)
		}
		paymentURL = response.Model.QRURL
	} else if response.Model.ID == "" {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("%w: page identifier is missing", ErrInvalidResponse)
	}
	if !validHTTPURL(paymentURL) {
		return providerdomain.CreatePaymentResult{}, fmt.Errorf("%w: payment URL is invalid", ErrInvalidResponse)
	}
	return providerdomain.CreatePaymentResult{ProviderPaymentID: paymentID, PaymentURL: paymentURL}, nil
}

func (p *Provider) doJSON(ctx context.Context, endpoint, requestID string, payload map[string]any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cloudpayments request encoding: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, strings.TrimRight(p.apiURL.String(), "/")+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cloudpayments request creation: %w", err)
	}
	request.SetBasicAuth(p.publicID, p.apiSecret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", requestID)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("cloudpayments API request: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cloudpayments API response read: %w", err)
	}
	if len(responseBody) > maxAPIResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds size limit", ErrInvalidResponse)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cloudpayments API returned HTTP %d", response.StatusCode)
	}
	return responseBody, nil
}

func normalizePaymentMethod(method string) (string, error) {
	switch method {
	case "", "card", "bank_card":
		return "card", nil
	case "sbp":
		return "sbp", nil
	default:
		return "", ErrInvalidRequest
	}
}

func parsePaymentID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return "", ErrInvalidRequest
	}
	return id.String(), nil
}

func paymentEmail(req providerdomain.CreatePaymentRequest) string {
	if req.CustomerEmail != "" {
		return req.CustomerEmail
	}
	return req.Receipt.Email
}

func moneyNumber(amountMinor int64) json.Number {
	return json.Number(strconv.FormatInt(amountMinor/100, 10) + "." + twoDigits(amountMinor%100))
}

func twoDigits(value int64) string {
	if value < 10 {
		return "0" + strconv.FormatInt(value, 10)
	}
	return strconv.FormatInt(value, 10)
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" && parsed.User == nil
}
