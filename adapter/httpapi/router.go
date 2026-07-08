package httpapi

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/traceid"
	"github.com/justinas/alice"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Seraf-seraf/payment/pkg/metrics"
	"github.com/Seraf-seraf/payment/ports"
)

//go:embed openapi.yaml
var openAPIFS embed.FS

// NewRouter создает HTTP router с middleware, OpenAPI-валидацией и handlers.
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
	r.Use(recordHTTPMetrics)

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

type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader сохраняет HTTP status code перед передачей ответа клиенту.
func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Write сохраняет status OK для ответов без явного WriteHeader.
func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

func recordHTTPMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		route := "unknown"
		if routeContext := chi.RouteContext(r.Context()); routeContext != nil {
			if pattern := routeContext.RoutePattern(); pattern != "" {
				route = pattern
			}
		}
		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(status)).Inc()
	})
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
