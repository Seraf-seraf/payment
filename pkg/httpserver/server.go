package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Server оборачивает http.Server и logger для управляемого запуска.
type Server struct {
	server *http.Server
	logger *slog.Logger
}

// New создает HTTP server wrapper.
func New(addr string, handler http.Handler, readHeaderTimeout time.Duration, logger *slog.Logger) *Server {
	return &Server{
		server: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
		},
		logger: logger,
	}
}

// Start запускает HTTP server и вызывает onFatal при фатальной ошибке.
func (s *Server) Start(onFatal func()) {
	go func() {
		s.logger.Info("http server started", slog.String("addr", s.server.Addr))
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http server failed", slog.Any("error", err))
			onFatal()
		}
	}()
}

// Shutdown корректно останавливает HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
