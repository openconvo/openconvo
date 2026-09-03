package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// HandlerFunc executes one job. Returning an error schedules a retry
// (or marks the job failed once attempts are exhausted).
type HandlerFunc func(ctx context.Context, job *Job) error

// Worker polls the queue and executes jobs with bounded concurrency.
type Worker struct {
	queue        *Queue
	logger       *slog.Logger
	handlers     map[string]HandlerFunc
	concurrency  int
	pollInterval time.Duration
}

// NewWorker creates a worker. Handlers are registered with Register
// before Run is called.
func NewWorker(queue *Queue, logger *slog.Logger) *Worker {
	return &Worker{
		queue:        queue,
		logger:       logger.With("component", "jobs"),
		handlers:     map[string]HandlerFunc{},
		concurrency:  4,
		pollInterval: 2 * time.Second,
	}
}

// Register adds a handler for a job kind. Must be called before Run.
func (w *Worker) Register(kind string, fn HandlerFunc) {
	w.handlers[kind] = fn
}

// WithConcurrency sets how many jobs a worker runs at once.
func (w *Worker) WithConcurrency(n int) *Worker {
	if n > 0 {
		w.concurrency = n
	}
	return w
}

// Kinds returns the registered job kinds.
func (w *Worker) Kinds() []string {
	kinds := make([]string, 0, len(w.handlers))
	for k := range w.handlers {
		kinds = append(kinds, k)
	}
	return kinds
}

// Run processes jobs until ctx is cancelled, then waits for in-flight
// jobs to unwind. Cancellation reaches the handlers, so a job still
// running at shutdown is returned to "pending" with its attempt given
// back: restarting mid-backfill must not spend a job's retries or
// record shutdown as a failure.
//
// Jobs of this worker's own kinds left in "running" state by a previous
// process are requeued first — only its own, because another worker
// sharing this queue may already be executing jobs of the kinds it owns.
func (w *Worker) Run(ctx context.Context) {
	kinds := w.Kinds()
	if reset, err := w.queue.ResetRunning(ctx, kinds); err != nil {
		w.logger.Error("requeue stale running jobs", "error", err)
	} else if reset > 0 {
		w.logger.Info("requeued jobs left running by previous process", "count", reset, "kinds", kinds)
	}

	w.logger.Info("worker started", "concurrency", w.concurrency, "kinds", kinds)

	sem := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup

loop:
	for ctx.Err() == nil {
		job, err := w.queue.claim(ctx, kinds)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			w.logger.Error("claim job", "error", err)
			if !sleepCtx(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if job == nil {
			if !sleepCtx(ctx, w.pollInterval) {
				break
			}
			continue
		}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Couldn't get a slot before shutdown; return the job.
			if err := w.queue.release(context.WithoutCancel(ctx), job); err != nil {
				w.logger.Error("release job on shutdown", "job_id", job.ID, "error", err)
			}
			break loop
		}

		wg.Add(1)
		go func(job *Job) {
			defer wg.Done()
			defer func() { <-sem }()
			w.execute(ctx, job)
		}(job)
	}

	wg.Wait()
	w.logger.Info("worker stopped")
}

func (w *Worker) execute(ctx context.Context, job *Job) {
	logger := w.logger.With("job_id", job.ID, "kind", job.Kind, "attempt", job.Attempts)

	handler, ok := w.handlers[job.Kind]
	if !ok {
		// No amount of retrying fixes an unknown kind: fail immediately.
		logger.Error("no handler registered for job kind")
		job.Attempts = job.MaxAttempts
		if err := w.queue.fail(context.WithoutCancel(ctx), job, fmt.Errorf("no handler for kind %q", job.Kind)); err != nil {
			logger.Error("mark job failed", "error", err)
		}
		return
	}

	start := time.Now()
	err := runHandler(ctx, handler, job)
	// Completion bookkeeping must survive shutdown cancellation.
	finishCtx := context.WithoutCancel(ctx)

	if err != nil {
		if ctx.Err() != nil {
			// Shutdown cut the handler short, so its error says nothing
			// about the job: return it to the queue, attempt intact.
			logger.Info("job interrupted by shutdown", "duration", time.Since(start))
			if relErr := w.queue.release(finishCtx, job); relErr != nil {
				logger.Error("release interrupted job", "job_id", job.ID, "error", relErr)
			}
			return
		}
		logger.Warn("job failed", "error", err, "duration", time.Since(start))
		if failErr := w.queue.fail(finishCtx, job, err); failErr != nil {
			logger.Error("record job failure", "error", failErr)
		}
		return
	}
	logger.Debug("job succeeded", "duration", time.Since(start))
	if err := w.queue.succeed(finishCtx, job.ID); err != nil {
		logger.Error("record job success", "error", err)
	}
}

func runHandler(ctx context.Context, handler HandlerFunc, job *Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return handler(ctx, job)
}

// sleepCtx sleeps for d, returning false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
