package boxsets

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
	"github.com/neilmc78/silo-plugin-boxset-collections/internal/tvdb"
)

// A large library with many box sets and franchises stays well inside a
// key budget far smaller than Silo's 256, and syncs again cleanly.
func TestSyncStaysWithinTheKeyBudget(t *testing.T) {
	s, api, tv := setupSeries(t)
	host := s.host.(*fakeHost)
	host.maxKeys = 20
	for i := range 120 {
		id := fmt.Sprint(1000 + i)
		api.boxSets[id] = silo.BoxSet{ID: id, Title: "Box " + id, PosterURL: "https://img/" + id}
		api.movieBoxSet[fmt.Sprintf("m%d-a", i)] = id
		api.movieBoxSet[fmt.Sprintf("m%d-b", i)] = id
	}
	for i := range 60 {
		list := fmt.Sprint(5000 + i)
		a, b := fmt.Sprint(20000+2*i), fmt.Sprint(20001+2*i)
		tv.lists[list] = tvdb.List{ID: list, Name: "Franchise " + list, Official: true, Image: "https://artworks.thetvdb.com/" + list, SeriesIDs: []string{a, b}}
		tv.seriesLists[a], tv.seriesLists[b] = []string{list}, []string{list}
		api.series = append(api.series, silo.SeriesRef{ContentID: "x" + a, TvdbID: a}, silo.SeriesRef{ContentID: "x" + b, TvdbID: b})
	}
	for range 2 {
		r, err := s.Sync(context.Background(), true)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Errors) > 0 {
			t.Fatalf("problems: %v", r.Errors)
		}
	}
	if api.bySlug("box-set-1119") == nil || api.bySlug("tvdb-list-5059") == nil {
		t.Fatal("not every collection was made")
	}
	if len(host.state) > 15 {
		keys := make([]string, 0, len(host.state))
		for k := range host.state {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		t.Fatalf("%d state keys, want a small fixed set: %v", len(host.state), keys)
	}
}

func TestFullStateSaysWhatToDo(t *testing.T) {
	s, _, _ := setupSeries(t)
	host := s.host.(*fakeHost)
	host.maxKeys = 1
	host.state["filler"] = []byte("x")
	r, _ := s.Sync(context.Background(), true)
	if !strings.Contains(strings.Join(r.Errors, "\n"), "Uninstall and reinstall the plugin once") {
		t.Fatalf("errors = %v, want the reinstall advice", r.Errors)
	}
}

func TestLegacyArtworkStateIsStillRead(t *testing.T) {
	s, _, host := setup(t)
	host.state["art:c9"] = []byte(`{"poster":{"src":"url:a","set":"th:b"}}`)
	st, err := s.loadState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.state = st
	if got := s.artworkStateOf(context.Background(), "c9")["poster"]; got.Src != "url:a" || got.Set != "th:b" {
		t.Fatalf("legacy state = %+v", got)
	}
}

func TestDeletedFranchiseIsExcludedAndCanComeBack(t *testing.T) {
	s, api, _ := setupSeries(t)
	host := s.host.(*fakeHost)
	ctx := context.Background()
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	c := api.bySlug("tvdb-list-7")
	if c == nil {
		t.Fatal("franchise not made")
	}
	// The admin deletes it: it is excluded and not made again.
	delete(api.collections, c.ID)
	r, err := s.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if api.bySlug("tvdb-list-7") != nil {
		t.Fatal("a deleted franchise was made again")
	}
	if got, _ := host.settings["excluded_tvdb_lists"].(string); got != "7" {
		t.Fatalf("excluded lists setting = %q, want 7", got)
	}
	if host.settings["api_key"] != "k" || host.settings["tvdb_api_key"] != "tv" {
		t.Fatalf("saving the exclusion lost other settings: %v", host.settings)
	}
	if !strings.Contains(strings.Join(r.Skipped, "\n"), "(7, you deleted it)") {
		t.Fatalf("skipped = %v", r.Skipped)
	}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("tvdb-list-7") != nil {
		t.Fatal("an excluded franchise was made again")
	}
	// The admin removes the ID from the setting: it comes back.
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "tvdb_api_key": "tv", "excluded_tvdb_lists": ""})
	if err := s.Configure(ctx, []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("tvdb-list-7") == nil {
		t.Fatal("a franchise removed from the excluded lists did not come back")
	}
}

func TestExcludingAListRemovesItsCollection(t *testing.T) {
	s, api, _ := setupSeries(t)
	ctx := context.Background()
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "tvdb_api_key": "tv", "excluded_tvdb_lists": "7 (Yellowstone)"})
	if err := s.Configure(ctx, []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("tvdb-list-7") != nil {
		t.Fatal("an excluded list kept its collection")
	}
}

// boxSetFixture is a movie library with one box set (TMDB collection 10).
func boxSetFixture(t *testing.T) (*Syncer, *fakeAPI, *fakeHost) {
	t.Helper()
	s, api, host := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	if _, err := s.Sync(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("box-set-10") == nil {
		t.Fatal("box set not made")
	}
	return s, api, host
}

func TestDeletedBoxSetIsExcludedAndCanComeBack(t *testing.T) {
	s, api, host := boxSetFixture(t)
	ctx := context.Background()
	delete(api.collections, api.bySlug("box-set-10").ID)
	for range 2 {
		if _, err := s.Sync(ctx, true); err != nil {
			t.Fatal(err)
		}
		if api.bySlug("box-set-10") != nil {
			t.Fatal("a deleted box set was made again")
		}
	}
	if got, _ := host.settings["excluded_box_sets"].(string); got != "10" || host.settings["api_key"] != "k" {
		t.Fatalf("settings = %v, want box set 10 excluded and the API key kept", host.settings)
	}
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "excluded_box_sets": ""})
	if err := s.Configure(ctx, []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("box-set-10") == nil {
		t.Fatal("a box set removed from the excluded setting did not come back")
	}
}

// 1.1.4 kept box set deletions in hidden state; 1.1.5 moves them into the
// setting, where they can be undone.
func TestHiddenBoxSetDeletionMovesIntoTheSetting(t *testing.T) {
	s, api, host := boxSetFixture(t)
	ctx := context.Background()
	delete(api.collections, api.bySlug("box-set-10").ID)
	st, err := s.loadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st.set(ownedMap, st.owned, ownedKey("lib", "box-set-10"), deletedMark)
	if err := s.saveState(ctx, st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := host.settings["excluded_box_sets"].(string); got != "10" {
		t.Fatalf("excluded box sets = %q, want the 1.1.4 deletion moved in", got)
	}
	if api.bySlug("box-set-10") != nil {
		t.Fatal("the deleted box set came back during the move")
	}
}

func TestBoxSetAndFranchiseDeletionsShareOneSave(t *testing.T) {
	s, api, _ := setupSeries(t)
	host := s.host.(*fakeHost)
	ctx := context.Background()
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	delete(api.collections, api.bySlug("box-set-10").ID)
	delete(api.collections, api.bySlug("tvdb-list-7").ID)
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if host.settings["excluded_box_sets"] != "10" || host.settings["excluded_tvdb_lists"] != "7" {
		t.Fatalf("settings = %v, want both deletions saved", host.settings)
	}
}

func TestExcludingABoxSetRemovesItsCollection(t *testing.T) {
	s, api, _ := boxSetFixture(t)
	ctx := context.Background()
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "excluded_box_sets": "10"})
	if err := s.Configure(ctx, []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("box-set-10") != nil {
		t.Fatal("an excluded box set kept its collection")
	}
}

func TestMovieBoxSetsSwitchOffSkipsMovies(t *testing.T) {
	s, api, _ := setupSeries(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k", "tvdb_api_key": "tv", "movie_box_sets": false})
	if err := s.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.lookups) != 0 || api.bySlug("box-set-10") != nil || r.Libraries != 0 {
		t.Fatalf("movies synced with the switch off: lookups %v", api.lookups)
	}
	if api.bySlug("tvdb-list-7") == nil {
		t.Fatal("TV franchises stopped with the movie switch off")
	}
}

func TestListsThatCanNeverFormAreCountedNotNamed(t *testing.T) {
	s, api, tv := setupSeries(t)
	// A film franchise with one spin-off series the library has.
	tv.lists["60"] = tvdb.List{ID: "60", Name: "Friday the 13th Franchise", Official: true, SeriesIDs: []string{"300"}}
	tv.seriesLists["300"] = append(tv.seriesLists["300"], "60")
	api.series = api.series[:3]
	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if r.ListsNeverForm != 1 {
		t.Fatalf("lists that can never form = %d, want 1", r.ListsNeverForm)
	}
	for _, n := range r.NearMisses {
		if n.ListID == "60" {
			t.Fatalf("a one-series list was named as a near miss: %v", n)
		}
	}
}

func TestParseListIDs(t *testing.T) {
	got := ParseListIDs("7502, 15 (The Simpsons)\n007502;  13357")
	if want := []string{"15", "7502", "13357"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRunWhileASyncRunsSaysSo(t *testing.T) {
	s, _, host := setup(t)
	s.setStatus("Sync started earlier")
	s.running.Store(true)
	defer s.running.Store(false)
	if _, err := s.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{}); err != nil {
		t.Fatal(err)
	}
	got := host.status[len(host.status)-1]
	if !strings.HasPrefix(got, "Sync started earlier") || !strings.Contains(got, "this sync is still running") {
		t.Fatalf("status = %q", got)
	}
}
