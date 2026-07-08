package httpapi

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/traceid"
	"github.com/justinas/alice"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Seraf-seraf/payment/ports"
)

//go:embed openapi.yaml
var openAPIFS embed.FS

var httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "payment_service",
	Subsystem: "http",
	Name:      "requests_total",
	Help:      "Total number of HTTP requests processed by the API.",
}, []string{"method", "route", "code"})

func NewRouter(
	logger *slog.Logger,
	merchants ports.MerchantAuthenticator,
	payments ports.PaymentUseCase,
	webhooks ports.WebhookHandler,
	hmacMaxSkew time.Duration,
) http.Handler {
	r := chi.NewRouter()

	requestLogger := logger.With(slog.String("component", "http"))
	r.Use(alice.New(
		traceid.Middleware,
		httplog.RequestLogger(requestLogger, &httplog.Options{
			Level:         slog.LevelInfo,
			Schema:        httplog.SchemaECS.Concise(false),
			RecoverPanics: true,
		}),
	).Then)

	r.Handle("/metrics", promhttp.Handler())

	spec := mustOpenAPISpec()
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(nethttpmiddleware.OapiRequestValidator(spec))
		server := NewServer(logger, merchants, payments, webhooks, hmacMaxSkew)
		HandlerWithOptions(server, ChiServerOptions{
			BaseRouter: api,
			ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
				writeError(w, http.StatusBadRequest, "bad_request", "Некорректный запрос.")
			},
		})
	})

	return r
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func mustOpenAPISpec() *openapi3.T {
	specBytes, err := openAPIFS.ReadFile("openapi.yaml")
	if err != nil {
		panic(err)
	}

	loader := openapi3.NewLoader()
	spec, err := loader.LoadFromData(specBytes)
	if err != nil {
		panic(err)
	}
	if err := spec.Validate(loader.Context); err != nil {
		panic(err)
	}

	for _, server := range spec.Servers {
		server.URL = "/api/v1"
	}

	return spec
}
