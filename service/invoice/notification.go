package invoice

import (
	"context"
	"errors"
	d "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/ports"
	"github.com/google/uuid"
	"time"
)

type NotificationService struct {
	tx  ports.TransactionManager
	now func() time.Time
}

func NewNotificationService(tx ports.TransactionManager, now func() time.Time) *NotificationService {
	return &NotificationService{tx: tx, now: now}
}
func (s *NotificationService) AcceptInvoiceNotification(ctx context.Context, connection d.Connection, n ports.VerifiedInvoiceNotification) error {
	if s.tx == nil || s.now == nil {
		return errors.New("invoice notification service not configured")
	}
	now := s.now().UTC()
	inbox := d.Inbox{ID: uuid.New(), ConnectionID: connection.ID, EventKey: n.EventKey, RawBodyHash: n.RawBodyHash, Observation: n.Observation, VerificationKind: n.VerificationKind, ReceivedAt: now}
	return s.tx.WithinTx(ctx, func(ctx context.Context, r ports.Repositories) error {
		created, err := r.InvoiceInbox.Create(ctx, inbox)
		if err != nil {
			return err
		}
		if !created {
			return nil
		}
		return r.InvoiceCommands.Create(ctx, newCommand("invoice.apply_notification", uuid.Nil, inbox.ID, now))
	})
}
