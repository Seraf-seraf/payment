package httpapi

import (
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httplog/v3"
	"github.com/go-chi/traceid"
	"github.com/justinas/alice"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed openapi.yaml
var openAPIFS embed.FS

var httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "go_swagger_template",
	Subsystem: "http",
	Name:      "requests_total",
	Help:      "Total number of HTTP requests processed by the API.",
}, []string{"method", "route", "code"})

func NewRouter(logger *slog.Logger) http.Handler {
	r := chi.NewRouter()

	logSchema := httplog.SchemaECS.Concise(false)
	requestLogger := logger.With(slog.String("component", "http"))
	chain := alice.New(
		traceid.Middleware,
		httplog.RequestLogger(requestLogger, &httplog.Options{
			Level:         slog.LevelInfo,
			Schema:        logSchema,
			RecoverPanics: true,
		}),
	)
	r.Use(chain.Then)

	r.Handle("/metrics", promhttp.Handler())

	spec := mustOpenAPISpec()
	r.Route("/api/v1", func(api chi.Router) {
		api.Use(nethttpmiddleware.OapiRequestValidator(spec))
		api.Get("/health", instrument("GET", "/api/v1/health", healthHandler()))
	})

	return r
}

func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func instrument(method, route string, handler http.Handler) http.HandlerFunc {
	wrapped := promhttp.InstrumentHandlerCounter(httpRequestsTotal.MustCurryWith(prometheus.Labels{
		"method": method,
		"route":  route,
	}), handler)
	return wrapped.ServeHTTP
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
