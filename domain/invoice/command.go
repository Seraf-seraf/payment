package invoice

import (
	"github.com/google/uuid"
	"time"
)

type Command struct {
	ID             uuid.UUID
	CommandType    string
	PaymentID      uuid.UUID
	InboxID        uuid.UUID
	Attempts       int
	LeaseToken     uuid.UUID
	LeaseExpiresAt time.Time
	Status         string
	NextAttemptAt  time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
