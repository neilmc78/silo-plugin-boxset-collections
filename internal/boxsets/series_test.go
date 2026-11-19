package boxsets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
	"github.com/neilmc78/silo-plugin-boxset-collections/internal/tvdb"
)

// fakeTVDB is an in-memory TheTVDB: series point at official lists.
type fakeTVDB struct {
	seriesLists map[string][]string // tvdb series ID -> list IDs
	lists       map[string]tvdb.List
	lookups     []string
	listReads   int
	lookupErr   error
}

func (f *fakeTVDB) SeriesLists(_ context.Context, id string) ([]tvdb.ListRef, error) {
	f.lookups = append(f.lookups, id)
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	var refs []tvdb.ListRef
	for _, l := range f.seriesLists[id] {
		refs = append(refs, tvdb.ListRef{ID: l, Official: f.lists[l].Official})
	}
	return refs, nil
}

func (f *fakeTVDB) List(_ context.Context, id string) (tvdb.List, error) {
	f.listReads++
	l, ok := f.lists[id]
	if !ok {
		return tvdb.List{}, tvdb.ErrNotFound
	}
	return l, nil
}

// setupSeries is setup with a TheTVDB key and a Yellowstone-like fixture:
// list 7 (official) holds three of the library's series and a series the
// library lacks; list 9 is not official; list 8 has only one library series.
func setupSeries(t *testing.T) (*Syncer, *fakeAPI, *fakeTVDB) {
	t.Helper()
	s, api, _ := setup(t)
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "tvdb_api_key": "tv", "series_min_shows": float64(2)})
	if err := s.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	tv := &fakeTVDB{
		seriesLists: map[string][]string{"100": {"7", "9"}, "200": {"7", "8"}, "300": {"7"}, "400": {"9"}},
		lists: map[string]tvdb.List{
			"7": {ID: "7", Name: "Yellowstone", Overview: "The Dutton family.", Slug: "yellowstone", Official: true,
				Image: "https://artworks.thetvdb.com/7.jpg", SeriesIDs: []string{"300", "999", "100", "200"}},
			"8": {ID: "8", Name: "Lonely", Official: true, SeriesIDs: []string{"200", "998"}},
			"9": {ID: "9", Name: "My favourites", Official: false, SeriesIDs: []string{"100", "400"}},
		},
	}
	s.newTVDB = func(string, string) TVDB { return tv }
	api.series = []silo.SeriesRef{
		{ContentID: "s1", TvdbID: "100"}, {ContentID: "s2", TvdbID: "200"},
		{ContentID: "s3", TvdbID: "300"}, {ContentID: "s4", TvdbID: "400"}, {ContentID: "s5"},
	}
	return s, api, tv
}

func TestSeriesSyncCreatesAnAttributedFranchise(t *testing.T) {
	s, api, _ := setupSeries(t)
	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	c := api.bySlug("tvdb-list-7")
	if c == nil || r.Franchises != 1 || r.SeriesLibraries != 1 || r.SeriesWithoutTVDB != 1 {
		t.Fatalf("want only the Yellowstone franchise, got %+v report %+v", api.collections, r)
	}
	if api.bySlug("tvdb-list-8") != nil || api.bySlug("tvdb-list-9") != nil {
		t.Fatal("a list below the threshold, or an unofficial one, became a collection")
	}
	if got := api.members[c.ID]; !slices.Equal(got, []string{"s3", "s1", "s2"}) {
		t.Fatalf("members = %v, want the library's series in list order", got)
	}
	if want := "The Dutton family.\n\nSource: TheTVDB.com (https://thetvdb.com/lists/yellowstone)"; c.Description != want {
		t.Fatalf("description = %q, want the attribution %q", c.Description, want)
	}
	var group silo.Group
	for _, g := range api.groups {
		if g.ID == *c.GroupID {
			group = g
		}
	}
	if group.Slug != "franchises" || group.Name != "Franchises" {
		t.Fatalf("group = %+v, want the Franchises group", group)
	}
}

func TestSeriesSyncNeedsATVDBKey(t *testing.T) {
	s, api, _ := setup(t)
	called := false
	s.newTVDB = func(string, string) TVDB { called = true; return &fakeTVDB{} }
	api.series = []silo.SeriesRef{{ContentID: "s1", TvdbID: "100"}}
	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if called || r.SeriesLibraries != 0 {
		t.Fatalf("series synced without a TheTVDB key: report %+v", r)
	}
}

func TestSeriesQuickSyncUsesTheCache(t *testing.T) {
	s, _, tv := setupSeries(t)
	ctx := context.Background()
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	tv.lookups, tv.listReads = nil, 0
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(tv.lookups) != 0 || tv.listReads != 0 {
		t.Fatalf("quick sync called TheTVDB: lookups %v, list reads %d", tv.lookups, tv.listReads)
	}
	// A refreshed series is looked up again.
	s.stale["s2"] = true
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tv.lookups, []string{"200"}) {
		t.Fatalf("lookups = %v, want only the refreshed series", tv.lookups)
	}
}

func TestSeriesSyncRemovesOnlyWhenTVDBConfirms(t *testing.T) {
	s, api, tv := setupSeries(t)
	ctx := context.Background()
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	// The library loses two of the franchise's series, but TheTVDB fails:
	// nothing is removed while lookups fail.
	api.series = api.series[:1]
	tv.lookupErr = errors.New("throttled")
	s.stale["tvlib"] = true
	r, err := s.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if api.bySlug("tvdb-list-7") == nil || r.Removed != 0 {
		t.Fatalf("removed a franchise while TheTVDB failed: report %+v", r)
	}
	if !strings.Contains(strings.Join(r.Errors, "\n"), "removals skipped") {
		t.Fatalf("errors = %v, want removals skipped", r.Errors)
	}
	// TheTVDB answers again and the list no longer qualifies: it goes.
	tv.lookupErr = nil
	s.stale["tvlib"] = true
	if r, err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("tvdb-list-7") != nil || r.Removed != 1 {
		t.Fatalf("franchise outlived its list: report %+v", r)
	}
}

func TestParseConfigReadsSeriesSettings(t *testing.T) {
	value, _ := structpb.NewStruct(map[string]any{"tvdb_api_key": " tv ", "series_min_shows": "3", "series_group_name": "Universes"})
	cfg, err := ParseConfig([]*pluginv1.ConfigEntry{{Key: "settings", Value: value}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TVDBAPIKey != "tv" || cfg.SeriesMinShows != 3 || cfg.SeriesGroupName != "Universes" {
		t.Fatalf("cfg = %+v", cfg)
	}
	value, _ = structpb.NewStruct(map[string]any{"series_min_shows": "lots"})
	if _, err := ParseConfig([]*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err == nil {
		t.Fatal("a non-number minimum was accepted")
	}
}

func TestRunReportsAPreviousSyncWithNoOfficialLists(t *testing.T) {
	s, _, tv := setupSeries(t)
	ctx := context.Background()
	// Every list the library's series are on is unofficial.
	for id, l := range tv.lists {
		l.Official = false
		tv.lists[id] = l
	}
	if _, err := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{}); err != nil {
		t.Fatalf("first Run, with no report yet: %v", err)
	}
	s.wg.Wait()
	_, err := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{})
	s.wg.Wait()
	if err == nil {
		t.Fatal("Run hid a sync that found no official lists")
	}
	for _, want := range []string{"0 official lists (3 unofficial", "0 franchises", "no official list"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}

	// Once a franchise forms, Run reports success again.
	for id, l := range tv.lists {
		l.Official = id != "9"
		tv.lists[id] = l
	}
	s.stale["tvlib"] = true
	s.syncAndRecord(ctx, false)
	if _, err := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{}); err != nil {
		s.wg.Wait()
		t.Fatalf("Run after a clean sync: %v", err)
	}
	s.wg.Wait()
}

func TestSyncRenamesTheGroupToTheSetting(t *testing.T) {
	s, api, _ := setupSeries(t)
	ctx := context.Background()
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "tvdb_api_key": "tv", "series_group_name": "Universes"})
	if err := s.Configure(ctx, []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range api.groups {
		if g.Slug == "franchises" {
			names = append(names, g.Name)
		}
	}
	if !slices.Equal(names, []string{"Universes"}) {
		t.Fatalf("franchise groups = %v, want the one group renamed to Universes", names)
	}
}

// runTwice records a sync's report and returns the error the next Run
// reports for it.
func runTwice(t *testing.T, s *Syncer) error {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{}); err != nil {
		s.wg.Wait()
		t.Fatalf("first Run, with no report yet: %v", err)
	}
	s.wg.Wait()
	_, err := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{})
	s.wg.Wait()
	return err
}

func TestRunNamesTheClosestListWhenNoFranchiseForms(t *testing.T) {
	s, api, _ := setupSeries(t)
	// The library keeps only one of the Yellowstone list's series.
	api.series = []silo.SeriesRef{{ContentID: "s3", TvdbID: "300"}}
	err := runTwice(t, s)
	if err == nil {
		t.Fatal("Run hid a sync where no official list had enough series")
	}
	for _, want := range []string{"Closest: Yellowstone (7) matched 1 of your series (list has 4 series; needs 2)", "0 franchises"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

func TestRunSaysWhenAListHasNoSeries(t *testing.T) {
	s, _, tv := setupSeries(t)
	// TheTVDB answers with the list but no series entities in it.
	for id, l := range tv.lists {
		l.SeriesIDs = nil
		tv.lists[id] = l
	}
	err := runTwice(t, s)
	if err == nil || !strings.Contains(err.Error(), "matched 0 of your series (list has 0 series") {
		t.Fatalf("err = %v, want the empty list named", err)
	}
}

func TestRunStaysQuietWithoutATVDBKey(t *testing.T) {
	s, api, _ := setup(t)
	api.series = []silo.SeriesRef{{ContentID: "s1", TvdbID: "100"}}
	if err := runTwice(t, s); err != nil {
		t.Fatalf("Run without a TheTVDB key: %v", err)
	}
}
