package metrics

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// HTTPRequestsTotal считает HTTP-запросы по методу, route pattern и статусу.
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "payment_service",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total number of HTTP requests processed by the API.",
	}, []string{"method", "route", "code"})

	// PaymentsCreatedTotal считает созданные платежи.
	PaymentsCreatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payment_service",
		Name:      "payments_created_total",
		Help:      "Total number of created payments.",
	})
	// PaymentsSucceededTotal считает успешно оплаченные платежи.
	PaymentsSucceededTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payment_service",
		Name:      "payments_succeeded_total",
		Help:      "Total number of succeeded payments.",
	})
	// PaymentsFailedTotal считает платежи, завершенные ошибкой.
	PaymentsFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payment_service",
		Name:      "payments_failed_total",
		Help:      "Total number of failed payments.",
	})
	// WebhooksReceivedTotal считает полученные webhook от провайдеров.
	WebhooksReceivedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payment_service",
		Name:      "webhooks_received_total",
		Help:      "Total number of received provider webhooks.",
	})
	// WebhooksDuplicateTotal считает дубли webhook от провайдеров.
	WebhooksDuplicateTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payment_service",
		Name:      "webhooks_duplicate_total",
		Help:      "Total number of duplicate provider webhooks.",
	})
	// OutboxPendingTotal показывает размер последней пачки pending outbox-событий.
	OutboxPendingTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "payment_service",
		Name:      "outbox_pending_total",
		Help:      "Current pending outbox events observed by workers.",
	})
	// OutboxFailedTotal считает окончательно не доставленные outbox-события.
	OutboxFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payment_service",
		Name:      "outbox_failed_total",
		Help:      "Total number of failed outbox events.",
	})
	// ProviderRequestDurationSeconds измеряет длительность запросов к провайдерам.
	ProviderRequestDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "payment_service",
		Name:      "provider_request_duration_seconds",
		Help:      "Provider request duration in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"provider", "operation", "success"})
)

// ObserveProviderRequest записывает длительность запроса к платежному провайдеру.
func ObserveProviderRequest(provider, operation string, success bool, seconds float64) {
	ProviderRequestDurationSeconds.WithLabelValues(provider, operation, strconv.FormatBool(success)).Observe(seconds)
}
