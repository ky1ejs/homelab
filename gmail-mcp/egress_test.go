package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestEgressAllowList(t *testing.T) {
	c, err := newEgressClient("https://auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Transport.(*egressTransport)
	for _, host := range []string{"gmail.googleapis.com", "oauth2.googleapis.com", "openidconnect.googleapis.com", "auth.example.com"} {
		if !tr.hosts[host] {
			t.Errorf("%s should be allowed", host)
		}
	}
	for _, u := range []string{
		"https://api.telemetry.example.com/v1/traces",
		"https://gmail.googleapis.com.evil.example/",
		"http://gmail.googleapis.com/", // plain http, even to an allowed host
		"https://accounts.google.com/", // only the browser goes there
	} {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if _, err := c.Do(req); !errors.Is(err, errEgress) {
			t.Errorf("%s: err = %v, want errEgress", u, err)
		}
	}
}

func TestEgressRefusesRedirects(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://"+r.Host+"/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	// The production client, CheckRedirect included, over the stand-in's TLS.
	c := egressClient(map[string]bool{u.Hostname(): true}, srv.Client().Transport.(*http.Transport))
	// The redirect target is the same allowed host, so only CheckRedirect
	// can refuse it.
	if _, err := c.Get(srv.URL); err == nil || !strings.Contains(err.Error(), "redirect refused") {
		t.Errorf("err = %v, want the redirect refused", err)
	}
}

func TestEgressIgnoresProxy(t *testing.T) {
	c, err := newEgressClient("https://auth.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if tr := c.Transport.(*egressTransport).next.(*http.Transport); tr.Proxy != nil {
		t.Error("egress transport honours a proxy from the environment")
	}
}

func TestEgressRejectsNonHTTPSIssuer(t *testing.T) {
	if _, err := newEgressClient("http://auth.example.com"); err == nil {
		t.Error("http issuer accepted")
	}
}
