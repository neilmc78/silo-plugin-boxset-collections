package boxsets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
	"github.com/neilmc78/silo-plugin-boxset-collections/internal/tvdb"
)

// Series franchises come from TheTVDB's official lists: a list with enough
// of a library's series becomes a collection in that library's
// "Franchises" group. They are synced only when the install has its own
// TheTVDB key. Collections hold series only; movies on a list are left out.
// Every collection credits TheTVDB.com and links to the list's page, as
// TheTVDB's free API tier requires.
const (
	franchiseSlugPrefix = "tvdb-list-"
	franchiseGroupSlug  = "franchises"

	// seriesRecheck is how old a series' list lookup may be before a full
	// sync looks it up again; quick syncs look up only new or refreshed
	// series.
	seriesRecheck = 7 * 24 * time.Hour
	// listRefetch is how long a list's details are reused.
	listRefetch = 7 * 24 * time.Hour

	tvdbArtworkBase = "https://artworks.thetvdb.com"
)

// TVDB is the part of TheTVDB's API the series sync uses; *tvdb.Client is
// it.
type TVDB interface {
	SeriesLists(ctx context.Context, seriesID string) ([]tvdb.ListRef, error)
	List(ctx context.Context, id string) (tvdb.List, error)
}

func seriesKind(cfg Config) collectionKind {
	return collectionKind{slugPrefix: franchiseSlugPrefix, groupSlug: franchiseGroupSlug, groupName: cfg.SeriesGroupName, noun: "franchise"}
}

// tvdbClient returns the TheTVDB client for the configured key, kept across
// syncs so its login token is reused.
func (s *Syncer) tvdbClient(cfg Config) TVDB {
	key := cfg.TVDBAPIKey + "\x00" + cfg.TVDBPin
	if s.tvdb == nil || s.tvdbFor != key {
		s.tvdb, s.tvdbFor = s.newTVDB(cfg.TVDBAPIKey, cfg.TVDBPin), key
	}
	return s.tvdb
}

// syncSeries brings every series library's franchise collections in line
// with TheTVDB's official lists. Only a cancelled context stops it; other
// failures are reported per library.
func (s *Syncer) syncSeries(ctx context.Context, api API, cfg Config, full bool, stale map[string]bool, report *Report) error {
	libraries, err := s.host.SeriesLibraries(ctx)
	if err != nil {
		report.Errors = append(report.Errors, "listing series libraries: "+err.Error())
		return nil
	}
	tv := s.tvdbClient(cfg)
	lists := map[string]listResult{} // read once per sync, shared by libraries
	for _, lib := range libraries {
		if err := s.syncSeriesLibrary(ctx, api, tv, cfg, lib, full, stale, lists, report); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", lib.Name, err))
			s.log.Warn("box set collections: series library sync failed", "library", lib.Name, "error", err)
			continue
		}
		report.SeriesLibraries++
	}
	return nil
}

type listResult struct {
	list tvdb.List
	err  error
}

func (s *Syncer) syncSeriesLibrary(ctx context.Context, api API, tv TVDB, cfg Config, lib Library, full bool,
	stale map[string]bool, lists map[string]listResult, report *Report) error {
	series, err := api.Series(ctx, lib.ID)
	if err != nil {
		return fmt.Errorf("listing series: %w", err)
	}
	// byTVDB maps a TheTVDB ID to the library's series; the first of two
	// series sharing one ID wins.
	byTVDB := map[string]string{}
	for _, sr := range series {
		if sr.TvdbID == "" {
			report.SeriesWithoutTVDB++
			continue
		}
		if _, dup := byTVDB[sr.TvdbID]; !dup {
			byTVDB[sr.TvdbID] = sr.ContentID
		}
	}

	// Which official lists each series is on, looked up only when new,
	// refreshed, rematched, or (on a full sync) a week old.
	cache, err := s.loadCache(ctx, seriesCache, lib.ID)
	if err != nil {
		return err
	}
	now := s.now()
	next := make(map[string]string, len(byTVDB))
	failed := 0
	for _, tvdbID := range sortedKeys(byTVDB) {
		contentID := byTVDB[tvdbID]
		entry, cached := cache[contentID]
		checked, cachedID, ids := parseSeriesEntry(entry)
		if cached && cachedID == tvdbID && !stale[contentID] && !stale[lib.ID] && (!full || now.Sub(checked) < seriesRecheck) {
			next[contentID] = entry
			continue
		}
		refs, err := tv.SeriesLists(ctx, tvdbID)
		if errors.Is(err, tvdb.ErrNotFound) {
			refs, err = nil, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if cached && cachedID == tvdbID {
				next[contentID] = entry
			}
			failed++
			report.addError(lib, "looking up the TheTVDB lists of series %s: %v", tvdbID, err)
			continue
		}
		report.SeriesLooked++
		ids = ids[:0]
		for _, r := range refs {
			ids = append(ids, r.ID)
		}
		next[contentID] = formatSeriesEntry(now, tvdbID, ids)
		if report.SeriesLooked%cacheSaveEvery == 0 {
			partial := maps.Clone(cache)
			maps.Copy(partial, next)
			if err := s.saveCache(ctx, seriesCache, lib.ID, partial); err != nil {
				return err
			}
		}
	}
	if err := s.saveCache(ctx, seriesCache, lib.ID, next); err != nil {
		return err
	}

	// The lists that form a franchise in this library.
	listIDs := map[string]bool{}
	for _, entry := range next {
		_, _, ids := parseSeriesEntry(entry)
		for _, id := range ids {
			listIDs[id] = true
		}
	}
	want := map[string]silo.BoxSet{}
	unreadable := map[string]bool{}
	for _, id := range sortedKeys(listIDs) {
		r, ok := lists[id]
		if !ok {
			l, err := s.cachedList(ctx, tv, id)
			r = listResult{list: l, err: err}
			lists[id] = r
		}
		if errors.Is(r.err, tvdb.ErrNotFound) {
			continue
		}
		if r.err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			unreadable[id] = true
			failed++
			report.addError(lib, "reading TheTVDB list %s: %v", id, r.err)
			continue
		}
		if b, ok := franchiseOf(r.list, byTVDB, cfg.SeriesMinShows); ok {
			want[id] = b
		}
	}
	report.Franchises += len(want)

	confirmGone := func(id string) (bool, error) {
		// Only TheTVDB's current answer for this list removes its
		// collection: gone, no longer official, or too few series.
		l, err := tv.List(ctx, id)
		switch {
		case errors.Is(err, tvdb.ErrNotFound):
			return true, nil
		case err != nil:
			return false, fmt.Errorf("could not confirm its TheTVDB list is gone: %w", err)
		}
		s.saveList(ctx, l)
		if _, ok := franchiseOf(l, byTVDB, cfg.SeriesMinShows); ok {
			return false, errors.New("its TheTVDB list still qualifies, though no series lookup found it")
		}
		return true, nil
	}
	return s.apply(ctx, api, lib, seriesKind(cfg), want, unreadable, failed, confirmGone, report)
}

// franchiseOf turns a list into a collection for a library: the list's
// series the library has, in list order, when it is official and the
// library has at least minShows of them.
func franchiseOf(l tvdb.List, byTVDB map[string]string, minShows int) (silo.BoxSet, bool) {
	if !l.Official || l.Name == "" {
		return silo.BoxSet{}, false
	}
	var members []string
	seen := map[string]bool{}
	for _, id := range l.SeriesIDs {
		if c, ok := byTVDB[id]; ok && !seen[c] {
			seen[c] = true
			members = append(members, c)
		}
	}
	if len(members) < max(minShows, 2) {
		return silo.BoxSet{}, false
	}
	return silo.BoxSet{
		ID: l.ID, Title: l.Name, Overview: attributed(l.Overview, l.PageURL()),
		PosterURL: artworkURL(l.Image), Members: members,
	}, true
}

// attributed adds the credit and link TheTVDB's free API tier requires to
// a collection's description, which is where viewers see the list's data.
func attributed(overview, page string) string {
	credit := "Source: TheTVDB.com (" + page + ")"
	if overview = strings.TrimSpace(overview); overview == "" {
		return credit
	}
	return overview + "\n\n" + credit
}

// artworkURL makes a TheTVDB image address absolute; TheTVDB may give a
// path on its artwork host.
func artworkURL(image string) string {
	image = strings.TrimSpace(image)
	if image == "" || strings.HasPrefix(image, "https://") || strings.HasPrefix(image, "http://") {
		return image
	}
	return tvdbArtworkBase + "/" + strings.TrimLeft(image, "/")
}

// A series cache entry is "<checked unix>|<tvdb id>|<list ids, comma
// separated>". The TheTVDB ID is kept so a rematched series is looked up
// again.
func formatSeriesEntry(checked time.Time, tvdbID string, listIDs []string) string {
	sort.Strings(listIDs)
	return strconv.FormatInt(checked.Unix(), 10) + "|" + tvdbID + "|" + strings.Join(listIDs, ",")
}

func parseSeriesEntry(entry string) (checked time.Time, tvdbID string, listIDs []string) {
	parts := strings.SplitN(entry, "|", 3)
	if len(parts) != 3 {
		return time.Time{}, "", nil
	}
	if unix, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
		checked = time.Unix(unix, 0)
	}
	if parts[2] != "" {
		listIDs = strings.Split(parts[2], ",")
	}
	return checked, parts[1], listIDs
}

type storedList struct {
	List    tvdb.List `json:"list"`
	Fetched int64     `json:"fetched"`
}

func listKey(id string) string { return "tvdblist:" + id }

// cachedList returns a list's details, reusing a copy younger than
// listRefetch, on full syncs too: lists change rarely, and each
// request spends the install's own TheTVDB allowance.
func (s *Syncer) cachedList(ctx context.Context, tv TVDB, id string) (tvdb.List, error) {
	if raw, ok, err := s.host.ReadState(ctx, listKey(id)); err == nil && ok {
		var stored storedList
		if json.Unmarshal(raw, &stored) == nil && s.now().Sub(time.Unix(stored.Fetched, 0)) < listRefetch {
			return stored.List, nil
		}
	}
	l, err := tv.List(ctx, id)
	if err != nil {
		return tvdb.List{}, err
	}
	s.saveList(ctx, l)
	return l, nil
}

func (s *Syncer) saveList(ctx context.Context, l tvdb.List) {
	raw, err := json.Marshal(storedList{List: l, Fetched: s.now().Unix()})
	if err != nil {
		return
	}
	if err := s.host.WriteState(ctx, listKey(l.ID), raw); err != nil {
		s.log.Warn("box set collections: could not cache a TheTVDB list", "list", l.ID, "error", err)
	}
}
