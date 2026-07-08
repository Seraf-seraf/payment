package app

import (
	"context"

	"github.com/Seraf-seraf/payment/bootstrap"
	"github.com/Seraf-seraf/payment/pkg/config"
)

// App представляет запущенное приложение и управляет его остановкой.
type App struct {
	stop func(context.Context) error
}

// New создает приложение с переданной функцией остановки.
func New(stop func(context.Context) error) *App {
	return &App{stop: stop}
}

// Stop корректно останавливает приложение в рамках переданного контекста.
func (a *App) Stop(ctx context.Context) error {
	return a.stop(ctx)
}

// Run собирает и запускает приложение через bootstrap.
func Run(ctx context.Context, cfg config.Config) (*App, error) {
	stop, err := bootstrap.Run(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return New(stop), nil
}
