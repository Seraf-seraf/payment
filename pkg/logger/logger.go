package logger

import (
	"log/slog"
	"os"
)

func New(appName, env string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		slog.String("app", appName),
		slog.String("env", env),
	)
}
