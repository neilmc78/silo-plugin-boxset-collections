package tvdb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// stub answers like TheTVDB v4: /login hands out a token, and every other
// call needs the current one. expire makes the next call answer 401, as an
// expired token does.
func stub(t *testing.T) (*Client, *int, *bool) {
	t.Helper()
	logins, expire := 0, false
	token := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["apikey"] != "key" || in["pin"] != "1234" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			logins++
			token = "t" + string(rune('0'+logins))
			_, _ = w.Write([]byte(`{"data":{"token":"` + token + `"}}`))
			return
		}
		if expire || r.Header.Get("Authorization") != "Bearer "+token {
			expire = false
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/series/361753/extended":
			if r.URL.Query().Get("short") != "true" {
				t.Errorf("query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data":{"lists":[
				{"id":7,"name":"Yellowstone","isOfficial":true},
				{"id":9,"name":"My favourites","isOfficial":false}]}}`))
		case "/lists/7/extended":
			_, _ = w.Write([]byte(`{"data":{"id":7,"name":" Yellowstone ","overview":"The Dutton family.","url":"yellowstone",
				"image":"https://artworks.thetvdb.com/lists/7.jpg","imageIsFallback":false,"isOfficial":true,
				"entities":[{"seriesId":400,"order":3},{"movieId":55,"order":2},{"seriesId":361753,"order":1},{"seriesId":400,"order":4}]}}`))
		case "/lists/8/extended":
			_, _ = w.Write([]byte(`{"data":{"id":8,"name":"Other","url":"/lists/other","image":"https://artworks.thetvdb.com/fallback.jpg","imageIsFallback":true,"isOfficial":true,"entities":[]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "key", "1234"), &logins, &expire
}

func TestSeriesListsKeepsOfficialOnly(t *testing.T) {
	c, logins, _ := stub(t)
	refs, err := c.SeriesLists(context.Background(), "361753")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != "7" || refs[0].Name != "Yellowstone" {
		t.Fatalf("refs = %+v, want only the official list", refs)
	}
	if *logins != 1 {
		t.Fatalf("logins = %d, want 1", *logins)
	}
}

func TestListOrdersSeriesAndDropsMovies(t *testing.T) {
	c, _, _ := stub(t)
	l, err := c.List(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"361753", "400"}; !slices.Equal(l.SeriesIDs, want) {
		t.Fatalf("series = %v, want %v in list order without movies or repeats", l.SeriesIDs, want)
	}
	if l.Name != "Yellowstone" || !l.Official || l.Image == "" || l.PageURL() != "https://thetvdb.com/lists/yellowstone" {
		t.Fatalf("list = %+v, page %s", l, l.PageURL())
	}
	other, err := c.List(context.Background(), "8")
	if err != nil {
		t.Fatal(err)
	}
	if other.Image != "" || other.PageURL() != "https://thetvdb.com/lists/other" {
		t.Fatalf("fallback list = %+v, want no image and its slug page", other)
	}
}

func TestLogsInAgainOnceWhenTheTokenExpires(t *testing.T) {
	c, logins, expire := stub(t)
	if _, err := c.List(context.Background(), "7"); err != nil {
		t.Fatal(err)
	}
	*expire = true
	if _, err := c.List(context.Background(), "7"); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if *logins != 2 {
		t.Fatalf("logins = %d, want one more after the 401", *logins)
	}
	if _, err := c.List(context.Background(), "404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing list: %v, want ErrNotFound", err)
	}
}

func TestBadKeyFailsLogin(t *testing.T) {
	c, _, _ := stub(t)
	c.pin = "wrong"
	var e *Error
	if _, err := c.SeriesLists(context.Background(), "361753"); !errors.As(err, &e) || e.Status != http.StatusUnauthorized {
		t.Fatalf("err = %v, want a 401 from login", err)
	}
}
