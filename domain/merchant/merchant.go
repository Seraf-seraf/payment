package merchant

import "github.com/google/uuid"

// Merchant описывает клиента сервиса, который может создавать платежи.
type Merchant struct {
	ID           uuid.UUID
	Name         string
	APIKeyHash   string
	SharedSecret string
	CallbackURL  string
	SuccessURL   string
	FailURL      string
	ProviderName string
	IsActive     bool
}
