package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	outboxdomain "github.com/Seraf-seraf/payment/domain/outbox"
	"github.com/Seraf-seraf/payment/pkg/metrics"
	"github.com/Seraf-seraf/payment/ports"
)

// Config задает параметры обработки outbox-событий.
type Config struct {
	PollInterval time.Duration
	BatchSize    int
	MaxAttempts  int
	WorkerCount  int
}

// Worker доставляет pending outbox-события callback-эндпоинтам мерчантов.
type Worker struct {
	logger    *slog.Logger
	cfg       Config
	merchants ports.MerchantRepository
	payments  ports.PaymentRepository
	outbox    ports.OutboxRepository
	callbacks ports.CallbackSender
	tx        ports.TransactionManager
	now       func() time.Time

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

var _ ports.OutboxWorker = (*Worker)(nil)

// NewWorker создает worker для обработки outbox-событий.
func NewWorker(
	logger *slog.Logger,
	cfg Config,
	merchants ports.MerchantRepository,
	payments ports.PaymentRepository,
	outbox ports.OutboxRepository,
	callbacks ports.CallbackSender,
	tx ports.TransactionManager,
	now func() time.Time,
) *Worker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 10
	}
	if cfg.WorkerCount <= 0 {
		cfg.WorkerCount = 1
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{
		logger:    logger,
		cfg:       cfg,
		merchants: merchants,
		payments:  payments,
		outbox:    outbox,
		callbacks: callbacks,
		tx:        tx,
		now:       now,
		done:      make(chan struct{}),
	}
}

// Start запускает фоновые goroutine обработки outbox-событий.
func (w *Worker) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	var wg sync.WaitGroup
	wg.Add(w.cfg.WorkerCount)
	for i := 0; i < w.cfg.WorkerCount; i++ {
		go func(workerID int) {
			defer wg.Done()
			w.loop(runCtx, workerID)
		}(i + 1)
	}
	go func() {
		wg.Wait()
		close(w.done)
	}()
}

// Stop останавливает worker и ждет завершения его goroutine.
func (w *Worker) Stop(ctx context.Context) error {
	w.once.Do(func() {
		if w.cancel != nil {
			w.cancel()
		}
	})
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) loop(ctx context.Context, workerID int) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if err := w.ProcessBatch(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("outbox batch failed", slog.Any("error", err), slog.Int("worker_id", workerID))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ProcessBatch обрабатывает одну пачку pending outbox-событий.
func (w *Worker) ProcessBatch(ctx context.Context) error {
	if w.tx == nil {
		return w.processBatch(ctx, w.outbox)
	}
	return w.tx.WithinTx(ctx, func(txCtx context.Context, repos ports.Repositories) error {
		return w.processBatch(txCtx, repos.Outbox)
	})
}

func (w *Worker) processBatch(ctx context.Context, outbox ports.OutboxRepository) error {
	events, err := outbox.FetchPending(ctx, w.cfg.BatchSize, w.now())
	if err != nil {
		return err
	}
	metrics.OutboxPendingTotal.Set(float64(len(events)))
	for _, event := range events {
		if err := w.processEvent(ctx, outbox, event); err != nil {
			w.logger.Error("outbox event failed", slog.Any("error", err), slog.String("event_id", event.ID.String()))
		}
	}
	return nil
}

func (w *Worker) processEvent(ctx context.Context, outbox ports.OutboxRepository, event outboxdomain.Event) error {
	payment, err := w.payments.FindByID(ctx, event.AggregateID)
	if err != nil {
		return err
	}
	merchant, err := w.merchants.FindByID(ctx, payment.MerchantID)
	if err != nil {
		return err
	}
	if err := w.callbacks.Send(ctx, merchant, payment, event); err != nil {
		attempts := event.Attempts + 1
		if attempts >= w.cfg.MaxAttempts {
			metrics.OutboxFailedTotal.Inc()
			return outbox.MarkFailed(ctx, event.ID, attempts, err.Error())
		}
		return outbox.MarkRetry(ctx, event.ID, attempts, w.now().Add(backoff(attempts)), err.Error())
	}
	return outbox.MarkSent(ctx, event.ID)
}

func backoff(attempt int) time.Duration {
	delay := time.Duration(attempt*attempt) * time.Second
	if delay > time.Minute {
		return time.Minute
	}
	return delay
}
