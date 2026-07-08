package app

import (
	"context"

	"github.com/Seraf-seraf/payment/bootstrap"
	"github.com/Seraf-seraf/payment/pkg/config"
)

type App struct {
	stop func(context.Context) error
}

func New(stop func(context.Context) error) *App {
	return &App{stop: stop}
}

func (a *App) Stop(ctx context.Context) error {
	return a.stop(ctx)
}

func Run(ctx context.Context, cfg config.Config) (*App, error) {
	stop, err := bootstrap.Run(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return New(stop), nil
}
