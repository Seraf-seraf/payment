package outbox

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending Status = "pending"
	StatusSent    Status = "sent"
	StatusFailed  Status = "failed"
)

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
