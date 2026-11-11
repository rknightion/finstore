// Package scheduler runs a few named jobs on fixed intervals or at a time of day, in process.
//
// Each job has its own goroutine, so a slow or failing job never delays another and a job never
// overlaps itself. A panic or error in a job is logged by job name and outcome only (never the
// panic value or error text, which could carry bank data) and the job runs again at its next slot.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/rknightion/finstore/internal/semconv"
)

// Job is one unit of scheduled work. It should return when ctx is done.
type Job func(ctx context.Context) error

// Observer watches each job run. Start is called as a run begins, with the context the job would
// receive, and returns the context to give the job (it may carry a span) and a function the
// scheduler calls once the run has ended, with its outcome: ok, error or panic. A job's panic is
// recovered before that call, so finish always runs.
type Observer interface {
	Start(ctx context.Context, job string) (context.Context, func(outcome string))
}

// Scheduler runs jobs registered with Every and Daily once Run is called.
type Scheduler struct {
	now func() time.Time
	log *slog.Logger
	obs Observer

	// Test seams: after waits d, and rnd returns a value in [0, n).
	after func(time.Duration) <-chan time.Time
	rnd   func(n int64) int64

	mu       sync.Mutex
	tasks    []task
	started  bool
	lastTick time.Time
}

// task is one registered job. first gives the delay before its first run; next gives the delay
// after a run that started at start and ended at end.
type task struct {
	name  string
	job   Job
	first func(now time.Time) time.Duration
	next  func(start, end time.Time) time.Duration
}

// New returns a Scheduler that reads the time from now and logs to log. A non-nil obs is told of
// every job run; nil observes nothing.
func New(now func() time.Time, log *slog.Logger, obs Observer) *Scheduler {
	return &Scheduler{
		now:   now,
		log:   log,
		obs:   obs,
		after: time.After,
		rnd:   rand.Int64N,
	}
}

// Every runs job every interval. The first run comes after a random delay in [0, jitter]. The
// interval counts from the start of a run, so a run that outlasts it is followed at once by the
// next, never by an overlapping one.
func (s *Scheduler) Every(name string, interval, jitter time.Duration, job Job) {
	if interval <= 0 || jitter < 0 {
		panic("scheduler: Every needs a positive interval and a non-negative jitter")
	}
	s.add(task{
		name: name,
		job:  job,
		first: func(time.Time) time.Duration {
			if jitter == 0 {
				return 0
			}
			return time.Duration(s.rnd(int64(jitter) + 1))
		},
		next: func(start, end time.Time) time.Duration { return max(0, interval-end.Sub(start)) },
	})
}

// Daily runs job each day at hour:minute local time in loc. A time that does not exist on a day
// (clocks going forward) runs at the same instant the day's clocks would have shown it, which is
// that wall time moved on by the change; a time that occurs twice (clocks going back) runs at its
// first occurrence only. Each run starts a random delay in [0, jitter] after its time, so that
// a fleet of clients does not reach a bank in the same second; the next day's time is counted
// from the slot, so the delay never accumulates. A jitter of an hour or more is refused: it could
// push a run past the next day's slot.
func (s *Scheduler) Daily(name string, hour, minute int, loc *time.Location, jitter time.Duration, job Job) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || loc == nil {
		panic("scheduler: Daily needs a valid time of day and a location")
	}
	if jitter < 0 || jitter >= time.Hour {
		panic("scheduler: Daily needs a jitter of at least zero and under an hour")
	}
	delay := func() time.Duration {
		if jitter == 0 {
			return 0
		}
		return time.Duration(s.rnd(int64(jitter) + 1))
	}
	s.add(task{
		name: name,
		job:  job,
		first: func(now time.Time) time.Duration {
			return nextDaily(now, hour, minute, loc).Sub(now) + delay()
		},
		// start is at or after its slot and under an hour past it, so the next slot after start is
		// the following day's.
		next: func(start, end time.Time) time.Duration {
			return max(0, nextDaily(start, hour, minute, loc).Add(delay()).Sub(end))
		},
	})
}

func (s *Scheduler) add(t task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		panic("scheduler: job added after Run")
	}
	s.tasks = append(s.tasks, t)
}

// Names returns the names of the registered jobs, in the order they were registered.
func (s *Scheduler) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.tasks))
	for i, t := range s.tasks {
		out[i] = t.name
	}
	return out
}

// LastTick returns when any job last started, or the zero time before the first start.
func (s *Scheduler) LastTick() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTick
}

// Run starts every job and blocks until ctx is done, then waits for the jobs still running. It
// returns an error only when called a second time.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("scheduler: Run called twice")
	}
	s.started = true
	tasks := s.tasks
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Go(func() { s.loop(ctx, t) })
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

func (s *Scheduler) loop(ctx context.Context, t task) {
	d := t.first(s.now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.after(d):
		}
		if ctx.Err() != nil {
			return
		}
		start := s.now()
		s.mu.Lock()
		s.lastTick = start
		s.mu.Unlock()
		s.runJob(ctx, t)
		d = t.next(start, s.now())
	}
}

// runJob runs one job, recovering a panic and logging a failure by name and outcome only.
//
// A job that returns an error after ctx was cancelled is reported as cancelled, not as an error:
// that is the service shutting down, and not a failure of the job. It is not logged as one.
func (s *Scheduler) runJob(ctx context.Context, t task) {
	jobCtx, finish := s.observeStart(ctx, t.name)
	outcome := semconv.JobOK
	defer func() {
		if r := recover(); r != nil {
			outcome = semconv.JobPanic
		}
		// The line is logged before the observer is told, so it is written inside the run's span.
		if outcome == semconv.JobError || outcome == semconv.JobPanic {
			s.log.LogAttrs(jobCtx, slog.LevelError, "scheduled job failed",
				slog.String(semconv.LogKind, t.name), slog.String(semconv.LogOutcome, outcome))
		}
		s.observeFinish(jobCtx, t.name, finish, outcome)
	}()
	if err := t.job(jobCtx); err != nil {
		outcome = semconv.JobError
		if ctx.Err() != nil {
			outcome = semconv.JobCancelled
		}
	}
}

// observeStart tells the observer a run is starting. An observer that panics or returns nothing
// usable is logged and ignored: telemetry must never stop a job or crash the service.
func (s *Scheduler) observeStart(ctx context.Context, job string) (out context.Context, finish func(string)) {
	out, finish = ctx, func(string) {}
	if s.obs == nil {
		return out, finish
	}
	defer func() {
		if r := recover(); r != nil {
			out, finish = ctx, func(string) {}
			s.observerFailed(ctx, job, "start")
		}
	}()
	c, f := s.obs.Start(ctx, job)
	if c != nil {
		out = c
	}
	if f != nil {
		finish = f
	}
	return out, finish
}

// observeFinish tells the observer how the run ended, surviving an observer that panics.
func (s *Scheduler) observeFinish(ctx context.Context, job string, finish func(string), outcome string) {
	defer func() {
		if r := recover(); r != nil {
			s.observerFailed(ctx, job, "finish")
		}
	}()
	finish(outcome)
}

// observerFailed logs that the observer panicked, by job and stage only: never the panic value.
func (s *Scheduler) observerFailed(ctx context.Context, job, stage string) {
	s.log.LogAttrs(ctx, slog.LevelError, "scheduler observer failed",
		slog.String(semconv.LogKind, job), slog.String(semconv.LogState, stage))
}

// nextDaily returns the first start of hour:minute in loc that is after t.
func nextDaily(t time.Time, hour, minute int, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	for i := range 3 {
		if c := dayInstant(y, m, d+i, hour, minute, loc); c.After(t) {
			return c
		}
	}
	panic(fmt.Sprintf("scheduler: no daily time after %v", t)) // unreachable: day+1 or day+2 is later
}

// dayInstant is the instant hour:minute is first shown on the given calendar day in loc.
func dayInstant(y int, m time.Month, d, hour, minute int, loc *time.Location) time.Time {
	wall := time.Date(y, m, d, hour, minute, 0, 0, time.UTC) // the wall time, read as UTC
	_, before := wall.Add(-24 * time.Hour).In(loc).Zone()
	_, after := wall.Add(24 * time.Hour).In(loc).Zone()
	var best time.Time
	for _, off := range []int{before, after} {
		inst := wall.Add(-time.Duration(off) * time.Second)
		l := inst.In(loc)
		if l.Hour() != hour || l.Minute() != minute || l.Day() != wall.Day() {
			continue
		}
		if best.IsZero() || inst.Before(best) {
			best = inst
		}
	}
	if best.IsZero() { // the time falls in a gap: use the offset in force before it
		return wall.Add(-time.Duration(before) * time.Second)
	}
	return best
}
