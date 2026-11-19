package boxsets

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Silo gives a plugin at most 256 instance state keys, of up to 256 KB each,
// and no way to delete one. So the plugin keeps a small, fixed set of keys:
// every map is stored in chunks of at most chunkBytes, under
// "<name>:<n>" with the count in "<name>:chunks". A map of any size costs a
// few keys, and the same keys are reused on every save.
//
//   - cache:<library>, tvdb2:<library>: per library title caches.
//   - artwork: collection ID -> the artwork the plugin set on it.
//   - tvdblists: TheTVDB list ID -> its details and when they were fetched.
//   - owned: "<library>/<slug>" -> the ID of a collection the plugin made or
//     adopted, or deletedMark once its admin deleted it.
//   - last_report: the previous sync's report.
const chunkBytes = 200 << 10

const (
	artworkMap = "artwork"
	listsMap   = "tvdblists"
	ownedMap   = "owned"
	// deletedMark is how 1.1.4 recorded a box set collection its admin
	// deleted. 1.1.5 moves each into the excluded box sets setting.
	deletedMark = "deleted"
)

func chunkKey(name string, n int) string { return fmt.Sprintf("%s:%d", name, n) }
func chunkCountKey(name string) string   { return name + ":chunks" }

// loadMap reads a chunked map; a missing one is empty.
func (s *Syncer) loadMap(ctx context.Context, name string) (map[string]string, error) {
	out := map[string]string{}
	raw, ok, err := s.host.ReadState(ctx, chunkCountKey(name))
	if err != nil || !ok {
		return out, err
	}
	var chunks int
	if err := json.Unmarshal(raw, &chunks); err != nil {
		return out, nil
	}
	for i := 0; i < chunks; i++ {
		raw, ok, err := s.host.ReadState(ctx, chunkKey(name, i))
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

// saveMap writes a map in chunks of at most chunkBytes, reusing the keys of
// the previous save. Chunks a smaller map no longer needs are left as they
// are; the count key says how many to read.
func (s *Syncer) saveMap(ctx context.Context, name string, m map[string]string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	chunks := 0
	part := map[string]string{}
	size := 2
	flush := func() error {
		raw, err := json.Marshal(part)
		if err != nil {
			return err
		}
		if err := s.host.WriteState(ctx, chunkKey(name, chunks), raw); err != nil {
			return stateError(err)
		}
		chunks++
		part, size = map[string]string{}, 2
		return nil
	}
	for _, k := range keys {
		// Quoting and escaping can grow an entry; budget for the worst case.
		entry := 2*(len(k)+len(m[k])) + 6
		if len(part) > 0 && size+entry > chunkBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		part[k] = m[k]
		size += entry
	}
	if len(part) > 0 {
		if err := flush(); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(chunks)
	if err := s.host.WriteState(ctx, chunkCountKey(name), raw); err != nil {
		return stateError(err)
	}
	return nil
}

// errStateFull explains Silo's key limit in the words the Last sync box
// shows.
const errStateFull = "Silo's storage for this plugin is full (256 keys, left by older versions of the plugin). Uninstall and reinstall the plugin once to clear it; your collections stay"

// stateError rewords Silo's "too many keys" refusal so the user knows what
// to do about it.
func stateError(err error) error {
	if err != nil && strings.Contains(err.Error(), "maximum of 256 keys") {
		return fmt.Errorf("%s: %w", errStateFull, err)
	}
	return err
}

// syncState is the shared state one sync reads once and saves once.
type syncState struct {
	artwork map[string]string
	lists   map[string]string
	owned   map[string]string
	dirty   map[string]bool
	// excluding are TheTVDB lists whose franchise collections the admin
	// deleted this sync, to add to the excluded lists setting.
	excluding map[string]bool
	// excludingBoxSets are box sets whose collections the admin deleted,
	// to add to the excluded box sets setting.
	excludingBoxSets map[string]bool
	// artFailures are images the sync could not set.
	artFailures []string
}

func (st *syncState) set(name string, m map[string]string, key, value string) {
	if m[key] == value {
		return
	}
	m[key] = value
	st.dirty[name] = true
}

func (st *syncState) drop(name string, m map[string]string, key string) {
	if _, ok := m[key]; !ok {
		return
	}
	delete(m, key)
	st.dirty[name] = true
}

func (s *Syncer) loadState(ctx context.Context) (*syncState, error) {
	st := &syncState{dirty: map[string]bool{}, excluding: map[string]bool{}, excludingBoxSets: map[string]bool{}}
	var err error
	if st.artwork, err = s.loadMap(ctx, artworkMap); err != nil {
		return nil, err
	}
	if st.lists, err = s.loadMap(ctx, listsMap); err != nil {
		return nil, err
	}
	if st.owned, err = s.loadMap(ctx, ownedMap); err != nil {
		return nil, err
	}
	return st, nil
}

// saveState writes the maps this sync changed.
func (s *Syncer) saveState(ctx context.Context, st *syncState) error {
	if st == nil {
		return nil
	}
	for name, m := range map[string]map[string]string{artworkMap: st.artwork, listsMap: st.lists, ownedMap: st.owned} {
		if !st.dirty[name] {
			continue
		}
		if err := s.saveMap(ctx, name, m); err != nil {
			return fmt.Errorf("saving %s: %w", name, err)
		}
		st.dirty[name] = false
	}
	return nil
}

func ownedKey(libraryID, slug string) string { return libraryID + "/" + slug }
