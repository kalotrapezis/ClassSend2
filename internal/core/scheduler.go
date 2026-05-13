package core

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ScheduledJob is one queued teacher-side command. The teacher process owns
// the timer; when it fires, the job's action is sent through SendCommand
// exactly as if the teacher had typed it at that moment. Jobs do not survive
// teacher restart — this is a stop-gap until the full schedule UI lands.
type ScheduledJob struct {
	ID         string    // short stable id ("S1", "S2", …) for cancel
	Action     string    // protocol command code
	Param      string    // for launch/op
	TargetID   string    // student id, or "" for all
	TargetText string    // human label e.g. "όλους" / "Μαρία (>3)"
	Label      string    // human label e.g. "🔒 Κλείδωμα"
	When       time.Time // absolute fire time
	// FollowUp, if set, is a second job to schedule when the first fires.
	// Used for lock-with-duration: lock now + unlock at when+duration.
	FollowUp *ScheduledJob
}

// Scheduler holds all currently-pending jobs and the goroutines that fire
// them. Safe for concurrent use from the TUI thread and the timer goroutines.
type Scheduler struct {
	mu     sync.Mutex
	jobs   map[string]*scheduledEntry
	nextID atomic.Uint64

	// Fire is what the scheduler calls when a job's time comes up. Set by
	// the wiring code (cmd/classsend/main.go) to the App.SendCommand path
	// plus a sys-message push. Decoupled so the scheduler doesn't depend on
	// the TUI package.
	Fire func(job ScheduledJob)
}

type scheduledEntry struct {
	job    ScheduledJob
	cancel chan struct{}
}

// NewScheduler constructs an empty scheduler. Caller must set Fire before
// adding jobs.
func NewScheduler() *Scheduler {
	return &Scheduler{jobs: make(map[string]*scheduledEntry)}
}

// Add queues a job. Returns the assigned ID. The job fires on its own
// goroutine when When elapses, unless Cancel is called first.
func (s *Scheduler) Add(job ScheduledJob) string {
	s.mu.Lock()
	id := fmt.Sprintf("S%d", s.nextID.Add(1))
	job.ID = id
	entry := &scheduledEntry{job: job, cancel: make(chan struct{})}
	s.jobs[id] = entry
	s.mu.Unlock()

	go s.run(entry)
	return id
}

func (s *Scheduler) run(entry *scheduledEntry) {
	wait := time.Until(entry.job.When)
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		// Remove from the map BEFORE firing so a Fire callback that tries to
		// schedule a follow-up doesn't see a stale entry.
		s.mu.Lock()
		delete(s.jobs, entry.job.ID)
		s.mu.Unlock()
		if s.Fire != nil {
			s.Fire(entry.job)
		}
		if entry.job.FollowUp != nil {
			s.Add(*entry.job.FollowUp)
		}
	case <-entry.cancel:
		// Already removed by Cancel.
	}
}

// Cancel removes a job by ID and stops its timer. Returns the cancelled job
// and true on success.
func (s *Scheduler) Cancel(id string) (ScheduledJob, bool) {
	s.mu.Lock()
	entry, ok := s.jobs[id]
	if !ok {
		s.mu.Unlock()
		return ScheduledJob{}, false
	}
	delete(s.jobs, id)
	s.mu.Unlock()
	close(entry.cancel)
	return entry.job, true
}

// List returns all pending jobs sorted by When ascending.
func (s *Scheduler) List() []ScheduledJob {
	s.mu.Lock()
	out := make([]ScheduledJob, 0, len(s.jobs))
	for _, e := range s.jobs {
		out = append(out, e.job)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].When.Before(out[j].When) })
	return out
}

// ── Time-clause parser ──────────────────────────────────────────────────────

// ScheduledWhen describes how a `| <when>` clause resolves. Either Absolute is
// set (a future time) and DurationMin is 0, or DurationMin is set (relative)
// and Absolute is the computed fire time. RollOver is true when an HH:MM in
// the past was rolled to tomorrow (caller should warn + confirm).
type ScheduledWhen struct {
	Absolute    time.Time
	DurationMin int
	IsDuration  bool // true when source was ":NN" form; for `lock` this means "lock for NN min"
	RollOver    bool // HH:MM was already past today
}

// ParseWhen interprets the right-hand side of `|` per the documented syntax:
//
//	HH:MM   → absolute clock time today; rolls to tomorrow if past.
//	:NN     → NN minutes from now (or, for lock-family actions, "lock for NN min" —
//	          interpreted by the caller; we set IsDuration=true).
//
// Returns an error on malformed input.
func ParseWhen(raw string, now time.Time) (ScheduledWhen, error) {
	raw = trimSpaces(raw)
	if raw == "" {
		return ScheduledWhen{}, fmt.Errorf("κενή ώρα μετά το |")
	}
	if raw[0] == ':' {
		var mins int
		if _, err := fmt.Sscanf(raw[1:], "%d", &mins); err != nil || mins <= 0 {
			return ScheduledWhen{}, fmt.Errorf("άκυρο διάστημα %q (περίμενα :λεπτά, π.χ. :15)", raw)
		}
		return ScheduledWhen{
			Absolute:    now.Add(time.Duration(mins) * time.Minute),
			DurationMin: mins,
			IsDuration:  true,
		}, nil
	}
	// HH:MM
	var hh, mm int
	if _, err := fmt.Sscanf(raw, "%d:%d", &hh, &mm); err != nil {
		return ScheduledWhen{}, fmt.Errorf("άκυρη ώρα %q (περίμενα HH:MM ή :λεπτά)", raw)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return ScheduledWhen{}, fmt.Errorf("άκυρη ώρα %02d:%02d", hh, mm)
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	rollover := false
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
		rollover = true
	}
	return ScheduledWhen{Absolute: target, RollOver: rollover}, nil
}

// trimSpaces is strings.TrimSpace inlined to avoid an import cycle of
// "strings" from this file. It's fine as-is, but keeping local makes the
// scheduler standalone.
func trimSpaces(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
