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

// hostHub is a hub that has the named systems. A system in broken answers
// its stats queries with a 500; failLogin refuses every login.
type hostHub struct {
	names     []string
	broken    string
	failLogin bool
}

func (hh hostHub) url(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/collections/users/auth-with-password", func(w http.ResponseWriter, r *http.Request) {
		if hh.failLogin {
			http.Error(w, `{"message":"down"}`, http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"token":"tok","record":{"role":"readonly"}}`)
	})
	mux.HandleFunc("GET /api/collections/{c}/records", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("filter")
		if hh.broken != "" && r.PathValue("c") == "system_stats" && strings.Contains(filter, strconv.Quote(hh.broken)) {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		var items []string
		if r.PathValue("c") == "systems" {
			for _, n := range hh.names {
				if strings.Contains(filter, strconv.Quote(n)) {
					items = append(items, fmt.Sprintf(`{"id":%q,"name":%q,"status":"up"}`, n, n))
				}
			}
		}
		fmt.Fprintf(w, `{"items":[%s]}`, strings.Join(items, ","))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// A hub without the mac studio still sends the NAS's report; a hub without
// the NAS fails the NAS's report, not a quiet Monday.
func TestBuildSkipsAnOptionalHostTheHubLacks(t *testing.T) {
	cfg := config{Email: "r@example.com", Password: "pw", System: "kyles-nas"}
	run := func(hh hostHub) ([]email, []string) {
		cfg.BeszelURL = hh.url(t)
		outs, err := build(context.Background(), cfg, time.Now(), reportHosts(cfg))
		if err != nil {
			t.Fatal(err)
		}
		return split(outs)
	}

	ms, failed := run(hostHub{names: []string{"kyles-nas"}})
	if len(failed) != 0 || len(ms) != 1 || !strings.HasPrefix(ms[0].Subject, "[homelab] weekly") {
		t.Fatalf("NAS only: %d reports, %v", len(ms), failed)
	}

	ms, failed = run(hostHub{names: []string{"kyles-nas", "mac-studio"}})
	if len(failed) != 0 || len(ms) != 2 || !strings.HasPrefix(ms[1].Subject, "[homelab] mac studio weekly") {
		t.Fatalf("both: %d reports, %v", len(ms), failed)
	}

	ms, failed = run(hostHub{names: []string{"mac-studio"}})
	if len(ms) != 1 || len(failed) != 1 || !strings.HasPrefix(failed[0], "kyles-nas: ") {
		t.Fatalf("no NAS: %d reports, failed %v", len(ms), failed)
	}

	// A broken mac studio costs the NAS nothing.
	ms, failed = run(hostHub{names: []string{"kyles-nas", "mac-studio"}, broken: "mac-studio"})
	if len(ms) != 1 || !strings.HasPrefix(ms[0].Subject, "[homelab] weekly") || len(failed) != 1 || !strings.HasPrefix(failed[0], "mac-studio: ") {
		t.Fatalf("broken mac studio: %d reports, failed %v", len(ms), failed)
	}

	// -only names the host, so its absence is an error, not a skip.
	cfg.Only = "mac-studio"
	ms, failed = run(hostHub{names: []string{"kyles-nas"}})
	if len(ms) != 0 || len(failed) != 1 || !strings.Contains(failed[0], "mac-studio") {
		t.Fatalf("-only mac-studio on a hub without it: %d reports, failed %v", len(ms), failed)
	}
}
