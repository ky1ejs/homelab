package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeHub answers the handful of PocketBase routes the reporter uses.
func fakeHub(t *testing.T, role string, records int) (*httptest.Server, *[]string) {
	t.Helper()
	var filters []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/collections/users/auth-with-password", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["password"] != "pw" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"Failed to authenticate."}`)
			return
		}
		fmt.Fprintf(w, `{"token":"tok","record":{"role":%q}}`, role)
	})
	mux.HandleFunc("GET /api/collections/{c}/records", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		filters = append(filters, q.Get("filter"))
		page, _ := strconv.Atoi(q.Get("page"))
		per, _ := strconv.Atoi(q.Get("perPage"))
		var items []string
		for i := (page - 1) * per; i < records && i < page*per; i++ {
			items = append(items, fmt.Sprintf(`{"id":"s%d","name":"kyles-nas","created":"2026-10-22 08:00:00.000Z","stats":{"cpu":%d}}`, i, i))
		}
		fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &filters
}

func TestLoginRefusesAnAccountThatCanWrite(t *testing.T) {
	for _, role := range []string{"admin", "user", ""} {
		srv, _ := fakeHub(t, role, 0)
		err := newBeszel(srv.URL, "r@x", "pw").login(context.Background())
		if !errors.Is(err, errNotReadonly) {
			t.Errorf("role %q: want errNotReadonly, got %v", role, err)
		}
	}
}

func TestLoginAcceptsReadonly(t *testing.T) {
	srv, _ := fakeHub(t, "readonly", 0)
	b := newBeszel(srv.URL, "r@x", "pw")
	if err := b.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.token != "tok" {
		t.Fatalf("token not kept: %q", b.token)
	}
}

func TestLoginSurfacesPocketBaseMessage(t *testing.T) {
	srv, _ := fakeHub(t, "readonly", 0)
	err := newBeszel(srv.URL, "r@x", "wrong").login(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Failed to authenticate") {
		t.Fatalf("want the hub's message in the error, got %v", err)
	}
}

func TestListPagesUntilAShortPage(t *testing.T) {
	srv, _ := fakeHub(t, "readonly", perPage+3)
	b := newBeszel(srv.URL, "r@x", "pw")
	if err := b.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	got, err := b.systemStats(context.Background(), "abc", "120m", since, since.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != perPage+3 {
		t.Fatalf("got %d records, want %d", len(got), perPage+3)
	}
	if got[0].Created.Year() != 2026 {
		t.Fatalf("created not parsed: %v", got[0].Created)
	}
}

func TestFilterQuotesValues(t *testing.T) {
	srv, filters := fakeHub(t, "readonly", 1)
	b := newBeszel(srv.URL, "r@x", "pw")
	if err := b.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.system(context.Background(), `nas" || name != "`); err != nil {
		t.Fatal(err)
	}
	want := `name = "nas\" || name != \""`
	if (*filters)[0] != want {
		t.Fatalf("filter = %s\nwant     %s", (*filters)[0], want)
	}
}

func TestStatsFilterUsesPocketBaseTime(t *testing.T) {
	srv, filters := fakeHub(t, "readonly", 0)
	b := newBeszel(srv.URL, "r@x", "pw")
	if err := b.login(context.Background()); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 22, 9, 0, 0, 0, time.FixedZone("BST", 3600))
	if _, err := b.systemStats(context.Background(), "abc", "480m", since, since.AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}
	// Bounded on both sides: a retry ten minutes late must not count samples
	// from after its own window.
	want := `system = "abc" && type = "480m" && created >= "2026-10-22 08:00:00.000Z" && created < "2026-10-29 08:00:00.000Z"`
	if (*filters)[0] != want {
		t.Fatalf("filter = %s\nwant     %s", (*filters)[0], want)
	}
}

func TestSysStatsDecodesBeszelKeys(t *testing.T) {
	// A trimmed 120m record as the hub stores it at the pinned version.
	raw := `{"created":"2026-10-28 06:00:00.000Z","stats":{"cpu":12.5,"cpum":96,"m":15.5,"mu":9.3,"mm":13.9,
		"mp":60,"s":4,"su":0.5,"d":100,"du":40,"dp":40,"t":{"cpu_thermal":55},"efs":{"data":{"d":3600,"du":2600}},
		"la":[1.1,1.2,1.3],"cpub":[5,2,3.5,0,89.5]}}`
	var r statsRecord
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	s := r.Stats
	if s.CPU != 12.5 || s.MaxCPU != 96 || s.MemTotal != 15.5 || s.MaxMem != 13.9 || s.MemPct != 60 ||
		s.SwapUsed != 0.5 || s.LoadAvg[2] != 1.3 || s.CPUBreak[2] != 3.5 || s.Temps["cpu_thermal"] != 55 ||
		s.ExtraFs["data"].Used != 2600 {
		t.Fatalf("decoded wrong: %+v", s)
	}
}
