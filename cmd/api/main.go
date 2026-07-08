package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Seraf-seraf/payment/app"
	"github.com/Seraf-seraf/payment/pkg/config"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("load config failed", slog.Any("error", err))
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.Run(ctx, cfg)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("start app failed", slog.Any("error", err))
		os.Exit(1)
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := application.Stop(shutdownCtx); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("app shutdown failed", slog.Any("error", err))
	}
}
