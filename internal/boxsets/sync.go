// Package boxsets keeps one Silo library collection per box set: a
// collection in a "Box Sets" group with the box set's title, overview,
// artwork and movies, all read from what Silo already stores. Nothing here
// calls TMDB.
package boxsets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
	"github.com/neilmc78/silo-plugin-boxset-collections/internal/tvdb"
)

// Silo events that can change which movies a library has or which box set a
// movie belongs to.
const (
	EventScanComplete    = "scan_complete"
	EventMetadataUpdated = "metadata_updated"
)

// Slugs mark what the plugin owns. They match what the box set collection
// feature in core used, so collections it created are adopted, not
// duplicated.
const (
	collectionSlugPrefix = "box-set-"
	groupSlug            = "box-sets"
)

// massRemovalFloor is how many collections a run may always remove; above
// it, a run that would remove more than half a library's box set
// collections removes none.
const massRemovalFloor = 5

// eventDelay gathers the events of one scan (or a burst of metadata
// refreshes) into one sync.
var eventDelay = 20 * time.Second

// Library is a movie library the host reports.
type Library struct {
	ID   string
	Name string
}

// Host is what the plugin needs from Silo's plugin host.
type Host interface {
	// BaseURL is the address the plugin reaches Silo's API on.
	BaseURL(ctx context.Context) (string, error)
	// MovieLibraries lists libraries that can hold movies.
	MovieLibraries(ctx context.Context) ([]Library, error)
	// SeriesLibraries lists libraries that can hold series.
	SeriesLibraries(ctx context.Context) ([]Library, error)
	// SaveSettings replaces the plugin's settings entry, which Silo stores
	// encrypted the same way as an admin's save.
	SaveSettings(ctx context.Context, values map[string]any) error
	// SetStatus shows a sync's report in the plugin's "Last sync" settings
	// box, the one place in Silo's UI a plugin can write to.
	SetStatus(ctx context.Context, report string) error
	ReadState(ctx context.Context, key string) ([]byte, bool, error)
	WriteState(ctx context.Context, key string, value []byte) error
}

// API is the part of Silo's REST API the sync uses; *silo.Client is it.
type API interface {
	PrimaryProfile(ctx context.Context) (silo.Profile, error)
	UseProfile(id string)
	Movies(ctx context.Context, libraryID string) ([]string, error)
	Series(ctx context.Context, libraryID string) ([]silo.SeriesRef, error)
	ItemBoxSet(ctx context.Context, contentID, libraryID string) (*silo.BoxSet, error)
	BoxSet(ctx context.Context, id, libraryID string) (silo.BoxSet, error)
	Collections(ctx context.Context, libraryID string) ([]silo.Collection, error)
	Groups(ctx context.Context, libraryID string) ([]silo.Group, error)
	CreateGroup(ctx context.Context, libraryID, name, slug string) (silo.Group, error)
	RenameGroup(ctx context.Context, id, name string) error
	CreateCollection(ctx context.Context, n silo.NewCollection) (silo.Collection, error)
	UpdateCollection(ctx context.Context, id, title, description string) error
	DeleteCollection(ctx context.Context, id string) error
	Members(ctx context.Context, collectionID string) ([]string, error)
	AddMember(ctx context.Context, collectionID, contentID string, position int) error
	RemoveMember(ctx context.Context, collectionID, contentID string) error
	OrderMembers(ctx context.Context, collectionID string, contentIDs []string) error
	Fetch(ctx context.Context, address string) ([]byte, string, error)
	UploadArtwork(ctx context.Context, collectionID, kind string, image []byte, contentType string) (silo.Collection, error)
	SetArtworkSource(ctx context.Context, collectionID, kind, sourceURL string) (silo.Collection, error)
}

// Syncer is the plugin: a scheduled task (a full sync) and an event
// consumer (a quick sync after scans and metadata refreshes).
type Syncer struct {
	pluginv1.UnimplementedScheduledTaskServer
	pluginv1.UnimplementedEventConsumerServer

	// Version is the plugin's version, shown in the sync status.
	Version string

	log    *slog.Logger
	host   Host
	newAPI func(baseURL, apiKey string) API
	// newTVDB builds the TheTVDB client for a key and PIN.
	newTVDB func(apiKey, pin string) TVDB
	// tvdb is kept across syncs so its login token is reused.
	tvdb    TVDB
	tvdbFor string
	now     func() time.Time

	mu  sync.Mutex
	cfg Config

	runMu sync.Mutex // one sync at a time
	// state is the running sync's shared state; set under runMu.
	state *syncState
	// lastStatus is the Last sync text last written; guarded by mu.
	lastStatus string

	// running is set while a Run's background sync is going; wg lets tests
	// wait for it.
	running atomic.Bool
	wg      sync.WaitGroup

	pending sync.Mutex // guards timer and stale
	timer   *time.Timer
	// stale holds movies a metadata refresh touched since the last sync;
	// the next sync looks them up again whatever the cache says.
	stale map[string]bool
}

// New returns a Syncer. newAPI builds the REST client; nil uses *silo.Client.
func New(log *slog.Logger, host Host, newAPI func(baseURL, apiKey string) API) *Syncer {
	if newAPI == nil {
		newAPI = func(baseURL, apiKey string) API { return silo.New(baseURL, apiKey) }
	}
	return &Syncer{
		log: log, host: host, newAPI: newAPI, stale: map[string]bool{}, now: time.Now,
		newTVDB: func(apiKey, pin string) TVDB { return tvdb.New("", apiKey, pin) },
	}
}

// Configure stores the settings the host sends.
func (s *Syncer) Configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	cfg, err := ParseConfig(entries)
	if err != nil {
		return err
	}
	// Whether a TheTVDB key arrived, never the key itself.
	s.log.Info("box set collections: configured", "tv_franchises", cfg.TVFranchises, "tvdb_key", cfg.TVDBAPIKey != "",
		"series_group", cfg.SeriesGroupName, "series_min_shows", cfg.SeriesMinShows)
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return nil
}

func (s *Syncer) config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Run is the scheduled task: a full sync that looks every movie up again,
// so drift the events missed is corrected. Silo gives the call 10 seconds
// (its plugin control timeout), far less than a sync of a large library
// takes, so Run starts the sync in the background and answers at once. Its
// output carries the previous sync's report; each sync's own report is
// logged and saved when it finishes.
func (s *Syncer) Run(ctx context.Context, _ *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	out := map[string]any{"status": "started"}
	if last := s.lastReport(ctx); last != nil {
		out["last_run"] = last
	}
	if !s.running.CompareAndSwap(false, true) {
		out["status"] = "already running"
		s.mu.Lock()
		current := s.lastStatus
		s.mu.Unlock()
		s.setStatus(current + fmt.Sprintf("\n\nRun now at %s: this sync is still running. This box updates when it finishes.",
			time.Now().UTC().Format(time.RFC3339)))
	} else {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.running.Store(false)
			bg, cancel := context.WithTimeout(context.Background(), syncTimeout)
			defer cancel()
			s.syncAndRecord(bg, true)
		}()
	}
	output, err := structpb.NewStruct(out)
	if err != nil {
		return nil, err
	}
	// Silo shows a plugin task's error but not its output, so a problem
	// the previous sync met is reported as this run's error. The new sync
	// has started all the same.
	if problem := problemSummary(out["last_run"], s.config().SeriesEnabled()); problem != "" {
		return nil, errors.New(problem)
	}
	return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
}

// problemSummary describes, in one line for Silo's task list, what went
// wrong in a saved sync report, or "" when nothing did. tvdbKey says whether
// series franchises are switched on.
func problemSummary(last any, tvdbKey bool) string {
	r, ok := last.(map[string]any)
	if !ok {
		return ""
	}
	num := func(key string) int {
		v, _ := r[key].(float64)
		return int(v)
	}
	first := ""
	if e, _ := r["error"].(string); e != "" {
		first = e
	} else if errs, _ := r["errors"].([]any); len(errs) > 0 {
		first, _ = errs[0].(string)
	}
	_, hasSeries := r["series_libraries"]
	var cause string
	switch {
	case first != "":
		cause = fmt.Sprintf("%d problem(s). First: %s", max(num("error_count"), 1), first)
	case tvdbKey && hasSeries && num("series_libraries") == 0:
		cause = "a TheTVDB key is set, but no series library was synced"
	case tvdbKey && !hasSeries:
		cause = "a TheTVDB key is set, but that sync did not check series libraries; it may have run before the key was saved, so run the task again"
	case tvdbKey && num("franchises") == 0 && num("official_lists") == 0:
		cause = fmt.Sprintf("TheTVDB has no official list for any of your series (%d unofficial lists ignored)", num("unofficial_lists"))
	case tvdbKey && num("franchises") == 0:
		cause = "no official list has enough of your series"
		if n := num("lists_never_form"); n > 0 {
			cause += fmt.Sprintf(" (%d have too few series in all to ever form)", n)
		}
		if misses, _ := r["near_misses"].([]any); len(misses) > 0 {
			closest := make([]string, 0, len(misses))
			for _, m := range misses {
				if s, ok := m.(string); ok {
					closest = append(closest, s)
				}
			}
			cause += ". Closest: " + strings.Join(closest[:min(len(closest), 3)], "; ")
		}
	default:
		return ""
	}
	summary := fmt.Sprintf("last sync: %d box sets", num("box_sets"))
	if tvdbKey && hasSeries {
		summary += fmt.Sprintf("; %d series libraries, %d series looked up, %d without a TheTVDB ID, %d official lists (%d unofficial, %d too small), %d franchises",
			num("series_libraries"), num("series_looked_up"), num("series_without_tvdb_id"),
			num("official_lists"), num("unofficial_lists"), num("lists_too_small"), num("franchises"))
	}
	return summary + ". " + cause
}

// syncTimeout bounds one background sync.
const syncTimeout = time.Hour

const lastReportKey = "last_report"

// syncAndRecord runs a sync, then logs and saves its report for the next
// Run's output.
func (s *Syncer) syncAndRecord(ctx context.Context, full bool) {
	start := time.Now()
	cfg := s.config()
	s.setStatus(fmt.Sprintf("Sync started %s (%s), plugin v%s, TV franchises: %s, TheTVDB key: %s.\nIf this stays for more than an hour, the sync stopped before finishing.",
		start.UTC().Format(time.RFC3339), runKind(full), s.Version, onOff(cfg.TVFranchises), presence(cfg.TVDBAPIKey != "")))
	report, err := s.Sync(ctx, full)
	s.setStatus(statusText(report, err, cfg, full, start, time.Now(), s.Version))
	record := report.Map()
	record["full"] = full
	record["finished_at"] = time.Now().UTC().Format(time.RFC3339)
	record["duration_seconds"] = int(time.Since(start).Seconds())
	if err != nil {
		record["error"] = err.Error()
		s.log.Warn("box set collections: sync failed", "error", err, "full", full)
	} else if len(report.Errors) > 0 {
		s.log.Warn("box set collections: sync finished with problems", "problems", len(report.Errors), "first", report.Errors[0])
	}
	raw, mErr := json.Marshal(record)
	if mErr == nil {
		// The call's context may be spent; the record still matters.
		wctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if wErr := s.host.WriteState(wctx, lastReportKey, raw); wErr != nil {
			s.log.Warn("box set collections: could not save the sync report", "error", wErr)
		}
	}
}

// setStatus writes the plugin's "Last sync" box. A failure is logged; it
// never fails the sync.
func (s *Syncer) setStatus(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.mu.Lock()
	s.lastStatus = text
	s.mu.Unlock()
	if err := s.host.SetStatus(ctx, text); err != nil {
		s.log.Warn("box set collections: could not write the sync status", "error", err)
	}
}

func runKind(full bool) string {
	if full {
		return "full"
	}
	return "quick, after a library change"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func presence(ok bool) string {
	if ok {
		return "present"
	}
	return "missing"
}

// statusText is a sync's report as plain text, one fact per line, for the
// plugin's "Last sync" box.
func statusText(r Report, err error, cfg Config, full bool, start, end time.Time, version string) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("Sync finished %s (%s, %s), plugin v%s.", end.UTC().Format(time.RFC3339), runKind(full), end.Sub(start).Round(time.Second), version)
	if err != nil {
		line("It stopped with an error: %v", err)
	}
	line("")
	line("Movies: %d libraries, %d box sets, %d movies looked up.", r.Libraries, r.BoxSets, r.Looked)
	line("")
	switch {
	case !cfg.TVFranchises:
		line("TV franchises: off in the plugin's settings.")
	case cfg.TVDBAPIKey == "":
		line("TV franchises: skipped, because the settings the plugin received have no TheTVDB API key.")
	default:
		line("TV franchises: %d series libraries synced.", r.SeriesLibraries)
		line("- series without a TheTVDB ID in Silo: %d", r.SeriesWithoutTVDB)
		line("- series looked up on TheTVDB this sync: %d (others came from the week-long cache)", r.SeriesLooked)
		line("- official lists your series are on: %d (unofficial lists ignored: %d)", r.ListsOfficial, r.ListsUnofficial)
		line("- lists with too few of your series: %d (needs %d)", r.ListsTooSmall, max(cfg.SeriesMinShows, 2))
		line("- lists with fewer than %d series in all, which can never form: %d", max(cfg.SeriesMinShows, 2), r.ListsNeverForm)
		line("- franchises: %d", r.Franchises)
		if len(r.NearMisses) > 0 {
			line("Closest lists that did not form a franchise:")
			for _, n := range r.NearMisses {
				line("- %s", n)
			}
		}
	}
	line("")
	line("Collections created %d, updated %d, removed %d.", r.Created, r.Updated, r.Removed)
	if len(r.Skipped) > 0 {
		line("")
		line("Not made because you deleted or excluded them (%d):", len(r.Skipped))
		for _, n := range r.Skipped[:min(len(r.Skipped), 20)] {
			line("- %s", n)
		}
	}
	if len(r.ArtworkFailures) > 0 {
		line("")
		line("Artwork that could not be set, tried again next sync (%d):", len(r.ArtworkFailures))
		for _, a := range r.ArtworkFailures[:min(len(r.ArtworkFailures), 5)] {
			line("- %s", a)
		}
	}
	if len(r.Errors) > 0 {
		line("")
		line("Problems (%d):", len(r.Errors))
		for _, e := range r.Errors[:min(len(r.Errors), 10)] {
			line("- %s", e)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (s *Syncer) lastReport(ctx context.Context) map[string]any {
	raw, ok, err := s.host.ReadState(ctx, lastReportKey)
	if err != nil || !ok {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// HandleEvent answers at once (the host allows a few seconds) and syncs in
// the background a little later, so a scan's burst of events costs one sync.
func (s *Syncer) HandleEvent(ctx context.Context, req *pluginv1.HandleEventRequest) (*pluginv1.HandleEventResponse, error) {
	switch req.GetEventName() {
	case EventMetadataUpdated:
		// A refreshed movie (or every movie of a refreshed library) may have
		// joined or left a box set, so the next sync looks it up again.
		if id := payloadID(req.GetPayload()); id != "" {
			s.pending.Lock()
			s.stale[id] = true
			s.pending.Unlock()
		}
	case EventScanComplete:
	default:
		return &pluginv1.HandleEventResponse{}, nil
	}
	s.schedule()
	return &pluginv1.HandleEventResponse{}, nil
}

func (s *Syncer) schedule() {
	s.pending.Lock()
	defer s.pending.Unlock()
	if s.timer != nil {
		s.timer.Reset(eventDelay)
		return
	}
	s.timer = time.AfterFunc(eventDelay, func() {
		s.pending.Lock()
		s.timer = nil
		s.pending.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
		defer cancel()
		s.syncAndRecord(ctx, false)
	})
}

func payloadID(p *structpb.Struct) string {
	if p == nil {
		return ""
	}
	// Silo hands bus events over as {"raw": "<id>"}: a movie's content ID
	// after an item refresh, a library ID after a library refresh.
	for _, key := range []string{"raw", "content_id", "contentId", "id", "value"} {
		if v := strings.TrimSpace(p.GetFields()[key].GetStringValue()); v != "" {
			return v
		}
	}
	return ""
}

// Report says what a sync did.
type Report struct {
	Libraries int
	Looked    int
	BoxSets   int
	// The series counts stay zero while no TheTVDB key is set.
	SeriesLibraries   int
	SeriesLooked      int
	Franchises        int
	SeriesWithoutTVDB int
	ListsOfficial     int
	ListsUnofficial   int
	ListsTooSmall     int
	// ListsNeverForm are official lists with fewer series in all than the
	// minimum, which no library can make into a franchise.
	ListsNeverForm int
	// ArtworkFailures are images the sync could not set; they are tried
	// again next sync.
	ArtworkFailures []string
	// Skipped are collections not made because the admin deleted or
	// excluded them.
	Skipped []string
	// NearMisses are the official lists closest to forming a franchise.
	NearMisses []NearMiss
	Created    int
	Updated    int
	Removed    int
	Errors     []string
}

// Map is the report as a scheduled task's output.
func (r Report) Map() map[string]any {
	// The first problems are enough to act on; the count says how many.
	shown := r.Errors[:min(len(r.Errors), 20)]
	errs := make([]any, len(shown))
	for i, e := range shown {
		errs[i] = e
	}
	return map[string]any{
		"libraries": r.Libraries, "movies_looked_up": r.Looked, "box_sets": r.BoxSets,
		"series_libraries": r.SeriesLibraries, "series_looked_up": r.SeriesLooked, "franchises": r.Franchises,
		"series_without_tvdb_id": r.SeriesWithoutTVDB, "official_lists": r.ListsOfficial,
		"unofficial_lists": r.ListsUnofficial, "lists_too_small": r.ListsTooSmall, "lists_never_form": r.ListsNeverForm, "near_misses": nearMisses(r.NearMisses),
		"created": r.Created, "updated": r.Updated, "removed": r.Removed, "errors": errs, "error_count": len(r.Errors),
	}
}

func nearMisses(ns []NearMiss) []any {
	out := make([]any, len(ns))
	for i, n := range ns {
		out[i] = n.String()
	}
	return out
}

// Sync brings every movie library's box set collections in line with its
// box sets. full looks every movie up again; otherwise only movies not seen
// before (or dropped from the cache by an event) are looked up.
func (s *Syncer) Sync(ctx context.Context, full bool) (Report, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	var report Report
	cfg := s.config()
	if cfg.APIKey == "" {
		return report, errors.New("set an API key in the plugin's settings first")
	}
	base := cfg.SiloURL
	if base == "" {
		var err error
		if base, err = s.host.BaseURL(ctx); err != nil {
			return report, fmt.Errorf("finding Silo's address: %w", err)
		}
	}
	api := s.newAPI(base, cfg.APIKey)
	profile, err := api.PrimaryProfile(ctx)
	if err != nil {
		return report, fmt.Errorf("reading the API key's profiles: %w", err)
	}
	api.UseProfile(profile.ID)

	st, err := s.loadState(ctx)
	if err != nil {
		return report, fmt.Errorf("reading the plugin's saved state: %w", err)
	}
	s.state = st
	defer func() {
		// The sync's context may be spent; what it learned still counts.
		sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := s.saveExclusions(sctx, st.excluding, st.excludingBoxSets); err != nil {
			report.Errors = append(report.Errors, "adding deleted collections to the excluded settings: "+err.Error())
		}
		if err := s.saveState(sctx, st); err != nil {
			report.Errors = append(report.Errors, err.Error())
			s.log.Warn("box set collections: could not save state", "error", err)
		}
		report.ArtworkFailures = st.artFailures
		s.state = nil
	}()

	var libraries []Library
	if cfg.MovieBoxSets {
		if libraries, err = s.host.MovieLibraries(ctx); err != nil {
			return report, fmt.Errorf("listing libraries: %w", err)
		}
	}
	s.pending.Lock()
	stale := s.stale
	s.stale = map[string]bool{}
	s.pending.Unlock()
	for _, lib := range libraries {
		if err := s.syncLibrary(ctx, api, cfg, lib, full, stale, &report); err != nil {
			if ctx.Err() != nil {
				s.pending.Lock()
				for id := range stale {
					s.stale[id] = true
				}
				s.pending.Unlock()
				return report, ctx.Err()
			}
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", lib.Name, err))
			s.log.Warn("box set collections: library sync failed", "library", lib.Name, "error", err)
			continue
		}
		report.Libraries++
	}
	if cfg.SeriesEnabled() {
		if err := s.syncSeries(ctx, api, cfg, full, stale, &report); err != nil {
			s.pending.Lock()
			for id := range stale {
				s.stale[id] = true
			}
			s.pending.Unlock()
			return report, err
		}
	}
	s.log.Info("box set collections: synced", "libraries", report.Libraries, "box_sets", report.BoxSets,
		"series_libraries", report.SeriesLibraries, "franchises", report.Franchises,
		"created", report.Created, "updated", report.Updated, "removed", report.Removed, "looked_up", report.Looked)
	if len(report.Errors) > 0 {
		// A library that failed may not have looked its refreshed movies up;
		// keep them for the next sync.
		s.pending.Lock()
		for id := range stale {
			s.stale[id] = true
		}
		s.pending.Unlock()
	}
	if len(report.Errors) > 0 && report.Libraries+report.SeriesLibraries == 0 {
		return report, errors.New(strings.Join(report.Errors, "; "))
	}
	return report, nil
}

func (s *Syncer) syncLibrary(ctx context.Context, api API, cfg Config, lib Library, full bool, stale map[string]bool, report *Report) error {
	movies, err := api.Movies(ctx, lib.ID)
	if err != nil {
		return fmt.Errorf("listing movies: %w", err)
	}
	inLibrary := make(map[string]bool, len(movies))
	for _, id := range movies {
		inLibrary[id] = true
	}

	// Which box set each movie is in. A lookup answers with the whole box
	// set and its movies, so its siblings need no lookup of their own, and
	// the box sets found this way need no second read.
	cache, err := s.loadCache(ctx, movieCache, lib.ID)
	if err != nil {
		return err
	}
	next := make(map[string]string, len(movies))
	found := map[string]silo.BoxSet{}
	// failed counts lookups and reads that failed this run. Any failure
	// means some box sets are unknown rather than gone, so nothing is
	// removed (see below).
	failed := 0
	for _, id := range movies {
		if _, done := next[id]; done {
			continue
		}
		if boxSet, ok := cache[id]; ok && !full && !stale[id] && !stale[lib.ID] {
			next[id] = boxSet
			continue
		}
		b, err := api.ItemBoxSet(ctx, id, lib.ID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Keep what was known and try again next run.
			if boxSet, ok := cache[id]; ok {
				next[id] = boxSet
			}
			failed++
			report.addError(lib, "looking up %s: %v", id, err)
			continue
		}
		report.Looked++
		if report.Looked%cacheSaveEvery == 0 {
			// Keep progress, so a sync stopped part way resumes from here.
			partial := maps.Clone(cache)
			maps.Copy(partial, next)
			if err := s.saveCache(ctx, movieCache, lib.ID, partial); err != nil {
				return err
			}
		}
		if b == nil {
			next[id] = ""
			continue
		}
		next[id] = b.ID
		for _, m := range b.Members {
			if inLibrary[m] {
				next[m] = b.ID
			}
		}
		if len(b.Members) > 0 {
			found[b.ID] = *b
		}
	}
	if err := s.saveCache(ctx, movieCache, lib.ID, next); err != nil {
		return err
	}

	// The box sets that form in this library. Ones known only from the
	// cache are read; Silo answers 404 for one the library has too few
	// movies of, or when box sets are off.
	want := map[string]silo.BoxSet{}
	unreadable := map[string]bool{}
	for _, id := range distinctBoxSets(next) {
		if b, ok := found[id]; ok {
			want[id] = b
			continue
		}
		b, err := api.BoxSet(ctx, id, lib.ID)
		if errors.Is(err, silo.ErrNotFound) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Not knowing is not the same as gone: keep its collection.
			unreadable[id] = true
			failed++
			report.addError(lib, "reading box set %s: %v", id, err)
			continue
		}
		if len(b.Members) > 0 {
			want[id] = b
		}
	}
	report.BoxSets += len(want)

	confirmGone := func(id string) (bool, error) {
		// Not finding a box set is not proof it is gone: only Silo saying
		// so for this box set (404) removes its collection.
		b, err := api.BoxSet(ctx, id, lib.ID)
		switch {
		case errors.Is(err, silo.ErrNotFound):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("could not confirm its box set is gone: %w", err)
		case len(b.Members) > 0:
			return false, errors.New("its box set still forms, though no movie lookup found it")
		default:
			return false, nil
		}
	}
	return s.apply(ctx, api, lib, s.movieKind(cfg), want, unreadable, failed, confirmGone, report)
}

// collectionKind is one family of collections the plugin owns: its slug
// prefix, its group, and what the report calls one.
type collectionKind struct {
	slugPrefix string
	groupSlug  string
	groupName  string
	noun       string
	// excluded, when set, names sources the admin excluded: never made, and
	// removed if they exist.
	excluded func(id string) bool
	// excludeOnDelete records a source whose collection the admin deleted
	// as excluded, where the admin can see and undo it. Without it the
	// deletion is remembered in the plugin's state.
	excludeOnDelete func(id string)
}

func (s *Syncer) movieKind(cfg Config) collectionKind {
	return collectionKind{
		slugPrefix: collectionSlugPrefix, groupSlug: groupSlug, groupName: cfg.GroupName, noun: "box set",
		excluded: func(id string) bool {
			return cfg.ExcludedBoxSet(id) || (s.state != nil && s.state.excludingBoxSets[id])
		},
		excludeOnDelete: func(id string) {
			if s.state != nil {
				s.state.excludingBoxSets[id] = true
			}
		},
	}
}

// apply brings a library's collections of one kind in line with want,
// keyed by the ID after the slug prefix. unreadable names IDs whose state is
// unknown, so their collections are kept; failed counts lookups that failed
// this run, which holds back every removal. confirmGone is asked before each
// removal and keeps the collection unless it answers true; its error says
// why one was kept.
func (s *Syncer) apply(ctx context.Context, api API, lib Library, kind collectionKind, want map[string]silo.BoxSet,
	unreadable map[string]bool, failed int, confirmGone func(id string) (bool, error), report *Report) error {
	collections, err := api.Collections(ctx, lib.ID)
	if err != nil {
		return fmt.Errorf("listing collections: %w", err)
	}
	have := map[string]silo.Collection{}
	for _, c := range collections {
		if id, ok := strings.CutPrefix(c.Slug, kind.slugPrefix); ok && id != "" {
			have[id] = c
		}
	}
	s.noteDeletions(lib, kind, have, want, report)

	// Excluded sources: their collections go at once, as the admin asked.
	for _, id := range sortedKeys(want) {
		if kind.excluded == nil || !kind.excluded(id) {
			continue
		}
		report.Skipped = append(report.Skipped, fmt.Sprintf("%s (%s, excluded)", want[id].Title, id))
		delete(want, id)
		if c, ok := have[id]; ok {
			if err := api.DeleteCollection(ctx, c.ID); err != nil {
				report.addError(lib, "removing excluded %q: %v", c.Title, err)
			} else {
				report.Removed++
				s.forgetCollection(ctx, lib, kind, id, c.ID)
			}
			delete(have, id)
		}
	}

	// Collections whose source no longer qualifies go, but only when this
	// run knows every item's state: a failed lookup (a throttled key, say)
	// leaves sources unknown, not gone. A run that would remove most of the
	// library's collections of this kind is refused too, so no bug or
	// outage can wipe them. One removal that fails (Silo answers 409 while a
	// Home row still shows it) is reported and the rest carry on.
	var remove []silo.Collection
	for _, id := range sortedKeys(have) {
		if _, ok := want[id]; !ok && !unreadable[id] {
			remove = append(remove, have[id])
		}
	}
	switch {
	case len(remove) == 0:
	case failed > 0:
		report.addError(lib, "removals skipped: %d lookups failed, so %d collections were kept until a run completes", failed, len(remove))
		remove = nil
	case len(remove) > massRemovalFloor && len(remove)*2 > len(have):
		report.addError(lib, "removals skipped: %d of %d %s collections would go, more than half; if that is intended, delete them by hand", len(remove), len(have), kind.noun)
		remove = nil
	}
	for _, c := range remove {
		gone, why := confirmGone(strings.TrimPrefix(c.Slug, kind.slugPrefix))
		if !gone {
			if why != nil {
				report.addError(lib, "kept %q: %v", c.Title, why)
			}
			continue
		}
		if err := api.DeleteCollection(ctx, c.ID); err != nil {
			report.addError(lib, "removing %q: %v", c.Title, err)
			continue
		}
		s.forgetCollection(ctx, lib, kind, strings.TrimPrefix(c.Slug, kind.slugPrefix), c.ID)
		report.Removed++
	}
	if len(want) == 0 {
		return nil
	}

	group, err := ensureGroup(ctx, api, lib.ID, kind.groupName, kind.groupSlug)
	if err != nil {
		return err
	}
	groupID := group.ID
	// The group name follows the plugin's setting, so changing the setting
	// renames the group. A failed rename is reported; the sync carries on.
	if name := strings.TrimSpace(kind.groupName); name != "" && strings.TrimSpace(group.Name) != name {
		if err := api.RenameGroup(ctx, group.ID, name); err != nil {
			report.addError(lib, "renaming the %q group to %q: %v", group.Name, name, err)
		}
	}
	for _, id := range sortedKeys(want) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		b := want[id]
		c, exists := have[id]
		if !exists {
			c, err = api.CreateCollection(ctx, silo.NewCollection{
				LibraryID: lib.ID, Title: b.Title, Description: b.Overview,
				Slug: kind.slugPrefix + id, GroupID: groupID,
			})
			if err != nil {
				report.addError(lib, "creating %q: %v", b.Title, err)
				continue
			}
			report.Created++
		}
		if s.state != nil {
			s.state.set(ownedMap, s.state.owned, ownedKey(lib.ID, kind.slugPrefix+id), c.ID)
		}
		changed, err := s.reconcile(ctx, api, c, b, exists)
		if err != nil {
			report.addError(lib, "updating %q: %v", b.Title, err)
			continue
		}
		if exists && changed {
			report.Updated++
		}
	}
	return nil
}

// saveExclusions adds TheTVDB lists and box sets whose collections the
// admin deleted to the excluded settings, in one write that keeps every
// other setting as the plugin last received it, and updates the plugin's
// copy.
func (s *Syncer) saveExclusions(ctx context.Context, lists, boxSets map[string]bool) error {
	if len(lists) == 0 && len(boxSets) == 0 {
		return nil
	}
	cfg := s.config()
	merge := func(have []string, add map[string]bool) []string {
		all := slices.Clone(have)
		for id := range add {
			all = append(all, id)
		}
		return ParseListIDs(strings.Join(all, ","))
	}
	excludedLists, excludedBoxSets := merge(cfg.ExcludedLists, lists), merge(cfg.ExcludedBoxSets, boxSets)
	values := maps.Clone(cfg.Raw)
	if values == nil {
		values = map[string]any{}
	}
	if len(lists) > 0 {
		values[fieldExcludedLists] = strings.Join(excludedLists, ", ")
	}
	if len(boxSets) > 0 {
		values[fieldExcludedBoxSets] = strings.Join(excludedBoxSets, ", ")
	}
	if err := s.host.SaveSettings(ctx, values); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg.ExcludedLists, s.cfg.ExcludedBoxSets, s.cfg.Raw = excludedLists, excludedBoxSets, values
	s.mu.Unlock()
	return nil
}

// noteDeletions finds collections this plugin made that are gone from
// Silo: their admin deleted them, so they must not come back. A box set's
// deletion is remembered in the plugin's state; a franchise's list is
// added to the excluded lists setting, where the admin can undo it. When
// most of a library's collections of a kind look deleted at once, it is
// more likely a fault than a decision, so nothing is recorded that run.
// Sources whose collections were deleted are taken out of want.
func (s *Syncer) noteDeletions(lib Library, kind collectionKind, have map[string]silo.Collection, want map[string]silo.BoxSet, report *Report) {
	if s.state == nil {
		return
	}
	prefix := ownedKey(lib.ID, kind.slugPrefix)
	var gone []string
	owned := 0
	for key, value := range s.state.owned {
		id, ok := strings.CutPrefix(key, prefix)
		if !ok || id == "" {
			continue
		}
		if value == deletedMark {
			if kind.excludeOnDelete != nil {
				// A deletion 1.1.4 kept out of sight moves into the
				// setting, where the admin can see and undo it.
				kind.excludeOnDelete(id)
				s.state.drop(ownedMap, s.state.owned, key)
			}
			if b, ok := want[id]; ok {
				report.Skipped = append(report.Skipped, fmt.Sprintf("%s (%s, you deleted it)", b.Title, id))
				delete(want, id)
			}
			continue
		}
		owned++
		if _, ok := have[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(gone) > massRemovalFloor && len(gone)*2 > owned {
		report.addError(lib, "%d of %d %s collections the plugin made are missing from Silo; not treating them as deleted this sync", len(gone), owned, kind.noun)
		return
	}
	sort.Strings(gone)
	for _, id := range gone {
		title := id
		if b, ok := want[id]; ok {
			title = b.Title
		}
		report.Skipped = append(report.Skipped, fmt.Sprintf("%s (%s, you deleted it)", title, id))
		delete(want, id)
		if kind.excludeOnDelete != nil {
			kind.excludeOnDelete(id)
			s.state.drop(ownedMap, s.state.owned, prefix+id)
		} else {
			s.state.set(ownedMap, s.state.owned, prefix+id, deletedMark)
		}
	}
}

// forgetCollection drops what the plugin kept about a collection it
// removed itself.
func (s *Syncer) forgetCollection(ctx context.Context, lib Library, kind collectionKind, id, collectionID string) {
	s.forgetArtwork(ctx, collectionID)
	if s.state != nil {
		s.state.drop(ownedMap, s.state.owned, ownedKey(lib.ID, kind.slugPrefix+id))
	}
}

// addError records one failure in a library without stopping its sync.
func (r *Report) addError(lib Library, format string, args ...any) {
	r.Errors = append(r.Errors, lib.Name+": "+fmt.Sprintf(format, args...))
}

// reconcile makes one collection match its box set: title, description,
// members in collection order, and artwork while it is the box set's (see
// syncArtwork). It reports whether anything changed.
func (s *Syncer) reconcile(ctx context.Context, api API, c silo.Collection, b silo.BoxSet, existed bool) (bool, error) {
	changed := false
	if existed && (c.Title != b.Title || c.Description != b.Overview) {
		if err := api.UpdateCollection(ctx, c.ID, b.Title, b.Overview); err != nil {
			return false, err
		}
		changed = true
	}

	current := []string{}
	if existed {
		var err error
		if current, err = api.Members(ctx, c.ID); err != nil {
			return false, err
		}
	}
	if !slices.Equal(current, b.Members) {
		wanted := map[string]bool{}
		for _, id := range b.Members {
			wanted[id] = true
		}
		have := map[string]bool{}
		for _, id := range current {
			have[id] = true
			if !wanted[id] {
				if err := api.RemoveMember(ctx, c.ID, id); err != nil {
					return false, err
				}
			}
		}
		for i, id := range b.Members {
			if !have[id] {
				if err := api.AddMember(ctx, c.ID, id, i); err != nil {
					return false, err
				}
			}
		}
		if err := api.OrderMembers(ctx, c.ID, b.Members); err != nil {
			return false, err
		}
		changed = true
	}

	if s.syncArtwork(ctx, api, c, b) {
		changed = true
	}
	return changed, nil
}

// artworkState remembers, per collection and kind, which box set image the
// plugin set (Src) and what the collection's image was right after (Set).
// While the collection still shows Set, the image is the plugin's to
// replace when the box set's changes; anything else is the admin's.
type artworkState map[string]artworkSlot

type artworkSlot struct {
	Src string `json:"src"`
	Set string `json:"set"`
}

// legacyArtworkKey is where versions before 1.1.4 kept a collection's
// artwork state, one key each.
func legacyArtworkKey(collectionID string) string { return "art:" + collectionID }

// syncArtwork sets the box set's poster and backdrop on a collection that has
// none of its own, and replaces them when the box set's change. An image the
// admin uploaded is never touched. Failures are logged, not fatal: artwork
// is retried on the next run.
func (s *Syncer) syncArtwork(ctx context.Context, api API, c silo.Collection, b silo.BoxSet) bool {
	state := s.artworkStateOf(ctx, c.ID)
	changed := false
	for _, a := range []struct {
		kind, curURL, curHash, srcURL, srcHash string
		collage                                bool
	}{
		{"poster", c.PosterURL, c.PosterHash, b.PosterURL, b.PosterThumbhash, c.PosterIsCollage},
		{"backdrop", c.BackdropURL, c.BackdropHash, b.BackdropURL, b.BackdropThumbhash, false},
	} {
		if a.srcURL == "" {
			continue
		}
		src := imageIdentity(a.srcURL, a.srcHash)
		slot, known := state[a.kind]
		empty := a.curURL == "" || a.collage
		ours := empty ||
			(known && slot.Set != "" && imageIdentity(a.curURL, a.curHash) == slot.Set) ||
			// The core box set feature this plugin replaces stored TMDB's URL.
			(!known && isTMDBImage(a.curURL))
		if !ours || (!empty && known && slot.Src == src) {
			continue
		}
		after, err := s.setArtwork(ctx, api, c.ID, a.kind, a.srcURL)
		if err != nil {
			s.log.Warn("box set collections: could not set artwork", "collection", c.Title, "kind", a.kind, "error", err)
			if s.state != nil {
				s.state.artFailures = append(s.state.artFailures, fmt.Sprintf("%s %s: %v", c.Title, a.kind, err))
			}
			continue
		}
		set := imageIdentity(after.PosterURL, after.PosterHash)
		if a.kind == "backdrop" {
			set = imageIdentity(after.BackdropURL, after.BackdropHash)
		}
		state[a.kind] = artworkSlot{Src: src, Set: set}
		changed = true
	}
	if changed && s.state != nil {
		if raw, err := json.Marshal(state); err == nil {
			s.state.set(artworkMap, s.state.artwork, c.ID, string(raw))
		}
	}
	return changed
}

// artworkStateOf returns what the plugin knows about a collection's
// artwork: from the shared artwork map, or from the per-collection key
// versions before 1.1.4 used, which is read but never written again.
func (s *Syncer) artworkStateOf(ctx context.Context, collectionID string) artworkState {
	state := artworkState{}
	if s.state != nil {
		if raw, ok := s.state.artwork[collectionID]; ok {
			_ = json.Unmarshal([]byte(raw), &state)
			return state
		}
	}
	if raw, ok, err := s.host.ReadState(ctx, legacyArtworkKey(collectionID)); err == nil && ok {
		_ = json.Unmarshal(raw, &state)
	}
	return state
}

// setArtwork has Silo fetch the image itself, which saves the plugin a
// download and an upload. Silo refuses private addresses and relative
// ones, so those (a LAN storage server's signed URL, Silo's own image
// proxy) are downloaded and uploaded instead.
func (s *Syncer) setArtwork(ctx context.Context, api API, collectionID, kind, address string) (silo.Collection, error) {
	if strings.HasPrefix(address, "https://") || strings.HasPrefix(address, "http://") {
		if c, err := api.SetArtworkSource(ctx, collectionID, kind, address); err == nil {
			return c, nil
		}
	}
	image, contentType, err := api.Fetch(ctx, address)
	if err != nil {
		return silo.Collection{}, err
	}
	return api.UploadArtwork(ctx, collectionID, kind, image, contentType)
}

func (s *Syncer) forgetArtwork(_ context.Context, collectionID string) {
	if s.state != nil {
		// An empty entry also hides a legacy key's state.
		s.state.set(artworkMap, s.state.artwork, collectionID, "{}")
	}
}

// imageIdentity names an image stably: its thumbhash when Silo has one, else
// its address without the signature query, which changes on every read.
func imageIdentity(address, thumbhash string) string {
	if thumbhash != "" {
		return "th:" + thumbhash
	}
	if address == "" {
		return ""
	}
	if i := strings.IndexByte(address, '?'); i >= 0 {
		address = address[:i]
	}
	return "url:" + address
}

func isTMDBImage(address string) bool {
	return strings.HasPrefix(address, "https://image.tmdb.org/") || strings.HasPrefix(address, "http://image.tmdb.org/")
}

// ensureGroup finds the plugin's group in a library by its slug, else one
// already named name, else creates it.
func ensureGroup(ctx context.Context, api API, libraryID, name, slug string) (silo.Group, error) {
	groups, err := api.Groups(ctx, libraryID)
	if err != nil {
		return silo.Group{}, fmt.Errorf("listing collection groups: %w", err)
	}
	for _, g := range groups {
		if g.Slug == slug {
			return g, nil
		}
	}
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g.Name), name) {
			return g, nil
		}
	}
	g, err := api.CreateGroup(ctx, libraryID, name, slug)
	if err != nil {
		return silo.Group{}, fmt.Errorf("creating the %q group: %w", name, err)
	}
	return g, nil
}

func distinctBoxSets(byMovie map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range byMovie {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The caches map a library's titles to what the sync learned about them.
// Each is a chunked map (see state.go) named "<prefix>:<library>".

// cacheSaveEvery is how many lookups pass between saves of the cache during
// a sync.
const cacheSaveEvery = 200

// Caches are named by a prefix: movieCache for box sets, seriesCache for
// TheTVDB lists.
const (
	movieCache = "cache"
	// seriesCache is versioned: v1.1.0 asked TheTVDB for the short series
	// record, which may have left out every list, and cached that for a
	// week. A new name makes every series be looked up again.
	seriesCache = "tvdb2"
)

func (s *Syncer) loadCache(ctx context.Context, prefix, libraryID string) (map[string]string, error) {
	return s.loadMap(ctx, prefix+":"+libraryID)
}

func (s *Syncer) saveCache(ctx context.Context, prefix, libraryID string, m map[string]string) error {
	if err := s.saveMap(ctx, prefix+":"+libraryID, m); err != nil {
		return fmt.Errorf("saving the cache: %w", err)
	}
	return nil
}
