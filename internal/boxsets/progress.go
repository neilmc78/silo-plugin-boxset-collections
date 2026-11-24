package boxsets

import (
	"fmt"
	"time"
)

// A sync of a large library can take an hour, and the Last sync box is the
// only place Silo shows anything about it. So while a sync runs, the box is
// rewritten every progressEvery with where the sync is: its phase, the
// library, and how far through it is.
const progressEvery = 15 * time.Second

// progress is where a running sync is. done and total are 0 for a phase
// that doesn't count items.
type progress struct {
	phase   string
	library string
	done    int
	total   int
}

// setProgress starts a phase.
func (s *Syncer) setProgress(phase, library string, total int) {
	s.mu.Lock()
	s.prog = progress{phase: phase, library: library, total: total}
	s.mu.Unlock()
}

// stepProgress counts one item of the current phase done.
func (s *Syncer) stepProgress() {
	s.mu.Lock()
	s.prog.done++
	s.mu.Unlock()
}

// progressLine says where the running sync is, in one line.
func (s *Syncer) progressLine() string {
	s.mu.Lock()
	p := s.prog
	s.mu.Unlock()
	if p.phase == "" {
		return "Now: starting."
	}
	line := "Now: " + p.phase
	if p.library != "" {
		line += " in " + p.library
	}
	if p.total > 0 {
		line += fmt.Sprintf(", %d of %d (%d%%)", p.done, p.total, p.done*100/p.total)
	}
	return line + "."
}

// progressText is the Last sync box while a sync runs.
func progressText(header, now string, elapsed time.Duration) string {
	return fmt.Sprintf("%s\n\n%s\nRunning for %s. This box updates every %s; reload the page to see it.",
		header, now, elapsed.Round(time.Second), progressEvery)
}

// watchProgress rewrites the Last sync box on every tick until the
// returned stop is called. stop waits for the last write, so the final
// report written after it is never overwritten by a late tick.
func (s *Syncer) watchProgress(header string, start time.Time) (stop func()) {
	ticks, stopTicks := s.progressTicks()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-ticks:
				s.setStatus(progressText(header, s.progressLine(), s.now().Sub(start)))
			}
		}
	}()
	return func() {
		stopTicks()
		close(done)
		<-finished
	}
}

// defaultProgressTicks ticks every progressEvery.
func defaultProgressTicks() (<-chan time.Time, func()) {
	t := time.NewTicker(progressEvery)
	return t.C, t.Stop
}
