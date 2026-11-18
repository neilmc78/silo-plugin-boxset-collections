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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
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
	ReadState(ctx context.Context, key string) ([]byte, bool, error)
	WriteState(ctx context.Context, key string, value []byte) error
}

// API is the part of Silo's REST API the sync uses; *silo.Client is it.
type API interface {
	PrimaryProfile(ctx context.Context) (silo.Profile, error)
	UseProfile(id string)
	Movies(ctx context.Context, libraryID string) ([]string, error)
	ItemBoxSetID(ctx context.Context, contentID, libraryID string) (string, error)
	BoxSet(ctx context.Context, id, libraryID string) (silo.BoxSet, error)
	Collections(ctx context.Context, libraryID string) ([]silo.Collection, error)
	Groups(ctx context.Context, libraryID string) ([]silo.Group, error)
	CreateGroup(ctx context.Context, libraryID, name, slug string) (silo.Group, error)
	CreateCollection(ctx context.Context, n silo.NewCollection) (silo.Collection, error)
	UpdateCollection(ctx context.Context, id, title, description string) error
	DeleteCollection(ctx context.Context, id string) error
	Members(ctx context.Context, collectionID string) ([]string, error)
	AddMember(ctx context.Context, collectionID, contentID string, position int) error
	RemoveMember(ctx context.Context, collectionID, contentID string) error
	OrderMembers(ctx context.Context, collectionID string, contentIDs []string) error
	Fetch(ctx context.Context, address string) ([]byte, string, error)
	UploadArtwork(ctx context.Context, collectionID, kind string, image []byte, contentType string) error
}

// Syncer is the plugin: a scheduled task (a full sync) and an event
// consumer (a quick sync after scans and metadata refreshes).
type Syncer struct {
	pluginv1.UnimplementedScheduledTaskServer
	pluginv1.UnimplementedEventConsumerServer

	log    *slog.Logger
	host   Host
	newAPI func(baseURL, apiKey string) API

	mu  sync.Mutex
	cfg Config

	runMu sync.Mutex // one sync at a time

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
	return &Syncer{log: log, host: host, newAPI: newAPI, stale: map[string]bool{}}
}

// Configure stores the settings the host sends.
func (s *Syncer) Configure(_ context.Context, entries []*pluginv1.ConfigEntry) error {
	cfg, err := ParseConfig(entries)
	if err != nil {
		return err
	}
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
// so drift the events missed is corrected.
func (s *Syncer) Run(ctx context.Context, _ *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	report, err := s.Sync(ctx, true)
	if err != nil {
		return nil, err
	}
	out, _ := structpb.NewStruct(report.Map())
	return &pluginv1.RunScheduledTaskResponse{Output: out}, nil
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
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		if _, err := s.Sync(ctx, false); err != nil {
			s.log.Warn("box set collections: sync after a library change failed", "error", err)
		}
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
	Created   int
	Updated   int
	Removed   int
	Errors    []string
}

// Map is the report as a scheduled task's output.
func (r Report) Map() map[string]any {
	errs := make([]any, len(r.Errors))
	for i, e := range r.Errors {
		errs[i] = e
	}
	return map[string]any{
		"libraries": r.Libraries, "movies_looked_up": r.Looked, "box_sets": r.BoxSets,
		"created": r.Created, "updated": r.Updated, "removed": r.Removed, "errors": errs,
	}
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

	libraries, err := s.host.MovieLibraries(ctx)
	if err != nil {
		return report, fmt.Errorf("listing libraries: %w", err)
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
	s.log.Info("box set collections: synced", "libraries", report.Libraries, "box_sets", report.BoxSets,
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
	if len(report.Errors) > 0 && report.Libraries == 0 {
		return report, errors.New(strings.Join(report.Errors, "; "))
	}
	return report, nil
}

func (s *Syncer) syncLibrary(ctx context.Context, api API, cfg Config, lib Library, full bool, stale map[string]bool, report *Report) error {
	movies, err := api.Movies(ctx, lib.ID)
	if err != nil {
		return fmt.Errorf("listing movies: %w", err)
	}

	// Which box set each movie is in, looked up only for movies not cached.
	cache, err := s.loadCache(ctx, lib.ID)
	if err != nil {
		return err
	}
	next := make(map[string]string, len(movies))
	for _, id := range movies {
		if boxSet, ok := cache[id]; ok && !full && !stale[id] && !stale[lib.ID] {
			next[id] = boxSet
			continue
		}
		boxSet, err := api.ItemBoxSetID(ctx, id, lib.ID)
		if err != nil {
			return fmt.Errorf("looking up a movie's box set: %w", err)
		}
		report.Looked++
		next[id] = boxSet
	}
	if err := s.saveCache(ctx, lib.ID, next); err != nil {
		return err
	}

	// The box sets that form in this library. Silo answers 404 for one the
	// library has too few movies of, or when box sets are off.
	want := map[string]silo.BoxSet{}
	for _, id := range distinctBoxSets(next) {
		b, err := api.BoxSet(ctx, id, lib.ID)
		if errors.Is(err, silo.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading box set %s: %w", id, err)
		}
		if len(b.Members) == 0 {
			continue
		}
		want[id] = b
	}
	report.BoxSets += len(want)

	collections, err := api.Collections(ctx, lib.ID)
	if err != nil {
		return fmt.Errorf("listing collections: %w", err)
	}
	have := map[string]silo.Collection{}
	for _, c := range collections {
		if id, ok := strings.CutPrefix(c.Slug, collectionSlugPrefix); ok && id != "" {
			have[id] = c
		}
	}

	// Collections whose box set no longer forms go first.
	for id, c := range have {
		if _, ok := want[id]; ok {
			continue
		}
		if err := api.DeleteCollection(ctx, c.ID); err != nil {
			return fmt.Errorf("removing %q: %w", c.Title, err)
		}
		report.Removed++
	}
	if len(want) == 0 {
		return nil
	}

	groupID, err := ensureGroup(ctx, api, lib.ID, cfg.GroupName)
	if err != nil {
		return err
	}
	for _, id := range sortedKeys(want) {
		b := want[id]
		c, exists := have[id]
		if !exists {
			c, err = api.CreateCollection(ctx, silo.NewCollection{
				LibraryID: lib.ID, Title: b.Title, Description: b.Overview,
				Slug: collectionSlugPrefix + id, GroupID: groupID,
			})
			if err != nil {
				return fmt.Errorf("creating %q: %w", b.Title, err)
			}
			report.Created++
		}
		changed, err := s.reconcile(ctx, api, c, b, exists)
		if err != nil {
			return fmt.Errorf("updating %q: %w", b.Title, err)
		}
		if exists && changed {
			report.Updated++
		}
	}
	return nil
}

// reconcile makes one collection match its box set: title, description,
// members in collection order, and artwork while the collection has none of
// its own. It reports whether anything changed.
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

	// Artwork is set only where the collection has none of its own, so an
	// admin's upload is never replaced.
	if (c.PosterURL == "" || c.PosterIsCollage) && b.PosterURL != "" {
		if ok := s.copyArtwork(ctx, api, c.ID, "poster", b.PosterURL); ok {
			changed = true
		}
	}
	if c.BackdropURL == "" && b.BackdropURL != "" {
		if ok := s.copyArtwork(ctx, api, c.ID, "backdrop", b.BackdropURL); ok {
			changed = true
		}
	}
	return changed, nil
}

// copyArtwork uploads a box set image to a collection. Artwork is not worth
// failing the sync over, so a failure is logged and retried next time.
func (s *Syncer) copyArtwork(ctx context.Context, api API, collectionID, kind, address string) bool {
	image, contentType, err := api.Fetch(ctx, address)
	if err == nil {
		err = api.UploadArtwork(ctx, collectionID, kind, image, contentType)
	}
	if err != nil {
		s.log.Warn("box set collections: could not copy artwork", "collection", collectionID, "kind", kind, "error", err)
		return false
	}
	return true
}

func ensureGroup(ctx context.Context, api API, libraryID, name string) (string, error) {
	groups, err := api.Groups(ctx, libraryID)
	if err != nil {
		return "", fmt.Errorf("listing collection groups: %w", err)
	}
	for _, g := range groups {
		if g.Slug == groupSlug {
			return g.ID, nil
		}
	}
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g.Name), name) {
			return g.ID, nil
		}
	}
	g, err := api.CreateGroup(ctx, libraryID, name, groupSlug)
	if err != nil {
		return "", fmt.Errorf("creating the %q group: %w", name, err)
	}
	return g.ID, nil
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

func sortedKeys(m map[string]silo.BoxSet) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The cache maps a library's movies to their box set ID ("" for none). It is
// kept in the host's instance state, split into chunks that stay well under
// the host's per-key limit.
const cacheChunk = 2500

func cacheKey(libraryID string, n int) string { return fmt.Sprintf("cache:%s:%d", libraryID, n) }
func cacheCountKey(libraryID string) string   { return "cache:" + libraryID + ":chunks" }

func (s *Syncer) loadCache(ctx context.Context, libraryID string) (map[string]string, error) {
	out := map[string]string{}
	raw, ok, err := s.host.ReadState(ctx, cacheCountKey(libraryID))
	if err != nil || !ok {
		return out, err
	}
	var chunks int
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return out, nil
	}
	for i := 0; i < chunks; i++ {
		raw, ok, err := s.host.ReadState(ctx, cacheKey(libraryID, i))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var part map[string]string
		if json.Unmarshal(raw, &part) == nil {
			for k, v := range part {
				out[k] = v
			}
		}
	}
	return out, nil
}

func (s *Syncer) saveCache(ctx context.Context, libraryID string, m map[string]string) error {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	chunks := 0
	for start := 0; start < len(ids); start += cacheChunk {
		part := map[string]string{}
		for _, id := range ids[start:min(start+cacheChunk, len(ids))] {
			part[id] = m[id]
		}
		raw, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if err := s.host.WriteState(ctx, cacheKey(libraryID, chunks), raw); err != nil {
			return fmt.Errorf("saving the cache: %w", err)
		}
		chunks++
	}
	raw, _ := json.Marshal(chunks)
	return s.host.WriteState(ctx, cacheCountKey(libraryID), raw)
}
