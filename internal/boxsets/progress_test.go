package boxsets

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
)

// While a sync runs, each tick rewrites the Last sync box with the phase,
// library and count; the final report replaces it when the sync ends.
func TestProgressShowsWhereTheSyncIs(t *testing.T) {
	s, api, host := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.boxSets["20"] = silo.BoxSet{ID: "20", Title: "Heat"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10", "m3": "20", "m4": "20", "m5": ""}

	ticks := make(chan time.Time)
	s.progressTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	host.statusCh = make(chan string, 16)

	// Hold the sync at its third lookup, then tick.
	reached, release := make(chan struct{}), make(chan struct{})
	api.onLookup = func(cid string) {
		if cid == "m3" {
			close(reached)
			<-release
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.syncAndRecord(context.Background(), true)
	}()

	if first := <-host.statusCh; !strings.Contains(first, "Sync started") || !strings.Contains(first, "Now: starting.") {
		t.Fatalf("start status = %q", first)
	}
	<-reached
	ticks <- time.Now()
	progress := <-host.statusCh
	// m1's lookup answers for m2 too, so m3 is the third movie of five.
	for _, want := range []string{"Now: Movies: looking up box sets in Movies, 3 of 5 (60%).", "Running for", "updates every 15s"} {
		if !strings.Contains(progress, want) {
			t.Fatalf("progress status lacks %q:\n%s", want, progress)
		}
	}
	close(release)
	<-finished

	if last := host.status[len(host.status)-1]; !strings.HasPrefix(last, "Sync finished") {
		t.Fatalf("final status = %q, want the finished report", last)
	}
}

// A tick that arrives after the sync stops must not overwrite the report.
func TestStoppedProgressWritesNothingMore(t *testing.T) {
	s, _, host := setup(t)
	ticks := make(chan time.Time, 1)
	s.progressTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	stop := s.watchProgress("header", time.Now())
	stop()
	before := len(host.status)
	ticks <- time.Now()
	s.setStatus("final")
	if len(host.status) != before+1 || host.status[len(host.status)-1] != "final" {
		t.Fatalf("status writes after stop: %v", host.status[before:])
	}
}
