// Package tvdb is a small client for the parts of TheTVDB v4 API the plugin
// uses: which lists a series is on, and a list's series. Data from TheTVDB
// is shown with attribution to TheTVDB.com, as its free API tier requires.
package tvdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is TheTVDB v4 API.
const DefaultBaseURL = "https://api4.thetvdb.com/v4"

// SiteURL is TheTVDB's website, which attribution links to.
const SiteURL = "https://thetvdb.com"

// ErrNotFound is a 404 from TheTVDB.
var ErrNotFound = errors.New("not found on TheTVDB")

// Error is a non-2xx answer from TheTVDB other than 404.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("TheTVDB answered %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// Client calls TheTVDB with one API key (and PIN, for subscriber keys). It
// sends one request at a time.
type Client struct {
	base   string
	apiKey string
	pin    string
	http   *http.Client

	mu    sync.Mutex // one request at a time; guards token
	token string

	// retryWait is how long a 429 waits before its one retry.
	retryWait time.Duration
}

// New returns a client. base "" uses DefaultBaseURL.
func New(base, apiKey, pin string) *Client {
	if base == "" {
		base = DefaultBaseURL
	}
	return &Client{
		base: strings.TrimRight(base, "/"), apiKey: apiKey, pin: pin,
		http: &http.Client{Timeout: 30 * time.Second}, retryWait: 10 * time.Second,
	}
}

// ListRef is a list a series is on.
type ListRef struct {
	ID   string
	Name string
}

// List is one TheTVDB list with its series, in list order.
type List struct {
	ID       string
	Name     string
	Overview string
	// Slug is the list's address on thetvdb.com, for attribution links.
	Slug string
	// Image is the list's own artwork; empty when TheTVDB has only a
	// fallback image.
	Image    string
	Official bool
	// SeriesIDs are the TheTVDB IDs of the list's series in list order.
	// Movies on the list are left out.
	SeriesIDs []string
}

// PageURL is the list's page on thetvdb.com.
func (l List) PageURL() string {
	if l.Slug != "" {
		return SiteURL + "/lists/" + l.Slug
	}
	return SiteURL + "/lists/" + l.ID
}

type listRecord struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Overview        string `json:"overview"`
	URL             string `json:"url"`
	Image           string `json:"image"`
	ImageIsFallback bool   `json:"imageIsFallback"`
	IsOfficial      bool   `json:"isOfficial"`
	Entities        []struct {
		SeriesID int64 `json:"seriesId"`
		MovieID  int64 `json:"movieId"`
		Order    int64 `json:"order"`
	} `json:"entities"`
}

// SeriesLists returns the official lists a series is on.
func (c *Client) SeriesLists(ctx context.Context, seriesID string) ([]ListRef, error) {
	var out struct {
		Data struct {
			Lists []listRecord `json:"lists"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/series/"+seriesID+"/extended?short=true", &out); err != nil {
		return nil, err
	}
	var refs []ListRef
	for _, l := range out.Data.Lists {
		if l.IsOfficial && l.ID > 0 {
			refs = append(refs, ListRef{ID: strconv.FormatInt(l.ID, 10), Name: l.Name})
		}
	}
	return refs, nil
}

// List returns one list with its series in list order.
func (c *Client) List(ctx context.Context, id string) (List, error) {
	var out struct {
		Data listRecord `json:"data"`
	}
	if err := c.get(ctx, "/lists/"+id+"/extended", &out); err != nil {
		return List{}, err
	}
	d := out.Data
	l := List{
		ID: strconv.FormatInt(d.ID, 10), Name: strings.TrimSpace(d.Name), Overview: strings.TrimSpace(d.Overview),
		Slug: slugOf(d.URL), Official: d.IsOfficial,
	}
	if l.ID == "0" {
		l.ID = id
	}
	if !d.ImageIsFallback {
		l.Image = d.Image
	}
	entities := d.Entities
	sort.SliceStable(entities, func(i, j int) bool { return entities[i].Order < entities[j].Order })
	seen := map[int64]bool{}
	for _, e := range entities {
		if e.SeriesID > 0 && !seen[e.SeriesID] {
			seen[e.SeriesID] = true
			l.SeriesIDs = append(l.SeriesIDs, strconv.FormatInt(e.SeriesID, 10))
		}
	}
	return l, nil
}

// slugOf reads a list's slug from its url field, which TheTVDB gives as the
// slug itself or as a path or address ending in it.
func slugOf(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if i := strings.LastIndexByte(u, '/'); i >= 0 {
		u = u[i+1:]
	}
	return u
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		if err := c.login(ctx); err != nil {
			return err
		}
	}
	status, err := c.send(ctx, path, out)
	if status == http.StatusUnauthorized {
		// The token expired: log in again, once.
		if err := c.login(ctx); err != nil {
			return err
		}
		status, err = c.send(ctx, path, out)
	}
	if status == http.StatusTooManyRequests {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.retryWait):
		}
		_, err = c.send(ctx, path, out)
	}
	return err
}

func (c *Client) send(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, decode(resp, out)
}

func (c *Client) login(ctx context.Context) error {
	in := map[string]string{"apikey": c.apiKey}
	if c.pin != "" {
		in["pin"] = c.pin
	}
	raw, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/login", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := decode(resp, &out); err != nil {
		return fmt.Errorf("logging in to TheTVDB: %w", err)
	}
	if out.Data.Token == "" {
		return errors.New("logging in to TheTVDB: no token in the answer")
	}
	c.token = out.Data.Token
	return nil
}

func decode(resp *http.Response, out any) error {
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &Error{Status: resp.StatusCode, Body: string(raw)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
