// Package cron schedules recurring agent tasks. The loop sleeps until the
// next due job (no fixed tick) and wakes on job changes.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/adhocore/gronx"
)

type Job struct {
	ID       string    `json:"id"`
	Schedule string    `json:"schedule"` // cron expression
	Message  string    `json:"message"`  // prompt run as an agent turn
	Channel  string    `json:"channel"`  // where to deliver the result
	ChatID   string    `json:"chat_id"`
	Enabled  bool      `json:"enabled"`
	LastRun  time.Time `json:"last_run,omitzero"`
}

// Handler runs one due job and returns the text to deliver ("" = silent).
type Handler func(ctx context.Context, job Job) (string, error)

// Deliver sends a job result to its channel/chat.
type Deliver func(channel, chatID, content string)

type Service struct {
	path    string
	handler Handler
	deliver Deliver

	mu     sync.Mutex
	jobs   map[string]*Job
	seq    int
	notify chan struct{}
	now    func() time.Time
	wg     sync.WaitGroup
}

func NewService(dir string, handler Handler, deliver Deliver) (*Service, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Service{
		path:    filepath.Join(dir, "jobs.json"),
		handler: handler,
		deliver: deliver,
		jobs:    map[string]*Job{},
		notify:  make(chan struct{}, 1),
		now:     time.Now,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load replaces the in-memory jobs with what is on disk. jobs.json is shared
// by every factor process on the machine — a `factor chat` session adds jobs
// to the same file the gateway schedules from — so disk is the truth and this
// map is only a cache of it. A file that is not there yet leaves the cache
// alone: that is a store nobody has written, not a store somebody emptied.
//
// The caller holds mu.
func (s *Service) load() error {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored struct {
		Seq  int    `json:"seq"`
		Jobs []*Job `json:"jobs"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	s.seq = stored.Seq
	s.jobs = make(map[string]*Job, len(stored.Jobs))
	for _, j := range stored.Jobs {
		s.jobs[j.ID] = j
	}
	return nil
}

// reload picks up whatever another process has written since this one last
// looked. A read that fails is reported and ignored: a scheduler that stopped
// running the jobs it already holds because a stat went wrong would trade a
// small problem for the one this whole file exists to prevent.
//
// The caller holds mu.
func (s *Service) reload() {
	if err := s.load(); err != nil {
		slog.Warn("cron store could not be re-read; scheduling from the last good copy",
			"path", s.path, "error", err)
	}
}

func (s *Service) save() error {
	stored := struct {
		Seq  int    `json:"seq"`
		Jobs []*Job `json:"jobs"`
	}{Seq: s.seq, Jobs: s.list()}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Service) list() []*Job {
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// List returns a snapshot of all jobs, including any another process has
// added since this one started.
func (s *Service) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.list() {
		out = append(out, *j)
	}
	return out
}

// Add validates and persists a new job, waking the scheduler.
func (s *Service) Add(schedule, message, channelName, chatID string) (Job, error) {
	if !gronx.New().IsValid(schedule) {
		return Job{}, fmt.Errorf("invalid cron expression %q", schedule)
	}
	if message == "" {
		return Job{}, fmt.Errorf("message is required")
	}
	// Syntax is not enough: "0 12 31 2 *" parses and never comes round, and a
	// job that can never run is worse than a rejected one, because it is
	// reported as scheduled.
	if _, err := gronx.NextTickAfter(schedule, s.now(), false); err != nil {
		return Job{}, fmt.Errorf("cron expression %q has no next occurrence", schedule)
	}
	s.mu.Lock()
	// Another process may have added jobs since this one last looked; adding
	// on top of a stale map reuses their ids and saves them away.
	s.reload()
	s.seq++
	job := &Job{
		ID:       fmt.Sprintf("cron-%d", s.seq),
		Schedule: schedule,
		Message:  message,
		Channel:  channelName,
		ChatID:   chatID,
		Enabled:  true,
		// Stamped now, not left zero: the scheduler fires anything overdue
		// the moment it wakes, and a job whose last run is unknown counts the
		// minute before it was created as missed.
		LastRun: s.now(),
	}
	s.jobs[job.ID] = job
	err := s.save()
	if err != nil {
		// Roll back so memory never disagrees with disk.
		delete(s.jobs, job.ID)
		s.seq--
	}
	// Copied under the lock: once woken, the scheduler writes LastRun on the
	// stored job, and a dereference after Unlock races it.
	out := *job
	s.mu.Unlock()
	if err != nil {
		return Job{}, err
	}
	s.wake()
	return out, nil
}

// Remove deletes a job.
func (s *Service) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	job, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("no cron job %q", id)
	}
	delete(s.jobs, id)
	if err := s.save(); err != nil {
		s.jobs[id] = job // roll back: the job is still on disk
		return err
	}
	s.wake()
	return nil
}

// SetEnabled toggles a job.
func (s *Service) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
	j, ok := s.jobs[id]
	if !ok {
		return fmt.Errorf("no cron job %q", id)
	}
	previous := j.Enabled
	j.Enabled = enabled
	if err := s.save(); err != nil {
		j.Enabled = previous // roll back to what is still on disk
		return err
	}
	s.wake()
	return nil
}

// nextRun reports when a job will next fire, and whether it ever will. It is
// what the cron tool answers with: a schedule the model wrote for a date that
// has already gone by this year is indistinguishable from a correct one until
// somebody says out loud that the next run is eleven months away.
func (s *Service) nextRun(job Job) (time.Time, bool) {
	tick, err := gronx.NextTickAfter(job.Schedule, s.now(), false)
	if err != nil {
		return time.Time{}, false
	}
	return tick, true
}

func (s *Service) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// nextWake returns the earliest next tick among enabled jobs.
func (s *Service) nextWake() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var next time.Time
	found := false
	for _, j := range s.jobs {
		if !j.Enabled {
			continue
		}
		tick, err := gronx.NextTickAfter(j.Schedule, s.now(), false)
		if err != nil {
			continue
		}
		if !found || tick.Before(next) {
			next, found = tick, true
		}
	}
	return next, found
}

// dueJobs returns enabled jobs due at or before now, marking LastRun.
func (s *Service) dueJobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var due []Job
	for _, j := range s.jobs {
		if !j.Enabled {
			continue
		}
		ref := j.LastRun
		if ref.IsZero() {
			ref = now.Add(-time.Minute)
		}
		tick, err := gronx.NextTickAfter(j.Schedule, ref, false)
		if err != nil {
			continue
		}
		if !tick.After(now) {
			j.LastRun = now
			due = append(due, *j)
		}
	}
	if len(due) > 0 {
		if err := s.save(); err != nil {
			slog.Warn("cron save failed", "error", err)
		}
	}
	return due
}

// maxSleep bounds how long the scheduler parks between checks. Two things
// change without waking it: another factor process editing jobs.json, and the
// wall clock itself — a suspended laptop resumes with a timer that still
// thinks it has an hour to run. Neither is worth a timer, and a check twice a
// minute costs one read of a small file.
var maxSleep = 30 * time.Second

// Run is the scheduler loop; it blocks until ctx is done.
func (s *Service) Run(ctx context.Context) {
	defer s.wg.Wait()
	for ctx.Err() == nil {
		// Overdue jobs run before this loop parks again, which is what makes
		// a reminder survive its process. A tick that fell while the gateway
		// was restarting — for a config change, an upgrade, a reboot — is
		// otherwise skipped until the schedule next comes round: a day later
		// for a daily job, and for the dated expression a model writes for
		// "remind me on the 5th", a year.
		s.reloadStore()
		for _, job := range s.dueJobs() {
			s.wg.Add(1)
			go func(job Job) {
				defer s.wg.Done()
				s.runJob(ctx, job)
			}(job)
		}

		d := maxSleep
		if next, found := s.nextWake(); found {
			if until := time.Until(next); until < d {
				d = until
			}
		}
		if d < 0 {
			d = 0
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.notify:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// reloadStore re-reads jobs.json under the lock, so the scheduling decisions
// that follow see what every other factor process has written.
func (s *Service) reloadStore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reload()
}

func (s *Service) runJob(ctx context.Context, job Job) {
	slog.Info("cron job firing", "id", job.ID)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	result, err := s.handler(ctx, job)
	if err != nil {
		slog.Error("cron job failed", "id", job.ID, "error", err)
		result = fmt.Sprintf("Scheduled task %s failed: %v", job.ID, err)
	}
	if result != "" && job.Channel != "" && s.deliver != nil {
		s.deliver(job.Channel, job.ChatID, result)
	}
}
