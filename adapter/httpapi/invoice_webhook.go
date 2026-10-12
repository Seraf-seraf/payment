package httpapi

import (
	"bytes"
	"errors"
	"github.com/Seraf-seraf/payment/ports"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const maxInvoiceBodyBytes int64 = 1 << 20

type InvoiceWebhookDependencies struct {
	Notifications ports.InvoiceNotificationUseCase
	Decoders      ports.InvoiceNotificationDecoderRegistry
}

func limitInvoiceBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		invoiceRoute := path == "/api/v1/invoice-payments" || strings.HasPrefix(path, "/api/v1/provider-invoice-webhooks/")
		if r.Method != http.MethodPost || !invoiceRoute {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > maxInvoiceBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Тело запроса превышает 1 MiB.")
			return
		}
		data, err := readLimitedInvoiceBody(w, r)
		if err != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		next.ServeHTTP(w, r)
	})
}
func readLimitedInvoiceBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxInvoiceBodyBytes+1))
	_ = r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Некорректное тело запроса.")
		return nil, err
	}
	if int64(len(data)) > maxInvoiceBodyBytes {
		err = errors.New("invoice body too large")
		writeError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Тело запроса превышает 1 MiB.")
		return nil, err
	}
	return data, nil
}
func (s *Server) ProviderInvoiceWebhook(w http.ResponseWriter, r *http.Request, connectionID string) {
	if s.invoiceDecoders == nil || s.invoiceNotifications == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Приём уведомлений временно недоступен.")
		return
	}
	connection, decoder, ok := s.invoiceDecoders.DecoderForConnection(connectionID)
	if !ok || decoder == nil || connection.ID != connectionID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Подключение не найдено.")
		return
	}
	body, err := readLimitedInvoiceBody(w, r)
	if err != nil {
		return
	}
	notification, err := decoder.VerifyAndDecode(ports.InvoiceNotificationEnvelope{Body: body, ContentType: r.Header.Get("Content-Type"), Headers: r.Header.Clone()})
	if err != nil {
		if errors.Is(err, ports.ErrInvoiceNotificationSignature) {
			writeError(w, http.StatusUnauthorized, "INVALID_SIGNATURE", "Недействительная подпись уведомления.")
		} else {
			writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "Некорректное уведомление.")
		}
		return
	}
	if err := s.invoiceNotifications.AcceptInvoiceNotification(r.Context(), connection, notification); err != nil {
		if s.logger != nil {
			s.logger.Error("invoice notification acceptance failed", slog.String("connection_id", connectionID), slog.String("error_code", "invoice_acceptance_failed"))
		}
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Не удалось сохранить уведомление.")
		return
	}
	ack := decoder.Acknowledgement()
	w.Header().Set("Content-Type", ack.ContentType)
	w.WriteHeader(ack.Status)
	_, _ = w.Write(ack.Body)
}
