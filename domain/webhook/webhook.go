package webhook

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID                uuid.UUID
	ProviderName      string
	ProviderEventID   string
	ProviderPaymentID string
	EventType         string
	RawPayload        json.RawMessage
	SignatureValid    bool
	ProcessedAt       *time.Time
	CreatedAt         time.Time
}
