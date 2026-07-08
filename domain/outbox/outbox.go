package outbox

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Status описывает состояние доставки outbox-события.
type Status string

const (
	// StatusPending означает, что событие ожидает доставки.
	StatusPending Status = "pending"
	// StatusSent означает, что событие успешно доставлено.
	StatusSent Status = "sent"
	// StatusFailed означает, что событие не удалось доставить после всех попыток.
	StatusFailed Status = "failed"
)

// Event описывает событие outbox для асинхронной доставки callback мерчанту.
type Event struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	Payload       json.RawMessage
	Status        Status
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
