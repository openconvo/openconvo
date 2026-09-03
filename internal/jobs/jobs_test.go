package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/jobs"
	"github.com/openconvo/openconvo/internal/testutil"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestEnqueueAndWorkerLifecycle(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	type payload struct {
		ChannelID string `json:"channel_id"`
	}

	var handled atomic.Int32
	worker := jobs.NewWorker(queue, discard())
	worker.Register("test.echo", func(_ context.Context, job *jobs.Job) error {
		var p payload
		if err := job.UnmarshalPayload(&p); err != nil {
			return err
		}
		if p.ChannelID != "chan-1" {
			return fmt.Errorf("unexpected payload: %+v", p)
		}
		handled.Add(1)
		return nil
	})

	id, err := queue.Enqueue(ctx, "test.echo", payload{ChannelID: "chan-1"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id == "" {
		t.Fatal("empty job id")
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		worker.Run(runCtx)
		close(done)
	}()

	waitFor(t, 10*time.Second, func() bool { return handled.Load() == 1 })
	cancel()
	<-done

	job, err := queue.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
}

func TestRetryWithBackoffThenFailure(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	var calls atomic.Int32
	worker := jobs.NewWorker(queue, discard())
	worker.Register("test.flaky", func(context.Context, *jobs.Job) error {
		calls.Add(1)
		return errors.New("transient explosion")
	})

	id, err := queue.Enqueue(ctx, "test.flaky", nil, jobs.WithMaxAttempts(2))
	if err != nil {
		t.Fatal(err)
	}

	// First attempt fails and is rescheduled with backoff.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Run(runCtx); close(done) }()

	waitFor(t, 10*time.Second, func() bool {
		job, err := queue.Get(ctx, id)
		return err == nil && job.Attempts == 1 && job.Status == jobs.StatusPending
	})
	job, _ := queue.Get(ctx, id)
	if job.LastError == "" {
		t.Error("last_error empty after failed attempt")
	}
	if !job.AvailableAt.After(time.Now().Add(time.Second)) {
		t.Errorf("no backoff applied: available_at = %s", job.AvailableAt)
	}
	cancel()
	<-done

	// Make it due now; the final attempt exhausts retries.
	if _, err := pool.Exec(ctx,
		"UPDATE jobs SET available_at = now() WHERE id = $1::uuid", id); err != nil {
		t.Fatal(err)
	}
	runCtx2, cancel2 := context.WithCancel(ctx)
	done2 := make(chan struct{})
	go func() { worker.Run(runCtx2); close(done2) }()

	waitFor(t, 10*time.Second, func() bool {
		job, err := queue.Get(ctx, id)
		return err == nil && job.Status == jobs.StatusFailed
	})
	cancel2()
	<-done2

	if calls.Load() != 2 {
		t.Errorf("handler calls = %d, want 2", calls.Load())
	}
}

func TestPanicIsARetryableFailure(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	worker := jobs.NewWorker(queue, discard())
	worker.Register("test.panic", func(context.Context, *jobs.Job) error {
		panic("boom")
	})

	id, err := queue.Enqueue(ctx, "test.panic", nil, jobs.WithMaxAttempts(1))
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Run(runCtx); close(done) }()
	waitFor(t, 10*time.Second, func() bool {
		job, err := queue.Get(ctx, id)
		return err == nil && job.Status == jobs.StatusFailed
	})
	cancel()
	<-done

	job, _ := queue.Get(ctx, id)
	if job.LastError == "" || job.Status != jobs.StatusFailed {
		t.Errorf("panic not recorded: status=%s err=%q", job.Status, job.LastError)
	}
}

// Shutting down mid-job must hand the job back to the queue untouched:
// it was interrupted, not tried and found wanting. Billing it an attempt
// (with a "context canceled" last_error) lets a few restarts during one
// long backfill drive the job to terminal failure.
func TestShutdownReturnsInFlightJobToQueue(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	started := make(chan struct{})
	worker := jobs.NewWorker(queue, discard())
	worker.Register("test.slow", func(handlerCtx context.Context, _ *jobs.Job) error {
		close(started)
		<-handlerCtx.Done()
		return handlerCtx.Err()
	})

	// One attempt: under the old behaviour shutdown alone failed the job.
	id, err := queue.Enqueue(ctx, "test.slow", nil, jobs.WithMaxAttempts(1))
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Run(runCtx); close(done) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("job never started")
	}
	cancel()
	<-done

	job, err := queue.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("status = %s, want pending — shutdown must not fail an interrupted job", job.Status)
	}
	if job.Attempts != 0 {
		t.Errorf("attempts = %d, want 0 — shutdown must not consume an attempt", job.Attempts)
	}
	if job.LastError != "" {
		t.Errorf("last_error = %q, want empty — shutdown is not a job failure", job.LastError)
	}
	if job.AvailableAt.After(time.Now()) {
		t.Errorf("available_at = %s, want claimable now — no backoff for an interrupted job", job.AvailableAt)
	}
}

// A worker with no registered handlers has an empty kind list, so per
// the claim filter it must never claim anything — not fail it, not
// hang trying, just leave it for whichever worker can handle it.
func TestWorkerWithNoHandlersClaimsNothing(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	id, err := queue.Enqueue(ctx, "test.unregistered", nil)
	if err != nil {
		t.Fatal(err)
	}

	worker := jobs.NewWorker(queue, discard())
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Run(runCtx); close(done) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	job, err := queue.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("status = %s, want pending (worker with no handlers claimed a job)", job.Status)
	}
}

// A worker must leave other workers' jobs alone: two workers share one
// queue, and claiming a kind you cannot handle fails that job outright.
func TestWorkerClaimsOnlyRegisteredKinds(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	if _, err := queue.Enqueue(ctx, "mine", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	othersID, err := queue.Enqueue(ctx, "theirs", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}

	handled := make(chan struct{}, 1)
	worker := jobs.NewWorker(queue, discard())
	worker.Register("mine", func(context.Context, *jobs.Job) error {
		handled <- struct{}{}
		return nil
	})

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(runCtx) }()

	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("registered kind was never handled")
	}
	cancel()
	<-done

	other, err := queue.Get(ctx, othersID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != jobs.StatusPending {
		t.Errorf("unregistered kind = %q, want %q (another worker's job was consumed)",
			other.Status, jobs.StatusPending)
	}
	if other.Attempts != 0 {
		t.Errorf("unregistered kind attempts = %d, want 0", other.Attempts)
	}
}

func TestDedupeKey(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	first, err := queue.Enqueue(ctx, "test.backfill", nil, jobs.WithDedupeKey("backfill:chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("first enqueue suppressed")
	}
	second, err := queue.Enqueue(ctx, "test.backfill", nil, jobs.WithDedupeKey("backfill:chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	if second != "" {
		t.Errorf("duplicate enqueue not suppressed: %s", second)
	}
	// A different key is unaffected.
	third, err := queue.Enqueue(ctx, "test.backfill", nil, jobs.WithDedupeKey("backfill:chan-2"))
	if err != nil {
		t.Fatal(err)
	}
	if third == "" {
		t.Error("distinct dedupe key suppressed")
	}

	// Once the first job completes, the key may be reused.
	if _, err := pool.Exec(ctx,
		"UPDATE jobs SET status = 'succeeded' WHERE id = $1::uuid", first); err != nil {
		t.Fatal(err)
	}
	fourth, err := queue.Enqueue(ctx, "test.backfill", nil, jobs.WithDedupeKey("backfill:chan-1"))
	if err != nil {
		t.Fatal(err)
	}
	if fourth == "" {
		t.Error("dedupe key not released after completion")
	}
}

func TestResetRunning(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	id, err := queue.Enqueue(ctx, "test.stuck", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-job.
	if _, err := pool.Exec(ctx,
		"UPDATE jobs SET status = 'running', started_at = now() WHERE id = $1::uuid", id); err != nil {
		t.Fatal(err)
	}

	reset, err := queue.ResetRunning(ctx, []string{"test.stuck"})
	if err != nil {
		t.Fatal(err)
	}
	if reset != 1 {
		t.Errorf("reset = %d, want 1", reset)
	}
	job, err := queue.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("status = %s, want pending", job.Status)
	}
}

// Two workers share one queue and start concurrently, so each one's
// startup reset must not touch the other's in-flight work. Without the
// kind filter, worker B's reset requeues the job worker A is executing:
// it runs twice, its attempt count is inflated, and the log claims a
// previous process left it behind.
func TestResetRunningLeavesOtherKindsRunning(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	id, err := queue.Enqueue(ctx, "worker.a", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Worker A claims and holds the job, exactly as it would while
	// worker B is still starting up.
	claimed := make(chan struct{})
	release := make(chan struct{})
	workerA := jobs.NewWorker(queue, discard())
	workerA.Register("worker.a", func(handlerCtx context.Context, _ *jobs.Job) error {
		close(claimed)
		// Also watch the handler's context, so a t.Fatal below cannot
		// leave this goroutine parked and hang the suite.
		select {
		case <-release:
		case <-handlerCtx.Done():
		}
		return nil
	})

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); workerA.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-claimed:
	case <-time.After(10 * time.Second):
		t.Fatal("worker A never claimed its job")
	}

	// Worker B starting up: it resets only the kinds it owns.
	reset, err := queue.ResetRunning(ctx, []string{"worker.b"})
	if err != nil {
		t.Fatal(err)
	}
	if reset != 0 {
		t.Errorf("reset = %d, want 0 — a worker must only requeue its own kinds", reset)
	}

	job, err := queue.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusRunning {
		t.Errorf("status = %s, want running — another worker's reset requeued a job in flight", job.Status)
	}
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a requeued job is claimed again and re-counted", job.Attempts)
	}

	close(release)
	cancel()
	<-done
}

func TestScheduledJobNotClaimedEarly(t *testing.T) {
	pool := testutil.NewDB(t)
	ctx := context.Background()
	queue := jobs.NewQueue(pool)

	var ran atomic.Int32
	worker := jobs.NewWorker(queue, discard())
	worker.Register("test.later", func(context.Context, *jobs.Job) error {
		ran.Add(1)
		return nil
	})

	if _, err := queue.Enqueue(ctx, "test.later", nil,
		jobs.WithRunAt(time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { worker.Run(runCtx); close(done) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	if ran.Load() != 0 {
		t.Errorf("future job ran early %d times", ran.Load())
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if jobs.Backoff(1) >= jobs.Backoff(3) {
		t.Error("backoff does not grow")
	}
	if jobs.Backoff(50) != jobs.Backoff(51) {
		t.Error("backoff does not cap")
	}
	if jobs.Backoff(50) > 10*time.Minute {
		t.Errorf("backoff cap too large: %s", jobs.Backoff(50))
	}
	// A job that never recorded an attempt must not panic on a negative
	// shift; it backs off as if it were the first attempt.
	if got := jobs.Backoff(0); got != jobs.Backoff(1) {
		t.Errorf("Backoff(0) = %s, want the first-attempt delay %s", got, jobs.Backoff(1))
	}
	if got := jobs.Backoff(-3); got != jobs.Backoff(1) {
		t.Errorf("Backoff(-3) = %s, want the first-attempt delay %s", got, jobs.Backoff(1))
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
