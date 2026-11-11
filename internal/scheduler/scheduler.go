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

// Scheduler runs jobs registered with Every and Daily once Run is called.
type Scheduler struct {
	now func() time.Time
	log *slog.Logger

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

// New returns a Scheduler that reads the time from now and logs to log.
func New(now func() time.Time, log *slog.Logger) *Scheduler {
	return &Scheduler{
		now:   now,
		log:   log,
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
// first occurrence only.
func (s *Scheduler) Daily(name string, hour, minute int, loc *time.Location, job Job) {
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 || loc == nil {
		panic("scheduler: Daily needs a valid time of day and a location")
	}
	s.add(task{
		name:  name,
		job:   job,
		first: func(now time.Time) time.Duration { return nextDaily(now, hour, minute, loc).Sub(now) },
		next: func(start, end time.Time) time.Duration {
			return max(0, nextDaily(start, hour, minute, loc).Sub(end))
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
func (s *Scheduler) runJob(ctx context.Context, t task) {
	outcome := "ok"
	defer func() {
		if r := recover(); r != nil {
			outcome = "panic"
		}
		if outcome != "ok" {
			s.log.LogAttrs(ctx, slog.LevelError, "scheduled job failed",
				slog.String(semconv.LogKind, t.name), slog.String(semconv.LogOutcome, outcome))
		}
	}()
	if err := t.job(ctx); err != nil {
		outcome = "error"
	}
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
