package main

import (
	"context"
	"errors"
	"strings"

	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"

	"github.com/neilmc78/silo-plugin-boxset-collections/internal/boxsets"
)

// host adapts the SDK's RuntimeHost client to what the sync needs.
type host struct{}

var errNoHost = errors.New("the Silo host is not connected yet")

func client() (*runtimehost.Client, error) {
	if c := runtime.Host(); c != nil {
		return c, nil
	}
	return nil, errNoHost
}

func (host) BaseURL(ctx context.Context) (string, error) {
	c, err := client()
	if err != nil {
		return "", err
	}
	info, err := c.GetHostInfo(ctx)
	if err != nil {
		return "", err
	}
	for _, u := range []string{info.InternalBaseURL, info.PublicBaseURL} {
		if u = strings.TrimSpace(u); u != "" {
			return u, nil
		}
	}
	return "", errors.New("Silo reported no address for its API; set the Silo URL in the plugin's settings")
}

// nonMovieTypes are library types that never hold movies.
var nonMovieTypes = map[string]bool{
	"tv": true, "series": true, "shows": true, "audiobook": true, "audiobooks": true,
	"ebook": true, "ebooks": true, "book": true, "books": true, "podcast": true, "podcasts": true,
	"music": true, "manga": true,
}

func (host) MovieLibraries(ctx context.Context) ([]boxsets.Library, error) {
	c, err := client()
	if err != nil {
		return nil, err
	}
	libs, err := c.ListLibraries(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]boxsets.Library, 0, len(libs))
	for _, l := range libs {
		if nonMovieTypes[strings.ToLower(strings.TrimSpace(l.GetMediaType()))] {
			continue
		}
		out = append(out, boxsets.Library{ID: l.GetId(), Name: l.GetName()})
	}
	return out, nil
}

func (host) ReadState(ctx context.Context, key string) ([]byte, bool, error) {
	c, err := client()
	if err != nil {
		return nil, false, err
	}
	return c.ReadInstanceState(ctx, key)
}

func (host) WriteState(ctx context.Context, key string, value []byte) error {
	c, err := client()
	if err != nil {
		return err
	}
	return c.WriteInstanceState(ctx, key, value)
}
