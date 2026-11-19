// Package silo is a small client for the parts of Silo's /api/v2 the plugin
// uses: the movie catalog, box sets, and library collections.
package silo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is a 404 from Silo.
var ErrNotFound = errors.New("not found")

// Error is a non-2xx answer from Silo other than 404.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("silo answered %d: %s", e.Status, strings.TrimSpace(e.Body))
}

// Silo rate-limits API keys: a standard-tier key gets 20 requests a second
// with a burst of 20, and is answered 429 above that. The client keeps to
// half of it, leaving room for anything else using the key.
const (
	requestsPerSecond = 10.0
	requestBurst      = 10
	maxRetries        = 4
	maxRetryWait      = 30 * time.Second
)

// Client calls one Silo server with an API key, acting as one profile.
type Client struct {
	base      string
	apiKey    string
	profileID string
	http      *http.Client
	pace      *pacer
}

// pacer is a token bucket: Wait returns once a request may go.
type pacer struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newPacer() *pacer { return &pacer{tokens: requestBurst, last: time.Now()} }

func (p *pacer) Wait(ctx context.Context) error {
	for {
		p.mu.Lock()
		now := time.Now()
		p.tokens = min(requestBurst, p.tokens+now.Sub(p.last).Seconds()*requestsPerSecond)
		p.last = now
		if p.tokens >= 1 {
			p.tokens--
			p.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - p.tokens) / requestsPerSecond * float64(time.Second))
		p.mu.Unlock()
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// New returns a client for the server at base (for example
// "http://127.0.0.1:8080"). Call UseProfile before profile-scoped calls.
func New(base, apiKey string) *Client {
	return &Client{
		base:   strings.TrimRight(base, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: 60 * time.Second},
		pace:   newPacer(),
	}
}

// UseProfile sets the profile catalog reads act as.
func (c *Client) UseProfile(id string) { c.profileID = id }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any, headers map[string]string) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if c.profileID != "" {
		req.Header.Set("X-Profile-Id", c.profileID)
	}
	resp, err := c.doPaced(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &Error{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// doPaced sends a request to Silo within the rate limit, and retries one
// Silo throttles (429) or cannot serve right now (503), waiting as long as
// Retry-After asks, else 1s, 2s, 4s, 8s.
func (c *Client) doPaced(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		if err := c.pace.Wait(ctx); err != nil {
			return nil, err
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable) ||
			attempt == maxRetries || req.GetBody == nil && req.Body != nil {
			return resp, nil
		}
		wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		if req.GetBody != nil {
			if req.Body, err = req.GetBody(); err != nil {
				return nil, err
			}
		}
	}
}

func retryAfter(header string, attempt int) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && s >= 0 {
		return min(time.Duration(s)*time.Second, maxRetryWait)
	}
	return min(time.Second<<attempt, maxRetryWait)
}

// Profile is one profile of the account the API key belongs to.
type Profile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsPrimary bool   `json:"is_primary"`
}

// PrimaryProfile returns the account's primary profile.
func (c *Client) PrimaryProfile(ctx context.Context) (Profile, error) {
	var out struct {
		Items []Profile `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v2/profiles", nil, nil, &out, nil); err != nil {
		return Profile{}, err
	}
	for _, p := range out.Items {
		if p.IsPrimary {
			return p, nil
		}
	}
	if len(out.Items) > 0 {
		return out.Items[0], nil
	}
	return Profile{}, errors.New("the API key's account has no profile")
}

// Movies returns the content IDs of every movie in a library.
func (c *Client) Movies(ctx context.Context, libraryID string) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		q := url.Values{"library_id": {libraryID}, "type": {"movie"}, "limit": {"200"}, "skip_total": {"true"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Items []struct {
				ContentID string `json:"content_id"`
			} `json:"items"`
			Page struct {
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := c.do(ctx, http.MethodGet, "/api/v2/catalog", q, nil, &page, nil); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.ContentID != "" {
				ids = append(ids, it.ContentID)
			}
		}
		if page.Page.NextCursor == "" || page.Page.NextCursor == cursor || len(page.Items) == 0 {
			return ids, nil
		}
		cursor = page.Page.NextCursor
	}
}

// SeriesRef is one series of a library with its TheTVDB ID ("" when it has
// none).
type SeriesRef struct {
	ContentID string
	TvdbID    string
}

// Series returns every series in a library with its TheTVDB ID.
func (c *Client) Series(ctx context.Context, libraryID string) ([]SeriesRef, error) {
	var out []SeriesRef
	cursor := ""
	for {
		q := url.Values{"library_id": {libraryID}, "type": {"series"}, "limit": {"200"}, "skip_total": {"true"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Items []struct {
				ContentID string `json:"content_id"`
				TvdbID    string `json:"tvdb_id"`
			} `json:"items"`
			Page struct {
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := c.do(ctx, http.MethodGet, "/api/v2/catalog", q, nil, &page, nil); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			if it.ContentID != "" {
				out = append(out, SeriesRef{ContentID: it.ContentID, TvdbID: strings.TrimSpace(it.TvdbID)})
			}
		}
		if page.Page.NextCursor == "" || page.Page.NextCursor == cursor || len(page.Items) == 0 {
			return out, nil
		}
		cursor = page.Page.NextCursor
	}
}

// BoxSet is one box set of a library, as Silo stores it.
type BoxSet struct {
	ID          string
	Title       string
	Overview    string
	PosterURL   string
	BackdropURL string
	// The thumbhashes identify the artwork while its signed URLs change.
	PosterThumbhash   string
	BackdropThumbhash string
	// Members are the content IDs of its movies in the library, in
	// collection order.
	Members []string
}

type boxSetDetail struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Overview    string `json:"overview"`
	PosterURL   string `json:"poster_url"`
	BackdropURL string `json:"backdrop_url"`
	PosterHash  string `json:"poster_thumbhash"`
	BackHash    string `json:"backdrop_thumbhash"`
	Items       []struct {
		ContentID string `json:"content_id"`
	} `json:"items"`
}

func (d boxSetDetail) boxSet() BoxSet {
	b := BoxSet{ID: d.ID, Title: d.Title, Overview: d.Overview, PosterURL: d.PosterURL, BackdropURL: d.BackdropURL,
		PosterThumbhash: d.PosterHash, BackdropThumbhash: d.BackHash}
	for _, it := range d.Items {
		if it.ContentID != "" {
			b.Members = append(b.Members, it.ContentID)
		}
	}
	return b
}

// artworkQuery asks for posters and backdrops large enough to keep.
func artworkQuery(libraryID string) url.Values {
	return url.Values{"library_id": {libraryID}, "poster_width": {"780"}, "backdrop_width": {"1280"}}
}

// ItemBoxSet returns the box set a movie belongs to in a library, with its
// movies, or nil when it belongs to none that forms there (too few movies,
// or box sets off).
func (c *Client) ItemBoxSet(ctx context.Context, contentID, libraryID string) (*BoxSet, error) {
	var out struct {
		BoxSet *boxSetDetail `json:"box_set"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v2/catalog/items/"+url.PathEscape(contentID)+"/box-set",
		artworkQuery(libraryID), nil, &out, nil)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil || out.BoxSet == nil {
		return nil, err
	}
	b := out.BoxSet.boxSet()
	return &b, nil
}

// BoxSet returns one box set of a library, or ErrNotFound when the library
// has box sets off or too few of its movies to form it.
func (c *Client) BoxSet(ctx context.Context, id, libraryID string) (BoxSet, error) {
	var out boxSetDetail
	if err := c.do(ctx, http.MethodGet, "/api/v2/catalog/box-sets/"+url.PathEscape(id), artworkQuery(libraryID), nil, &out, nil); err != nil {
		return BoxSet{}, err
	}
	return out.boxSet(), nil
}

// Collection is a library collection as the admin API lists it.
type Collection struct {
	ID              string  `json:"id"`
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	Slug            string  `json:"slug"`
	CollectionType  string  `json:"collection_type"`
	GroupID         *string `json:"group_id"`
	PosterURL       string  `json:"poster_url"`
	PosterIsCollage bool    `json:"poster_is_collage"`
	PosterHash      string  `json:"poster_thumbhash"`
	BackdropURL     string  `json:"backdrop_url"`
	BackdropHash    string  `json:"backdrop_thumbhash"`
	ManagementMode  string  `json:"management_mode"`
}

// Collections lists a library's collections.
func (c *Client) Collections(ctx context.Context, libraryID string) ([]Collection, error) {
	var out struct {
		Items []Collection `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v2/admin/collections", url.Values{"library_id": {libraryID}}, nil, &out, nil)
	return out.Items, err
}

// Group is a library's collection group.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// Groups lists a library's collection groups.
func (c *Client) Groups(ctx context.Context, libraryID string) ([]Group, error) {
	var out struct {
		Items []Group `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v2/admin/libraries/"+url.PathEscape(libraryID)+"/collection-groups", nil, nil, &out, nil)
	return out.Items, err
}

// CreateGroup creates a collection group in a library.
func (c *Client) CreateGroup(ctx context.Context, libraryID, name, slug string) (Group, error) {
	var out Group
	in := map[string]any{"name": name, "slug": slug, "default_sort_mode": "name_asc"}
	err := c.do(ctx, http.MethodPost, "/api/v2/admin/libraries/"+url.PathEscape(libraryID)+"/collection-groups", nil, in, &out, nil)
	return out, err
}

// NewCollection is what CreateCollection creates.
type NewCollection struct {
	LibraryID   string
	Title       string
	Description string
	Slug        string
	GroupID     string
}

// CreateCollection creates a manual collection.
func (c *Client) CreateCollection(ctx context.Context, n NewCollection) (Collection, error) {
	in := map[string]any{
		"title":           n.Title,
		"description":     n.Description,
		"slug":            n.Slug,
		"collection_type": "manual",
		"library_id":      n.LibraryID,
		"library_ids":     []string{n.LibraryID},
	}
	if n.GroupID != "" {
		in["group_id"] = n.GroupID
	}
	var out Collection
	err := c.do(ctx, http.MethodPost, "/api/v2/admin/collections", nil, in, &out, nil)
	return out, err
}

// ifMatchAny satisfies the precondition Silo requires on guarded writes
// (PATCH and DELETE of a collection, member reorder); without it they answer
// 428. The plugin owns these collections, so it overwrites unconditionally.
var ifMatchAny = map[string]string{"If-Match": "*"}

// UpdateCollection patches a collection's title and description.
func (c *Client) UpdateCollection(ctx context.Context, id, title, description string) error {
	in := map[string]any{"title": title, "description": description}
	return c.do(ctx, http.MethodPatch, "/api/v2/admin/collections/"+url.PathEscape(id), nil, in, nil, ifMatchAny)
}

// DeleteCollection deletes a collection.
func (c *Client) DeleteCollection(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v2/admin/collections/"+url.PathEscape(id), nil, nil, nil, ifMatchAny)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// Members returns a collection's members' content IDs in order.
func (c *Client) Members(ctx context.Context, collectionID string) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		q := url.Values{"limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Items []struct {
				MediaItemID string `json:"media_item_id"`
			} `json:"items"`
			Page struct {
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := c.do(ctx, http.MethodGet, "/api/v2/admin/collections/"+url.PathEscape(collectionID)+"/items", q, nil, &page, nil); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			ids = append(ids, it.MediaItemID)
		}
		if page.Page.NextCursor == "" || page.Page.NextCursor == cursor || len(page.Items) == 0 {
			return ids, nil
		}
		cursor = page.Page.NextCursor
	}
}

// AddMember adds a movie to a collection at a position.
func (c *Client) AddMember(ctx context.Context, collectionID, contentID string, position int) error {
	return c.do(ctx, http.MethodPut, "/api/v2/admin/collections/"+url.PathEscape(collectionID)+"/items/"+url.PathEscape(contentID),
		nil, map[string]any{"position": position}, nil, nil)
}

// RemoveMember removes a movie from a collection.
func (c *Client) RemoveMember(ctx context.Context, collectionID, contentID string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v2/admin/collections/"+url.PathEscape(collectionID)+"/items/"+url.PathEscape(contentID), nil, nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// OrderMembers sets the order of a collection's members.
func (c *Client) OrderMembers(ctx context.Context, collectionID string, contentIDs []string) error {
	return c.do(ctx, http.MethodPut, "/api/v2/admin/collections/"+url.PathEscape(collectionID)+"/items/order",
		nil, map[string]any{"ordered_ids": contentIDs}, nil, ifMatchAny)
}

// Fetch downloads an image Silo pointed at. A relative address is on this
// server and carries the API key; an absolute one (a signed storage URL) is
// fetched as is.
func (c *Client) Fetch(ctx context.Context, address string) ([]byte, string, error) {
	absolute := strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://")
	target := address
	if !absolute {
		target = c.base + "/" + strings.TrimLeft(address, "/")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	own := !absolute || strings.HasPrefix(target, c.base+"/")
	if own {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		if c.profileID != "" {
			req.Header.Set("X-Profile-Id", c.profileID)
		}
	}
	var resp *http.Response
	if own {
		resp, err = c.doPaced(req)
	} else {
		resp, err = c.http.Do(req)
	}
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, "", err
	}
	return raw, resp.Header.Get("Content-Type"), nil
}

// UploadArtwork sets a collection's poster or backdrop ("poster" or
// "backdrop") from image bytes, and returns the collection after it.
func (c *Client) UploadArtwork(ctx context.Context, collectionID, kind string, image []byte, contentType string) (Collection, error) {
	if contentType == "" {
		contentType = http.DetectContentType(image)
	}
	return c.putArtwork(ctx, collectionID, kind, func(w *multipart.Writer) error {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="image"; filename="`+kind+extension(contentType)+`"`)
		h.Set("Content-Type", contentType)
		part, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = part.Write(image)
		return err
	})
}

// SetArtworkSource has Silo download a collection's poster or backdrop from
// an absolute URL itself. Silo refuses private addresses, so a signed URL on
// a LAN storage server fails here and the caller uploads the bytes instead.
func (c *Client) SetArtworkSource(ctx context.Context, collectionID, kind, sourceURL string) (Collection, error) {
	return c.putArtwork(ctx, collectionID, kind, func(w *multipart.Writer) error {
		return w.WriteField("source_url", sourceURL)
	})
}

func (c *Client) putArtwork(ctx context.Context, collectionID, kind string, fill func(*multipart.Writer) error) (Collection, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := fill(w); err != nil {
		return Collection{}, err
	}
	if err := w.Close(); err != nil {
		return Collection{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.base+"/api/v2/admin/collections/"+url.PathEscape(collectionID)+"/"+kind, &buf)
	if err != nil {
		return Collection{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var out Collection
	err = c.send(req, &out)
	return out, err
}

func extension(contentType string) string {
	switch {
	case strings.Contains(contentType, "png"):
		return ".png"
	case strings.Contains(contentType, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}
