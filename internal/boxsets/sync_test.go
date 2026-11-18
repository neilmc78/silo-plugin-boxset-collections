package boxsets

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/silo"
)

type fakeHost struct{ state map[string][]byte }

func (h *fakeHost) BaseURL(context.Context) (string, error) { return "http://silo", nil }
func (h *fakeHost) MovieLibraries(context.Context) ([]Library, error) {
	return []Library{{ID: "lib", Name: "Movies"}}, nil
}
func (h *fakeHost) ReadState(_ context.Context, k string) ([]byte, bool, error) {
	v, ok := h.state[k]
	return v, ok, nil
}
func (h *fakeHost) WriteState(_ context.Context, k string, v []byte) error {
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
	var out []string
	for id := range f.movieBoxSet {
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}
func (f *fakeAPI) ItemBoxSetID(_ context.Context, cid, _ string) (string, error) {
	f.lookups = append(f.lookups, cid)
	return f.movieBoxSet[cid], nil
}
func (f *fakeAPI) BoxSet(_ context.Context, id, _ string) (silo.BoxSet, error) {
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
func (f *fakeAPI) UploadArtwork(_ context.Context, id, kind string, _ []byte, _ string) error {
	f.uploads = append(f.uploads, id+":"+kind)
	if kind == "poster" {
		f.collections[id].PosterURL = "uploaded"
	} else {
		f.collections[id].BackdropURL = "uploaded"
	}
	return nil
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
	if !slices.Equal(api.uploads, []string{"old:poster"}) {
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
