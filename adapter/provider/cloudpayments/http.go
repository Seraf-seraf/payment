package cloudpayments

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Seraf-seraf/payment/ports"
)

const maxWebhookBodyBytes = 1 << 20

func HandleWebhook(w http.ResponseWriter, r *http.Request, next ports.WebhookHandler, logger *slog.Logger) {
	respond := func(status int, category string) {
		code := 1
		if status == http.StatusOK {
			code = 0
		} else if logger != nil {
			logger.Error("cloudpayments webhook rejected", slog.String("provider", "cloudpayments"), slog.String("phase", "webhook"), slog.String("error_category", category))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(struct {
			Code int `json:"code"`
		}{code})
	}
	if r.Method != http.MethodPost {
		respond(http.StatusMethodNotAllowed, "method")
		return
	}
	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && contentType != "application/x-www-form-urlencoded") || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) {
		respond(http.StatusUnsupportedMediaType, "content_type")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBodyBytes+1))
	if err != nil {
		respond(http.StatusServiceUnavailable, "body_read")
		return
	}
	if len(raw) > maxWebhookBodyBytes {
		respond(http.StatusRequestEntityTooLarge, "body_limit")
		return
	}
	if !utf8.Valid(raw) {
		respond(http.StatusBadRequest, "notification")
		return
	}
	body, err := json.Marshal(webhookEnvelope{ContentType: contentType, RawBody: raw})
	if err != nil {
		respond(http.StatusServiceUnavailable, "envelope")
		return
	}
	err = next.Handle(r.Context(), "cloudpayments", r.Header, body)
	switch {
	case err == nil:
		respond(http.StatusOK, "")
	case errors.Is(err, ErrInvalidSignature):
		respond(http.StatusUnauthorized, "signature")
	case errors.Is(err, ErrInvalidNotification):
		respond(http.StatusBadRequest, "notification")
	case errors.Is(err, ErrPaymentMismatch):
		respond(http.StatusConflict, "mismatch")
	default:
		respond(http.StatusServiceUnavailable, "processing")
	}
}
