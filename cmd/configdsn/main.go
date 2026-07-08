package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/Seraf-seraf/payment/pkg/config"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "path to config YAML")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("load config failed", slog.Any("error", err))
		os.Exit(1)
	}
	if cfg.Postgres.DSN == "" {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("postgres dsn is required")
		os.Exit(1)
	}
	fmt.Println(cfg.Postgres.DSN)
}
