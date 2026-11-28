package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// An Issue is one day's brief as the agent writes it: drafts/<date>.json,
// copied by write.sh to published/<date>/script.json once `daily-brief
// validate` accepts it.
//
// IT IS ATTACKER-INFLUENCED INPUT. The agent that writes it reads the open web
// all morning, so anything in it may have been put there by a page it read.
// Every reader checks it again rather than trusting the copy step: write.sh
// before publishing, voice.py in its box with no network, and `publish`, which
// holds the Gmail and Spotify credentials. What validation buys is a bound, not
// trust: plain text of a known size, and links that are https and nothing else.
// It cannot tell a true sentence from a false one, and nothing here claims to.
//
// The schema is also the agent's: agent-instructions.md describes it, and a
// change to a limit here is a change to that file.
type Issue struct {
	Date     string    `json:"date"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary"`
	Sections []Section `json:"sections"`
}

type Section struct {
	Title string `json:"title"`
	Items []Item `json:"items"`
}

type Item struct {
	Headline string `json:"headline"`
	Text     string `json:"text"`
	Source   string `json:"source"`
	URL      string `json:"url"`
}

// Limits, in characters (runes). maxSpoken bounds the podcast: Kokoro reads
// about 15 characters a second, so 18,000 is about 20 minutes of audio, and on
// the J4125 anything from half an hour to two hours to render. The
// instructions ask for about half that; this is the ceiling, not the target.
// voice.py keeps its own, slightly larger, ceiling for the same reason.
const (
	maxIssueBytes      = 256 << 10
	maxTitle           = 90
	maxSummary         = 500
	maxSections        = 8
	maxSectionTitle    = 40
	maxItemsPerSection = 6
	maxItems           = 30
	maxHeadline        = 120
	maxText            = 900
	maxSource          = 60
	maxURL             = 500
	maxSpoken          = 18000
	maxProblems        = 12
)

var dateRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// validDate is the one test of a date name, used for the JSON field and for
// every directory name the publisher and the voice job will act on.
func validDate(s string) bool {
	if !dateRE.MatchString(s) {
		return false
	}
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// ValidationError lists every problem found, up to maxProblems, so that one
// fix-up pass by the agent can address them all. Its text goes back to the
// agent verbatim, and into a failure email, so it must never quote more of
// the input than a field's path.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return strings.Join(e.Problems, "; ")
}

// LoadIssue reads and validates one issue. want is the date the file must
// carry, which is the date in its path: an issue that names another day is
// refused rather than sent under the wrong subject line.
func LoadIssue(path, want string) (*Issue, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseIssue(f, want)
}

func ParseIssue(r io.Reader, want string) (*Issue, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxIssueBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxIssueBytes {
		return nil, &ValidationError{[]string{fmt.Sprintf("file is over %d bytes", maxIssueBytes)}}
	}
	if !utf8.Valid(raw) {
		return nil, &ValidationError{[]string{"file is not valid UTF-8"}}
	}
	if p := exactKeys(raw); len(p) > 0 {
		return nil, &ValidationError{p}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var is Issue
	if err := dec.Decode(&is); err != nil {
		return nil, &ValidationError{[]string{"not the expected JSON: " + jsonProblem(err)}}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &ValidationError{[]string{"more than one JSON value in the file"}}
	}
	if err := is.Validate(want); err != nil {
		return nil, err
	}
	return &is, nil
}

// exactKeys refuses any key that is not one of the schema's own, letter for
// letter. encoding/json matches keys case-insensitively, so it reads
// {"text": "A", "Text": "B"} as B and DisallowUnknownFields lets it through,
// while voice.py's json.loads keeps both and reads "text", A: the email would
// say one thing and the podcast another. Shapes it cannot walk are left to the
// strict decode after it, which reports them.
func exactKeys(raw []byte) []string {
	type object = map[string]json.RawMessage
	var p []string
	check := func(path string, m object, want ...string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !slices.Contains(want, k) && len(p) < maxProblems {
				p = append(p, fmt.Sprintf("%s: unknown field %q; the fields are %s, in lower case",
					path, truncate(k, 40), strings.Join(want, ", ")))
			}
		}
	}
	var top object
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	check("issue", top, "date", "title", "summary", "sections")
	var sections []object
	if json.Unmarshal(top["sections"], &sections) != nil {
		return p
	}
	for i, s := range sections {
		sp := fmt.Sprintf("sections[%d]", i)
		check(sp, s, "title", "items")
		var items []object
		if json.Unmarshal(s["items"], &items) != nil {
			continue
		}
		for j, it := range items {
			check(fmt.Sprintf("%s.items[%d]", sp, j), it, "headline", "text", "source", "url")
		}
	}
	return p
}

// jsonProblem keeps encoding/json's message for the structural errors, which
// name a field or an offset, and drops the rest. A syntax error's text holds
// no input; an unknown-field error names the field, which the agent needs.
func jsonProblem(err error) string {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		return fmt.Sprintf("syntax error at byte %d", se.Offset)
	case errors.As(err, &te):
		return fmt.Sprintf("%s should be %s", te.Field, te.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "unknown field " + truncate(strings.TrimPrefix(err.Error(), "json: unknown field "), 40)
	default:
		return "unreadable"
	}
}

func (is *Issue) Validate(want string) error {
	var p []string
	add := func(format string, a ...any) {
		if len(p) < maxProblems {
			p = append(p, fmt.Sprintf(format, a...))
		}
	}
	text := func(path, s string, max int) {
		if problem := checkText(s, max); problem != "" {
			add("%s: %s", path, problem)
		}
	}

	switch {
	case !validDate(is.Date):
		add("date: must be YYYY-MM-DD")
	case want != "" && is.Date != want:
		add("date: must be %s, the day this issue is for", want)
	}
	text("title", is.Title, maxTitle)
	// The title is the email's subject. mime.QEncoding leaves plain ASCII as
	// it is, so a title of "=?utf-8?b?...?=" would reach the header as an
	// encoded word, which mail clients decode: a way back in for the format
	// characters checkText refuses.
	if strings.Contains(is.Title, "=?") {
		add("title: must not contain \"=?\"")
	}
	text("summary", is.Summary, maxSummary)

	if n := len(is.Sections); n == 0 || n > maxSections {
		add("sections: need 1 to %d, got %d", maxSections, n)
	}
	items := 0
	spoken := runes(is.Title) + runes(is.Summary)
	for i, s := range is.Sections {
		sp := fmt.Sprintf("sections[%d]", i)
		text(sp+".title", s.Title, maxSectionTitle)
		spoken += runes(s.Title)
		if n := len(s.Items); n == 0 || n > maxItemsPerSection {
			add("%s.items: need 1 to %d, got %d", sp, maxItemsPerSection, n)
		}
		for j, it := range s.Items {
			ip := fmt.Sprintf("%s.items[%d]", sp, j)
			text(ip+".headline", it.Headline, maxHeadline)
			text(ip+".text", it.Text, maxText)
			text(ip+".source", it.Source, maxSource)
			if problem := checkURL(it.URL); problem != "" {
				add("%s.url: %s", ip, problem)
			}
			spoken += runes(it.Headline) + runes(it.Text)
			items++
		}
	}
	if items > maxItems {
		add("sections: %d items in all, the most is %d", items, maxItems)
	}
	if spoken > maxSpoken {
		add("issue: %d characters to read aloud, the most is %d", spoken, maxSpoken)
	}
	if len(p) > 0 {
		return &ValidationError{p}
	}
	return nil
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// checkText allows plain single-line prose and nothing else. Control
// characters would let a field break a mail header or a log line; format
// characters (bidi overrides, zero-width joiners) let text display as
// something other than what it says, which in a link's label is the whole of a
// phishing trick. Leading or trailing space is refused rather than trimmed so
// that the Go and Python readers cannot disagree about what a field holds.
func checkText(s string, max int) string {
	if s == "" {
		return "empty"
	}
	if n := runes(s); n > max {
		return fmt.Sprintf("%d characters, the most is %d", n, max)
	}
	if strings.TrimSpace(s) != s {
		return "starts or ends with whitespace"
	}
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
			(unicode.IsSpace(r) && r != ' ') {
			return "contains a control, format or non-space whitespace character; plain text on one line only"
		}
	}
	return ""
}

// checkURL allows an absolute https link to a named host, with no
// credentials in it. html/template would also neutralise a javascript: link
// in the email, but the podcast's in-player links have no such second guard.
func checkURL(s string) string {
	if s == "" {
		return "empty"
	}
	if len(s) > maxURL {
		return fmt.Sprintf("over %d characters", maxURL)
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return "must be plain ASCII with no spaces (percent-encode the rest)"
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "not a URL"
	}
	switch {
	case u.Scheme != "https":
		return "must start with https://"
	case u.Opaque != "" || u.Host == "":
		return "must be an absolute link to a website"
	case u.User != nil:
		return "must not carry a user name or password"
	case !strings.Contains(u.Hostname(), "."):
		return "must name a public host"
	}
	return ""
}

func truncate(s string, n int) string {
	if runes(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "..."
}
