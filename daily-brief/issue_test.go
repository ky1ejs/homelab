package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseIssueAcceptsSample(t *testing.T) {
	is := sampleIssue("2026-11-28")
	got, err := ParseIssue(bytes.NewReader(issueJSON(t, is)), "2026-11-28")
	if err != nil {
		t.Fatal(err)
	}
	if got.Sections[0].Items[1].Source != "FT" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestParseIssueRefusesShape(t *testing.T) {
	good := string(issueJSON(t, sampleIssue("2026-11-28")))
	for name, in := range map[string]string{
		"unknown field": strings.Replace(good, `"title":`, `"extra":1,"title":`, 1),
		"two values":    good + good,
		"not json":      "hello",
		"wrong type":    strings.Replace(good, `"sections":[`, `"sections":"x","y":[`, 1),
		"bad utf8":      strings.Replace(good, "quiet", "qu\xffiet", 1),
		"too big":       good[:len(good)-1] + `,"x":"` + strings.Repeat("a", maxIssueBytes) + `"}`,
	} {
		_, err := ParseIssue(strings.NewReader(in), "2026-11-28")
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: got %v, want a ValidationError", name, err)
			continue
		}
		if strings.Contains(err.Error(), "aaaa") || strings.Contains(err.Error(), "hello") {
			t.Errorf("%s: error quotes the input: %v", name, err)
		}
	}
}

func TestValidateFields(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Issue)
		want   string
	}{
		"other date":      {func(is *Issue) { is.Date = "2026-11-27" }, "date: must be 2026-11-28"},
		"bad date":        {func(is *Issue) { is.Date = "2026-02-30" }, "date: must be YYYY-MM-DD"},
		"empty title":     {func(is *Issue) { is.Title = "" }, "title: empty"},
		"long title":      {func(is *Issue) { is.Title = strings.Repeat("a", maxTitle+1) }, "title: 91 characters"},
		"newline":         {func(is *Issue) { is.Summary = "a\nb" }, "summary: contains a control"},
		"bidi override":   {func(is *Issue) { is.Sections[0].Items[0].Source = "BBC‮" }, "sections[0].items[0].source: contains"},
		"zero width":      {func(is *Issue) { is.Sections[0].Items[0].Headline = "a​b" }, "headline: contains"},
		"nbsp":            {func(is *Issue) { is.Title = "a b" }, "title: contains"},
		"padded":          {func(is *Issue) { is.Title = " a" }, "title: starts or ends with whitespace"},
		"no sections":     {func(is *Issue) { is.Sections = nil }, "sections: need 1 to 8, got 0"},
		"empty section":   {func(is *Issue) { is.Sections[1].Items = nil }, "sections[1].items: need 1 to 6, got 0"},
		"http":            {func(is *Issue) { is.Sections[0].Items[0].URL = "http://example.com/" }, "must start with https://"},
		"javascript":      {func(is *Issue) { is.Sections[0].Items[0].URL = "javascript:alert(1)" }, "must start with https://"},
		"opaque":          {func(is *Issue) { is.Sections[0].Items[0].URL = "https:example.com" }, "absolute link"},
		"userinfo":        {func(is *Issue) { is.Sections[0].Items[0].URL = "https://u:p@example.com/" }, "user name or password"},
		"single label":    {func(is *Issue) { is.Sections[0].Items[0].URL = "https://localhost/" }, "public host"},
		"space in url":    {func(is *Issue) { is.Sections[0].Items[0].URL = "https://a.com/x y" }, "plain ASCII"},
		"unicode url":     {func(is *Issue) { is.Sections[0].Items[0].URL = "https://exämple.com/" }, "plain ASCII"},
		"too many items":  {func(is *Issue) { is.Sections[0].Items = append(is.Sections[0].Items, make([]Item, 5)...) }, "need 1 to 6, got 7"},
		"too much speech": {func(is *Issue) { tooMuchSpeech(is) }, "characters to read aloud"},
		"encoded word":    {func(is *Issue) { is.Title = "=?utf-8?q?Fake?=" }, `title: must not contain "=?"`},
	}
	for name, c := range cases {
		is := sampleIssue("2026-11-28")
		c.mutate(is)
		err := is.Validate("2026-11-28")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func tooMuchSpeech(is *Issue) {
	is.Sections = nil
	for i := 0; i < 4; i++ {
		s := Section{Title: "Section"}
		for j := 0; j < 6; j++ {
			s.Items = append(s.Items, Item{Headline: "H", Text: strings.Repeat("a", maxText), Source: "S", URL: "https://a.com/"})
		}
		is.Sections = append(is.Sections, s)
	}
}

func TestValidateCapsProblems(t *testing.T) {
	is := sampleIssue("2026-11-28")
	for i := range is.Sections {
		for j := range is.Sections[i].Items {
			is.Sections[i].Items[j] = Item{}
		}
	}
	var ve *ValidationError
	if err := is.Validate("2026-11-28"); !errors.As(err, &ve) || len(ve.Problems) != maxProblems {
		t.Fatalf("got %v, want exactly %d problems", err, maxProblems)
	}
}

func TestValidDate(t *testing.T) {
	for s, want := range map[string]bool{
		"2026-11-28": true, "2028-02-29": true, "2026-02-29": false,
		"2026-1-28": false, "../../etc": false, "2026-11-28x": false, "": false,
	} {
		if validDate(s) != want {
			t.Errorf("validDate(%q) = %v", s, !want)
		}
	}
}

func TestCmdValidatePrintsProblems(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/d.json"
	is := sampleIssue("2026-11-28")
	is.Title = ""
	writeFile(t, path, issueJSON(t, is))
	var out bytes.Buffer
	if err := cmdValidate(path, "2026-11-28", &out); err == nil {
		t.Fatal("want an error")
	}
	if out.String() != "title: empty\n" {
		t.Fatalf("got %q", out.String())
	}
	out.Reset()
	writeFile(t, path, issueJSON(t, sampleIssue("2026-11-28")))
	if err := cmdValidate(path, "2026-11-28", &out); err != nil || out.Len() != 0 {
		t.Fatalf("valid issue: %v %q", err, out.String())
	}
}

// encoding/json matches keys without regard to case, so {"text":"A",
// "Text":"B"} decodes as B while voice.py reads A. Only the exact keys pass.
func TestParseIssueRefusesCaseVariantKeys(t *testing.T) {
	good := string(issueJSON(t, sampleIssue("2026-11-28")))
	for name, c := range map[string]struct{ in, want string }{
		"item": {strings.Replace(good, `"text":"The Bank kept rates on hold."`,
			`"text":"The Bank kept rates on hold.","Text":"Something else."`, 1), `sections[0].items[0]: unknown field "Text"`},
		"section": {strings.Replace(good, `"title":"AI and software"`, `"Title":"AI and software"`, 1), `sections[1]: unknown field "Title"`},
		"top":     {strings.Replace(good, `"summary":`, `"SUMMARY":`, 1), `issue: unknown field "SUMMARY"`},
	} {
		_, err := ParseIssue(strings.NewReader(c.in), "2026-11-28")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
