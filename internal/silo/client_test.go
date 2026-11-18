package silo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stub answers like Silo for the calls under test: every request needs the
// API key and the profile, and guarded writes need If-Match (else 428).
func stub(t *testing.T) (*Client, *[]string) {
	t.Helper()
	var calls []string
	guarded := map[string]bool{
		"PATCH /api/v2/admin/collections/c1":           true,
		"DELETE /api/v2/admin/collections/c1":          true,
		"PUT /api/v2/admin/collections/c1/items/order": true,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		calls = append(calls, key)
		if r.Header.Get("Authorization") != "Bearer k" || r.Header.Get("X-Profile-Id") != "p1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if guarded[key] && r.Header.Get("If-Match") == "" {
			w.WriteHeader(http.StatusPreconditionRequired)
			return
		}
		switch key {
		case "GET /api/v2/catalog/items/m1/box-set":
			if r.URL.Query().Get("library_id") != "7" {
				t.Errorf("library_id %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"box_set":{"id":"10","title":"Alien Box Set","poster_url":"https://img/p?sig=1","poster_thumbhash":"PH","items":[{"content_id":"m1"},{"content_id":"m2"}]}}`))
		case "GET /api/v2/catalog/items/m9/box-set":
			_, _ = w.Write([]byte(`{}`))
		case "GET /api/v2/catalog/items/gone/box-set":
			w.WriteHeader(http.StatusNotFound)
		case "PUT /api/v2/admin/collections/c1/poster":
			if err := r.ParseMultipartForm(1 << 20); err != nil || r.FormValue("source_url") != "https://img/p" {
				t.Errorf("artwork form: %v %q", err, r.FormValue("source_url"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "poster_url": "/s/x?sig=2", "poster_thumbhash": "NEW"})
		case "DELETE /api/v2/admin/collections/used":
			w.WriteHeader(http.StatusConflict)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", "k")
	c.UseProfile("p1")
	return c, &calls
}

func TestGuardedWritesSendIfMatch(t *testing.T) {
	c, _ := stub(t)
	ctx := context.Background()
	if err := c.UpdateCollection(ctx, "c1", "t", "d"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := c.OrderMembers(ctx, "c1", []string{"m1"}); err != nil {
		t.Fatalf("order: %v", err)
	}
	if err := c.DeleteCollection(ctx, "c1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var e *Error
	if err := c.DeleteCollection(ctx, "used"); err == nil || !asError(err, &e) || e.Status != http.StatusConflict {
		t.Fatalf("in-use delete: %v", err)
	}
}

func TestItemBoxSet(t *testing.T) {
	c, _ := stub(t)
	ctx := context.Background()
	b, err := c.ItemBoxSet(ctx, "m1", "7")
	if err != nil || b == nil || b.ID != "10" || b.PosterThumbhash != "PH" || strings.Join(b.Members, ",") != "m1,m2" {
		t.Fatalf("box set %+v, %v", b, err)
	}
	for _, id := range []string{"m9", "gone"} {
		if b, err := c.ItemBoxSet(ctx, id, "7"); b != nil || err != nil {
			t.Fatalf("%s: %+v, %v", id, b, err)
		}
	}
}

func TestSetArtworkSource(t *testing.T) {
	c, _ := stub(t)
	got, err := c.SetArtworkSource(context.Background(), "c1", "poster", "https://img/p")
	if err != nil || got.PosterHash != "NEW" {
		t.Fatalf("%+v, %v", got, err)
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	*target = e
	return ok
}

func TestRetriesThrottledRequests(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if calls <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if string(body) != `{"description":"d","title":"t"}` {
			t.Errorf("retried body %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	if err := c.UpdateCollection(context.Background(), "c1", "t", "d"); err != nil || calls != 3 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}

func TestPacesRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL, "k")
	n := requestBurst + int(requestsPerSecond/2) // the burst, then half a second's worth
	start := time.Now()
	for range n {
		if err := c.DeleteCollection(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Fatalf("%d requests in %v: not paced under Silo's API key limit", n, elapsed)
	}
}
