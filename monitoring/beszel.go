package main

// A read-only client for the Beszel hub's API.
//
// Beszel is a PocketBase application, so this is PocketBase's record API: log in
// against the users collection, then list records with a filter. Nothing here
// writes. The account it logs in as must have Beszel's `readonly` role, and
// login() refuses any other -- the credential sits in a .env file, so it should
// be worth as little as possible to whoever reads that file.
//
// The field names below are Beszel's JSON keys at the version pinned in
// docker-compose.yml (internal/entities/system/system.go upstream). They are
// terse on purpose upstream (`mp` is memory percent) and are spelled out here
// once, in the struct tags, so nothing else in this package has to know them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// pbTime is PocketBase's datetime: "2006-01-02 15:04:05.000Z", always UTC.
type pbTime struct{ time.Time }

const pbLayout = "2006-01-02 15:04:05.000Z"

func (t *pbTime) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return err
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	for _, layout := range []string{pbLayout, "2006-01-02 15:04:05Z", time.RFC3339Nano} {
		if v, err := time.Parse(layout, s); err == nil {
			t.Time = v
			return nil
		}
	}
	return fmt.Errorf("unrecognised PocketBase time %q", s)
}

// pbFilterTime formats a time for use inside a PocketBase filter expression.
func pbFilterTime(t time.Time) string { return t.UTC().Format(pbLayout) }

type systemRecord struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Info   struct {
		Cores    int    `json:"c"`
		Threads  int    `json:"t"`
		CPUModel string `json:"m"`
		Agent    string `json:"v"`
	} `json:"info"`
}

// sysStats is one averaged sample. In a 120m or 480m record every field is a
// mean over the window EXCEPT the Max* fields, which carry the peak. That
// matters for temperature: the hub averages it and keeps no peak, so a
// two-hour record understates a short spike.
type sysStats struct {
	CPU      float64            `json:"cpu"`  // percent
	MaxCPU   float64            `json:"cpum"` // percent, peak within the window
	MemTotal float64            `json:"m"`    // GB
	MemUsed  float64            `json:"mu"`   // GB
	MaxMem   float64            `json:"mm"`   // GB used, peak within the window
	MemPct   float64            `json:"mp"`   // percent
	Swap     float64            `json:"s"`    // GB
	SwapUsed float64            `json:"su"`   // GB
	DiskPct  float64            `json:"dp"`   // percent, root filesystem
	DiskUsed float64            `json:"du"`   // GB
	DiskSize float64            `json:"d"`    // GB
	Temps    map[string]float64 `json:"t"`    // degrees C, by sensor
	ExtraFs  map[string]fsStats `json:"efs"`  // keyed by device or folder name
	LoadAvg  [3]float64         `json:"la"`   // 1, 5, 15 minute
	CPUBreak []float64          `json:"cpub"` // user, system, iowait, steal, idle
}

type fsStats struct {
	Size float64 `json:"d"`  // GB
	Used float64 `json:"du"` // GB
}

type statsRecord struct {
	Created pbTime   `json:"created"`
	Stats   sysStats `json:"stats"`
}

type containerSample struct {
	Name string  `json:"n"`
	CPU  float64 `json:"c"` // percent of the whole host
	Mem  float64 `json:"m"` // MB
}

type containerRecord struct {
	Created pbTime            `json:"created"`
	Stats   []containerSample `json:"stats"`
}

type beszel struct {
	base     string
	email    string
	password string
	http     *http.Client
	token    string
}

func newBeszel(base, email, password string) *beszel {
	return &beszel{
		base:     strings.TrimRight(base, "/"),
		email:    email,
		password: password,
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// errNotReadonly is returned when the reporter's account could change things.
// Refusing is the point: this password sits in .env next to the SMTP
// password, and the less it can do the less a leaked .env costs.
var errNotReadonly = errors.New("beszel account is not readonly")

func (b *beszel) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"identity": b.email, "password": b.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.base+"/api/collections/users/auth-with-password", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Token  string `json:"token"`
		Record struct {
			Role string `json:"role"`
		} `json:"record"`
	}
	if err := b.do(req, &out); err != nil {
		return fmt.Errorf("beszel login: %w", err)
	}
	if out.Record.Role != "readonly" {
		return fmt.Errorf("%w: %s has role %q - give the reporter its own readonly user (README.md#the-reporters-account)",
			errNotReadonly, b.email, out.Record.Role)
	}
	b.token = out.Token
	return nil
}

func (b *beszel) do(req *http.Request, out any) error {
	if b.token != "" {
		req.Header.Set("Authorization", b.token)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// PocketBase errors are {"message": "..."}; fall back to the status.
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, e.Message)
	}
	return json.Unmarshal(data, out)
}

const perPage = 500

// list fetches every record of a collection matching filter, oldest first.
// skipTotal avoids a COUNT(*) per page; the last page is the short one.
func list[T any](ctx context.Context, b *beszel, collection, filter, sort string) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("filter", filter)
		q.Set("sort", sort)
		q.Set("perPage", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(page))
		q.Set("skipTotal", "1")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			b.base+"/api/collections/"+collection+"/records?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var out struct {
			Items []T `json:"items"`
		}
		if err := b.do(req, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if len(out.Items) < perPage {
			return all, nil
		}
		// A runaway loop here would hammer the hub. 50 pages is 25,000 records,
		// far beyond the ~84 a week of 120m samples should ever produce.
		if page >= 50 {
			return nil, fmt.Errorf("%s: more than %d records, refusing to keep paging", collection, page*perPage)
		}
	}
}

// pbQuote quotes a string literal for a PocketBase filter. The values here come
// from our own config, not from a request, but a system name with a quote in it
// should fail to match rather than change the filter.
func pbQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func (b *beszel) system(ctx context.Context, name string) (systemRecord, error) {
	systems, err := list[systemRecord](ctx, b, "systems", "name = "+pbQuote(name), "name")
	if err != nil {
		return systemRecord{}, err
	}
	if len(systems) == 0 {
		return systemRecord{}, fmt.Errorf("no system named %q visible to %s - is SHARE_ALL_SYSTEMS set on the hub?", name, b.email)
	}
	return systems[0], nil
}

// windowFilter selects one system's records of one type in [since, until).
func windowFilter(systemID, kind string, since, until time.Time) string {
	return fmt.Sprintf("system = %s && type = %s && created >= %s && created < %s",
		pbQuote(systemID), pbQuote(kind), pbQuote(pbFilterTime(since)), pbQuote(pbFilterTime(until)))
}

func (b *beszel) systemStats(ctx context.Context, systemID, kind string, since, until time.Time) ([]statsRecord, error) {
	return list[statsRecord](ctx, b, "system_stats", windowFilter(systemID, kind, since, until), "created")
}

func (b *beszel) containerStats(ctx context.Context, systemID, kind string, since, until time.Time) ([]containerRecord, error) {
	return list[containerRecord](ctx, b, "container_stats", windowFilter(systemID, kind, since, until), "created")
}
