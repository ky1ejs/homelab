package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// The only outbound HTTP client in this server, and it reaches one host: the
// authorization server, for discovery and its signing keys. Copied from
// gmail-mcp/egress.go with Google's hosts taken out.
//
// The tailnet node (tsnet, in main.go) dials Tailscale's coordination server
// and its relays itself, below this client. Nothing else in the binary dials
// out: the tools read one local file.

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

// newEgressClient allows the authorization server's host and nothing else.
func newEgressClient(issuer string) (*http.Client, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, fmt.Errorf("OAUTH_ISSUER must be an https URL, got %q", issuer)
	}
	return egressClient(map[string]bool{u.Hostname(): true}, http.DefaultTransport.(*http.Transport)), nil
}

// egressClient wraps a copy of base, with any proxy switched off: a proxy is
// a host nobody put on the list that sees every request's destination. Tests
// pass their own base so a TLS stand-in can sit behind the same client.
func egressClient(hosts map[string]bool, base *http.Transport) *http.Client {
	tr := base.Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &egressTransport{hosts: hosts, next: tr},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("outbound redirect refused")
		},
	}
}
