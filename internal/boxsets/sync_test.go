package boxsets

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
)

type fakeHost struct {
	mu    sync.Mutex
	state map[string][]byte
}

func (h *fakeHost) BaseURL(context.Context) (string, error) { return "http://silo", nil }
func (h *fakeHost) MovieLibraries(context.Context) ([]Library, error) {
	return []Library{{ID: "lib", Name: "Movies"}}, nil
}
func (h *fakeHost) ReadState(_ context.Context, k string) ([]byte, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.state[k]
	return v, ok, nil
}
func (h *fakeHost) WriteState(_ context.Context, k string, v []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state[k] = v
	return nil
}

// fakeAPI is an in-memory Silo: movies point at box sets, and box sets
// with fewer than two movies answer 404 as Silo does.
type fakeAPI struct {
	movieBoxSet map[string]string // movie -> box set ("" for none)
	boxSets     map[string]silo.BoxSet
	collections map[string]*silo.Collection
	members     map[string][]string
	groups      []silo.Group
	lookups     []string
	boxSetReads int
	deleteErr   error
	lookupErr   map[string]error
	readErr     map[string]error
	slowMovies  time.Duration
	sourceErr   error
	uploads     []string
	nextID      int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{movieBoxSet: map[string]string{}, boxSets: map[string]silo.BoxSet{},
		collections: map[string]*silo.Collection{}, members: map[string][]string{}}
}

func (f *fakeAPI) id(prefix string) string { f.nextID++; return fmt.Sprintf("%s%d", prefix, f.nextID) }

func (f *fakeAPI) PrimaryProfile(context.Context) (silo.Profile, error) {
	return silo.Profile{ID: "p1", IsPrimary: true}, nil
}
func (f *fakeAPI) UseProfile(string) {}
func (f *fakeAPI) Movies(context.Context, string) ([]string, error) {
	time.Sleep(f.slowMovies)
	var out []string
	for id := range f.movieBoxSet {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}
func (f *fakeAPI) ItemBoxSet(ctx context.Context, cid, lib string) (*silo.BoxSet, error) {
	f.lookups = append(f.lookups, cid)
	if err := f.lookupErr[cid]; err != nil {
		return nil, err
	}
	id := f.movieBoxSet[cid]
	if id == "" {
		return nil, nil
	}
	b, err := f.boxSet(id)
	if err != nil {
		return nil, nil
	}
	return &b, nil
}
func (f *fakeAPI) BoxSet(_ context.Context, id, _ string) (silo.BoxSet, error) {
	f.boxSetReads++
	if err := f.readErr[id]; err != nil {
		return silo.BoxSet{}, err
	}
	return f.boxSet(id)
}

func (f *fakeAPI) boxSet(id string) (silo.BoxSet, error) {
	b, ok := f.boxSets[id]
	if !ok {
		return silo.BoxSet{}, silo.ErrNotFound
	}
	b.Members = nil
	for _, m := range f.sortedMovies() {
		if f.movieBoxSet[m] == id {
			b.Members = append(b.Members, m)
		}
	}
	if len(b.Members) < 2 {
		return silo.BoxSet{}, silo.ErrNotFound
	}
	return b, nil
}
func (f *fakeAPI) sortedMovies() []string { m, _ := f.Movies(context.Background(), ""); return m }
func (f *fakeAPI) Collections(context.Context, string) ([]silo.Collection, error) {
	var out []silo.Collection
	for _, c := range f.collections {
		out = append(out, *c)
	}
	return out, nil
}
func (f *fakeAPI) Groups(context.Context, string) ([]silo.Group, error) { return f.groups, nil }
func (f *fakeAPI) CreateGroup(_ context.Context, _, name, slug string) (silo.Group, error) {
	g := silo.Group{ID: f.id("g"), Name: name, Slug: slug}
	f.groups = append(f.groups, g)
	return g, nil
}
func (f *fakeAPI) CreateCollection(_ context.Context, n silo.NewCollection) (silo.Collection, error) {
	gid := n.GroupID
	c := &silo.Collection{ID: f.id("c"), Title: n.Title, Description: n.Description, Slug: n.Slug, GroupID: &gid}
	f.collections[c.ID] = c
	return *c, nil
}
func (f *fakeAPI) UpdateCollection(_ context.Context, id, title, desc string) error {
	f.collections[id].Title, f.collections[id].Description = title, desc
	return nil
}
func (f *fakeAPI) DeleteCollection(_ context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.collections, id)
	delete(f.members, id)
	return nil
}
func (f *fakeAPI) Members(_ context.Context, id string) ([]string, error) {
	return slices.Clone(f.members[id]), nil
}
func (f *fakeAPI) AddMember(_ context.Context, id, cid string, _ int) error {
	f.members[id] = append(f.members[id], cid)
	return nil
}
func (f *fakeAPI) RemoveMember(_ context.Context, id, cid string) error {
	f.members[id] = slices.DeleteFunc(f.members[id], func(s string) bool { return s == cid })
	return nil
}
func (f *fakeAPI) OrderMembers(_ context.Context, id string, ids []string) error {
	f.members[id] = slices.Clone(ids)
	return nil
}
func (f *fakeAPI) Fetch(_ context.Context, address string) ([]byte, string, error) {
	return []byte(address), "image/jpeg", nil
}
func (f *fakeAPI) UploadArtwork(_ context.Context, id, kind string, image []byte, _ string) (silo.Collection, error) {
	f.uploads = append(f.uploads, id+":"+kind+":upload")
	return f.setArt(id, kind, string(image)), nil
}
func (f *fakeAPI) SetArtworkSource(_ context.Context, id, kind, src string) (silo.Collection, error) {
	if f.sourceErr != nil {
		return silo.Collection{}, f.sourceErr
	}
	f.uploads = append(f.uploads, id+":"+kind+":source")
	return f.setArt(id, kind, src), nil
}

// setArt stores an image the way Silo does: a new stored path whose
// thumbhash follows the image.
func (f *fakeAPI) setArt(id, kind, src string) silo.Collection {
	c := f.collections[id]
	stored, hash := "/stored/"+f.id("img"), "hash-of-"+strings.SplitN(src, "?", 2)[0]
	if kind == "poster" {
		c.PosterURL, c.PosterHash, c.PosterIsCollage = stored+"?sig=1", hash, false
	} else {
		c.BackdropURL, c.BackdropHash = stored+"?sig=1", hash
	}
	return *c
}

func setup(t *testing.T) (*Syncer, *fakeAPI, *fakeHost) {
	t.Helper()
	api := newFakeAPI()
	host := &fakeHost{state: map[string][]byte{}}
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), host, func(string, string) API { return api })
	value, _ := structpb.NewStruct(map[string]any{"api_key": "k"})
	if err := s.Configure(context.Background(), []*pluginv1.ConfigEntry{{Key: "settings", Value: value}}); err != nil {
		t.Fatal(err)
	}
	return s, api, host
}

func (f *fakeAPI) bySlug(slug string) *silo.Collection {
	for _, c := range f.collections {
		if c.Slug == slug {
			return c
		}
	}
	return nil
}

func TestSyncCreatesUpdatesAndRemoves(t *testing.T) {
	s, api, _ := setup(t)
	ctx := context.Background()
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien", Overview: "o", PosterURL: "/p", BackdropURL: "/b"}
	api.boxSets["20"] = silo.BoxSet{ID: "20", Title: "Solo"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10", "m3": "20", "m4": ""}
	// An admin's own collection, which the plugin must not touch.
	api.collections["mine"] = &silo.Collection{ID: "mine", Title: "Mine", Slug: "mine"}

	r, err := s.Sync(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	c := api.bySlug("box-set-10")
	if c == nil || r.Created != 1 || r.BoxSets != 1 {
		t.Fatalf("want one box set collection, got %+v report %+v", api.collections, r)
	}
	if len(api.groups) != 1 || api.groups[0].Slug != "box-sets" || api.groups[0].Name != "Box Sets" || *c.GroupID != api.groups[0].ID {
		t.Fatalf("group: %+v, collection group %v", api.groups, *c.GroupID)
	}
	if got := api.members[c.ID]; !slices.Equal(got, []string{"m1", "m2"}) {
		t.Fatalf("members %v", got)
	}
	if len(api.uploads) != 2 {
		t.Fatalf("uploads %v", api.uploads)
	}

	// Title change, a new member, and a second box set forming.
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien Collection", Overview: "o"}
	api.movieBoxSet["m5"] = "10"
	api.movieBoxSet["m6"] = "20"
	if r, err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if c.Title != "Alien Collection" || !slices.Equal(api.members[c.ID], []string{"m1", "m2", "m5"}) {
		t.Fatalf("after update: %q %v", c.Title, api.members[c.ID])
	}
	if api.bySlug("box-set-20") == nil || r.Created != 1 || r.Updated != 1 {
		t.Fatalf("report %+v", r)
	}
	if len(api.uploads) != 2 {
		t.Fatalf("artwork re-uploaded over existing art: %v", api.uploads)
	}

	// Box set 20 drops below two movies: its collection goes, the admin's stays.
	delete(api.movieBoxSet, "m6")
	if r, err = s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if api.bySlug("box-set-20") != nil || r.Removed != 1 || api.collections["mine"] == nil {
		t.Fatalf("after removal: %+v report %+v", api.collections, r)
	}
}

func TestSyncAdoptsExistingCollectionAndKeepsUploadedArt(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien", PosterURL: "/p", BackdropURL: "/b"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	api.groups = []silo.Group{{ID: "g0", Name: "Franchises", Slug: "box-sets"}}
	api.collections["old"] = &silo.Collection{ID: "old", Title: "Alien", Slug: "box-set-10",
		PosterURL: "/admin-upload", BackdropURL: "/admin-backdrop"}
	api.members["old"] = []string{"m2", "m1", "stray"}

	r, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Created != 0 || len(api.collections) != 1 || len(api.groups) != 1 {
		t.Fatalf("duplicated instead of adopting: %+v %+v", api.collections, api.groups)
	}
	if !slices.Equal(api.members["old"], []string{"m1", "m2"}) {
		t.Fatalf("members %v", api.members["old"])
	}
	if len(api.uploads) != 0 {
		t.Fatalf("replaced the admin's artwork: %v", api.uploads)
	}

	// A collage poster is Silo's default, not the admin's: it is replaced.
	api.collections["old"].PosterIsCollage = true
	if _, err := s.Sync(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.uploads, []string{"old:poster:upload"}) {
		t.Fatalf("uploads %v", api.uploads)
	}
}

func TestSyncCachesLookupsAndRechecksStaleMovies(t *testing.T) {
	s, api, _ := setup(t)
	ctx := context.Background()
	api.movieBoxSet = map[string]string{"m1": "", "m2": ""}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	api.lookups = nil
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(api.lookups) != 0 {
		t.Fatalf("cached movies looked up again: %v", api.lookups)
	}

	// A metadata refresh marks a movie stale; only it is looked up.
	payload, _ := structpb.NewStruct(map[string]any{"raw": "m2"})
	eventDelay = 1<<62 - 1 // never fire in the test
	if _, err := s.HandleEvent(ctx, &pluginv1.HandleEventRequest{EventName: EventMetadataUpdated, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	s.timer.Stop()
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.lookups, []string{"m2"}) {
		t.Fatalf("lookups %v", api.lookups)
	}

	// A refreshed library re-checks all its movies.
	api.lookups = nil
	payload, _ = structpb.NewStruct(map[string]any{"raw": "lib"})
	if _, err := s.HandleEvent(ctx, &pluginv1.HandleEventRequest{EventName: EventMetadataUpdated, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	s.timer.Stop()
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(api.lookups) != 2 {
		t.Fatalf("library refresh lookups %v", api.lookups)
	}

	// A full run looks every movie up.
	api.lookups = nil
	if _, err := s.Sync(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(api.lookups) != 2 {
		t.Fatalf("full run lookups %v", api.lookups)
	}
}

func TestSyncNeedsAPIKey(t *testing.T) {
	s := New(slog.New(slog.NewTextHandler(io.Discard, nil)), &fakeHost{state: map[string][]byte{}}, nil)
	if _, err := s.Sync(context.Background(), true); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err %v", err)
	}
}

func TestCacheRoundTripsAcrossChunks(t *testing.T) {
	s, _, _ := setup(t)
	ctx := context.Background()
	m := map[string]string{}
	for i := range cacheChunk*2 + 7 {
		m[fmt.Sprintf("m%05d", i)] = fmt.Sprint(i % 3)
	}
	if err := s.saveCache(ctx, "lib", m); err != nil {
		t.Fatal(err)
	}
	got, err := s.loadCache(ctx, "lib")
	if err != nil || len(got) != len(m) || got["m00004"] != "1" {
		t.Fatalf("got %d entries, err %v", len(got), err)
	}
}

func TestSyncLooksUpEachBoxSetOnce(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10", "m3": "10", "m4": ""}
	if _, err := s.Sync(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// m1 answers for its siblings m2 and m3; m4 has no box set.
	if !slices.Equal(api.lookups, []string{"m1", "m4"}) || api.boxSetReads != 0 {
		t.Fatalf("lookups %v, box set reads %d", api.lookups, api.boxSetReads)
	}
	// A quick run reads cached box sets once each.
	api.lookups = nil
	if _, err := s.Sync(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(api.lookups) != 0 || api.boxSetReads != 1 {
		t.Fatalf("lookups %v, box set reads %d", api.lookups, api.boxSetReads)
	}
}

func TestSyncCarriesOnPastAFailedRemoval(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	api.collections["gone"] = &silo.Collection{ID: "gone", Title: "Gone", Slug: "box-set-99"}
	api.deleteErr = &silo.Error{Status: 409, Body: "used by a page section"}

	r, err := s.Sync(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Created != 1 || r.Removed != 0 || len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "Gone") {
		t.Fatalf("report %+v", r)
	}
}

func TestSyncArtworkFollowsTheBoxSet(t *testing.T) {
	s, api, _ := setup(t)
	ctx := context.Background()
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien",
		PosterURL: "https://img.example/p1?sig=a", PosterThumbhash: "P1"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	c := api.bySlug("box-set-10")
	if !slices.Equal(api.uploads, []string{c.ID + ":poster:source"}) {
		t.Fatalf("uploads %v", api.uploads)
	}

	// Same image under a fresh signature: nothing to do.
	b := api.boxSets["10"]
	b.PosterURL = "https://img.example/p1?sig=b"
	api.boxSets["10"] = b
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(api.uploads) != 1 {
		t.Fatalf("re-set an unchanged poster: %v", api.uploads)
	}

	// TMDB has a new poster: the plugin's poster follows it. Silo refuses
	// the source (a private address), so the bytes are uploaded.
	b.PosterURL, b.PosterThumbhash = "https://img.example/p2", "P2"
	api.boxSets["10"] = b
	api.sourceErr = &silo.Error{Status: 422, Body: "not allowed"}
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(api.uploads) != 2 || api.uploads[1] != c.ID+":poster:upload" {
		t.Fatalf("uploads %v", api.uploads)
	}

	// The admin uploads their own; a later box set change leaves it alone.
	c.PosterURL, c.PosterHash = "/stored/admin", "ADMIN"
	b.PosterURL, b.PosterThumbhash = "https://img.example/p3", "P3"
	api.boxSets["10"] = b
	if _, err := s.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(api.uploads) != 2 || c.PosterHash != "ADMIN" {
		t.Fatalf("replaced the admin's poster: %v", api.uploads)
	}
}

func TestSyncTakesOverTMDBArtworkFromTheCoreFeature(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien", BackdropURL: "https://img.example/b", BackdropThumbhash: "B"}
	api.movieBoxSet = map[string]string{"m1": "10", "m2": "10"}
	api.collections["old"] = &silo.Collection{ID: "old", Title: "Alien", Slug: "box-set-10",
		BackdropURL: "https://image.tmdb.org/t/p/original/x.jpg"}
	if _, err := s.Sync(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.uploads, []string{"old:backdrop:source"}) {
		t.Fatalf("uploads %v", api.uploads)
	}
}

// Silo throttles API keys (429). A movie whose lookup failed is unknown,
// not box-set-less: its box set's collection must survive.
func TestSyncKeepsCollectionsWhenLookupsFail(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.boxSets["20"] = silo.BoxSet{ID: "20", Title: "Rocky"}
	api.movieBoxSet = map[string]string{"a1": "10", "a2": "10", "r1": "20", "r2": "20"}
	api.collections["c20"] = &silo.Collection{ID: "c20", Title: "Rocky", Slug: "box-set-20"}
	throttled := &silo.Error{Status: 429, Body: "rate limited"}
	api.lookupErr = map[string]error{"r1": throttled, "r2": throttled}

	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if api.collections["c20"] == nil || r.Removed != 0 {
		t.Fatalf("deleted a collection whose movies could not be looked up: %+v", r)
	}
	if api.bySlug("box-set-10") == nil {
		t.Fatalf("known box set not created: %+v", api.collections)
	}
	if !strings.Contains(strings.Join(r.Errors, "\n"), "removals skipped") {
		t.Fatalf("report does not say removals were skipped: %v", r.Errors)
	}
}

func TestSyncRefusesMassRemoval(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"a1": "10", "a2": "10"}
	for i := range 8 {
		id := fmt.Sprintf("old%d", i)
		api.collections[id] = &silo.Collection{ID: id, Title: id, Slug: fmt.Sprintf("box-set-%d", 100+i)}
	}
	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Removed != 0 || len(api.collections) != 9 {
		t.Fatalf("mass removal went ahead: removed %d, left %d", r.Removed, len(api.collections))
	}
	if !strings.Contains(strings.Join(r.Errors, "\n"), "removals skipped") {
		t.Fatalf("report: %v", r.Errors)
	}
}

// A collection goes only when Silo says its box set does not form (404).
// Any other answer about it keeps the collection.
func TestSyncRemovesOnlyOnSilosWord(t *testing.T) {
	s, api, _ := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"a1": "10", "a2": "10"}
	api.collections["c30"] = &silo.Collection{ID: "c30", Title: "Rocky", Slug: "box-set-30"}
	api.collections["c40"] = &silo.Collection{ID: "c40", Title: "Saw", Slug: "box-set-40"}
	api.readErr = map[string]error{"30": &silo.Error{Status: 500, Body: "boom"}}

	r, err := s.Sync(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if api.collections["c30"] == nil {
		t.Fatal("removed a collection Silo could not answer for")
	}
	if api.collections["c40"] != nil || r.Removed != 1 {
		t.Fatalf("box set 40 does not form (404) and should go: %+v", r)
	}
}

// Silo gives a scheduled task 10 seconds to answer (DefaultControlTimeout),
// so Run must answer at once and sync in the background, unaffected by the
// call's deadline.
func TestRunSyncsInTheBackground(t *testing.T) {
	s, api, host := setup(t)
	api.boxSets["10"] = silo.BoxSet{ID: "10", Title: "Alien"}
	api.movieBoxSet = map[string]string{"a1": "10", "a2": "10"}
	api.slowMovies = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	resp, err := s.Run(ctx, &pluginv1.RunScheduledTaskRequest{})
	if err != nil || time.Since(start) > 40*time.Millisecond {
		t.Fatalf("Run took %v, err %v", time.Since(start), err)
	}
	if got := resp.GetOutput().AsMap()["status"]; got != "started" {
		t.Fatalf("status %v", got)
	}
	again, _ := s.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{})
	if got := again.GetOutput().AsMap()["status"]; got != "already running" {
		t.Fatalf("second Run status %v", got)
	}

	s.wg.Wait()
	if api.bySlug("box-set-10") == nil {
		t.Fatal("background sync did not create the collection")
	}
	if _, ok, _ := host.ReadState(context.Background(), "last_report"); !ok {
		t.Fatal("report not saved")
	}
	next, _ := s.Run(context.Background(), &pluginv1.RunScheduledTaskRequest{})
	last, _ := next.GetOutput().AsMap()["last_run"].(map[string]any)
	if last["created"] != float64(1) {
		t.Fatalf("last_run %v", next.GetOutput().AsMap())
	}
	s.wg.Wait()
}
