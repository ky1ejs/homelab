package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The only outbound network path in this server.
//
// gmail-mcp holds a credential that can send mail as you, and it runs on the
// one kind of host in this repo that is reachable from the internet. So every
// outbound request goes through a transport that refuses any host not named
// here, rather than trusting that no code path -- ours or a dependency's --
// ever dials somewhere else. There is no telemetry to switch off because there
// is nowhere for it to go.
//
// Google's consent page (accounts.google.com) is deliberately NOT on the list:
// only your browser visits it, during -login.
var googleHosts = []string{
	"oauth2.googleapis.com",        // token refresh and the -login code exchange
	"openidconnect.googleapis.com", // -login's check of which account consented
	"gmail.googleapis.com",         // the Gmail API itself
}

var errEgress = errors.New("outbound request refused: host is not on the egress allow list")

type egressTransport struct {
	hosts map[string]bool
	next  http.RoundTripper
}

func (t *egressTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" || !t.hosts[r.URL.Hostname()] {
		return nil, fmt.Errorf("%w (%s://%s)", errEgress, r.URL.Scheme, r.URL.Host)
	}
	return t.next.RoundTrip(r)
}

// newEgressClient allows the Google hosts plus the authorization server's own
// host, which go-oidc needs for discovery and its signing keys.
func newEgressClient(issuer string) (*http.Client, error) {
	hosts := map[string]bool{}
	for _, h := range googleHosts {
		hosts[h] = true
	}
	if issuer != "" {
		u, err := url.Parse(issuer)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			return nil, fmt.Errorf("OAUTH_ISSUER must be an https URL, got %q", issuer)
		}
		hosts[u.Hostname()] = true
	}
	return egressClient(hosts, http.DefaultTransport.(*http.Transport)), nil
}

// egressClient wraps a copy of base, with any proxy switched off: the default
// transport honours HTTPS_PROXY, and a proxy is a host nobody put on the list
// that sees every request's destination. Tests pass their own base so a TLS
// stand-in can sit behind the same client production uses.
func egressClient(hosts map[string]bool, base *http.Transport) *http.Client {
	tr := base.Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &egressTransport{hosts: hosts, next: tr},
		// A redirect is a request to a host we did not choose. Every hop would
		// pass through RoundTrip anyway, but nothing here should ever be
		// redirected, so refusing outright is the clearer failure.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("outbound redirect refused")
		},
	}
}
