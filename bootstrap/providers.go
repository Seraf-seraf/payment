package bootstrap

import "github.com/Seraf-seraf/payment/ports"

type ProviderRegistry struct {
	providers map[string]ports.PaymentProvider
}

var _ ports.ProviderRegistry = (*ProviderRegistry)(nil)

func NewProviderRegistry(providers ...ports.PaymentProvider) *ProviderRegistry {
	registry := &ProviderRegistry{providers: map[string]ports.PaymentProvider{}}
	for _, provider := range providers {
		registry.providers[provider.Name()] = provider
	}
	return registry
}

func (r *ProviderRegistry) Get(name string) (ports.PaymentProvider, bool) {
	provider, ok := r.providers[name]
	return provider, ok
}
