package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

type Server struct {
	server *http.Server
	logger *slog.Logger
}

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

func (s *Server) Start(onFatal func()) {
	go func() {
		s.logger.Info("http server started", slog.String("addr", s.server.Addr))
		if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("http server failed", slog.Any("error", err))
			onFatal()
		}
	}()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}
