// Who is allowed to press the buttons.
//
// THE CREDENTIAL IS THE CONNECTION, NOT A SECRET. This stack is reachable only
// through `tailscale serve` on the NAS's own tailnet node, which terminates TLS
// with a real Let's Encrypt certificate and adds identity headers naming the
// tailnet user behind the request. The dashboard reads Tailscale-User-Login and
// checks it against an allow list. Nothing is typed, nothing is stored, nothing
// expires in a way that leaves you locked out of the tool you opened because
// something was broken.
//
// It replaced a pasted DASH_TOKEN, and the reasoning -- including why WorkOS
// AuthKit, which vault-mcp uses, was not the answer here -- is in
// obsidian-vault/DECISIONS.md#updating-services-identity-at-the-door-digests-in-git.
//
// THIS INVERTS THE OLD POSTURE, AND THAT IS THE THING TO UNDERSTAND BEFORE
// EDITING THIS FILE. Under DASH_TOKEN an attacker needed a 32-byte secret.
// Under a proxy header they need only to reach this listener directly and send
//
//	Tailscale-User-Login: you@example.com
//
// which is a curl away. The header is a credential ONLY because `tailscale
// serve` is the sole path to the listener. Two checks defend that, in two
// different processes, on purpose:
//
//  1. HERE: a request carrying Tailscale-Funnel-Request is refused outright.
//     Funnel traffic gets no identity headers and arrives by the same loopback
//     path Serve does, so nothing else in this file would catch it. Nothing
//     funnels this port today; this is what keeps that true if someone tries.
//
//  2. IN THE AGENT: agent.go asks the daemon how this stack's own web container
//     is actually published, and refuses every mutating and sensitive action if
//     any host binding is not loopback. That is the check that matters, and it
//     deliberately does NOT live here -- see the long note on exposure() for why
//     inspecting r.RemoteAddr cannot answer this question inside a container.
//
// The PAGE is still not gated. It shows container names, states, images and
// update badges, which is what it showed before; putting a wall in front of
// that would mean proving identity to answer "is vault-sync up". What changed is
// the population that can load it at all: reaching it now means being on the
// tailnet, which is a far smaller set than "can reach TCP 8088".
//
// The token gated two sets and still does: actions that change the host
// (mutating) and actions that hand back content the page does not already show
// (sensitive). What stays open -- status, ps, env check, preflight -- tells a
// caller no more than the page they could already load.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	// Set by `tailscale serve` on every request that arrived over the tailnet,
	// naming the user who owns the source node. Tagged nodes have no user and
	// get no header, so they fail closed.
	identityHeader = "Tailscale-User-Login"

	// Set by `tailscale funnel` on requests that came from the public internet.
	// Serve does NOT add identity headers to those, so a funnelled request would
	// otherwise arrive as "no identity" rather than as something to refuse
	// loudly. Refusing it by name says what happened.
	funnelHeader = "Tailscale-Funnel-Request"

	// The header a cross-site form cannot set at all.
	//
	// THIS IS NOW THE ONLY CSRF DEFENCE, where it used to be the second of two.
	// There is no session cookie any more, so SameSite=Strict protects nothing:
	// the proxy attaches identity to a cross-origin request from evil.com as
	// readily as to one from the page itself. A plain form post cannot set a
	// custom header, and a fetch() that sets one triggers a CORS preflight this
	// server never answers -- so requiring it is what keeps a mutating request
	// something only this page can produce. Do not relax it, and do not add CORS
	// headers anywhere in this binary.
	csrfHeader = "X-Homelab-Action"
)

var (
	errReadOnly = errors.New("this dashboard is running read-only: DASH_READ_ONLY is set")
	errFunnel   = errors.New("refusing a request that arrived over a Tailscale Funnel: this stack is tailnet-only")
	errNoAction = errors.New("missing " + csrfHeader + " header")
)

type authenticator struct {
	// logins is the set of tailnet users who may act, from DASH_ALLOWED_LOGINS.
	// Compared case-folded: these are email-shaped identifiers, and a capital
	// letter in .env should be a typo rather than a lockout.
	logins map[string]bool

	// anyLogin honours every account on the tailnet. Deliberately separate from
	// an empty `logins`, so "anyone on my tailnet may deploy" is something the
	// operator states rather than something they get by leaving a variable
	// blank. Same reasoning, and the same variable shape, as vault-mcp's
	// OAUTH_ALLOW_ANY_SUBJECT -- and for the same reason: an empty value that
	// silently means "allow" is how that server was already wide open once.
	anyLogin bool

	// readOnly shows everything and refuses every action with an explanation
	// rather than a 403 that looks like a bug. It used to be spelled "leave
	// DASH_TOKEN blank"; it is now said out loud, because a mode worth
	// supporting is worth stating.
	readOnly bool
}

// newAuthenticator refuses to build a permissive one by accident.
//
// A blank allow list is a startup failure, not "allow everybody". That specific
// mistake -- an empty value being the permissive one -- is how vault-mcp shipped
// an open vault on a public hostname, and the fix there was this same pair of
// variables. See vault-mcp/README.md#pinning-the-subject.
func newAuthenticator(allowed string, anyLogin, readOnly bool) (*authenticator, error) {
	a := &authenticator{
		logins:   map[string]bool{},
		anyLogin: anyLogin,
		readOnly: readOnly,
	}
	for _, l := range strings.Split(allowed, ",") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			a.logins[l] = true
		}
	}

	if !readOnly && !anyLogin && len(a.logins) == 0 {
		return nil, errors.New("DASH_ALLOWED_LOGINS is empty; set it to your tailnet login " +
			"(DASH_ALLOW_ANY_TAILNET_USER=1 to honour every account on the tailnet, " +
			"DASH_READ_ONLY=1 to run with no actions at all)")
	}
	return a, nil
}

func (a *authenticator) ReadOnly() bool { return a.readOnly }

// Identify answers WHO, and only who.
//
// Deliberately separate from whether they may do anything. The page shows the
// login it was reached by even in read-only mode, and even for someone not on
// the allow list -- "you are signed in as X, and X may not deploy" is a far
// better answer than a page that claims not to know who you are. Conflating the
// two questions produced exactly that wrong answer, which is why they are two
// functions.
//
// The errors are written to be shown on the page. Each one is a
// misconfiguration a human has to go and fix, and "forbidden" would send them
// looking in the wrong place.
func (a *authenticator) Identify(r *http.Request) (string, error) {
	if r.Header.Get(funnelHeader) != "" {
		return "", errFunnel
	}

	login := strings.ToLower(strings.TrimSpace(r.Header.Get(identityHeader)))
	if login == "" {
		return "", fmt.Errorf("no %s header: this request did not come through `tailscale serve`, "+
			"or it came from a tagged node, which has no user", identityHeader)
	}
	return login, nil
}

// MayAct answers WHETHER, without the CSRF question.
//
// It is what the page asks to decide between drawing buttons and drawing an
// explanation, because at render time there is no action header to require --
// the page has not been submitted yet.
func (a *authenticator) MayAct(r *http.Request) (string, error) {
	if a.readOnly {
		return "", errReadOnly
	}
	login, err := a.Identify(r)
	if err != nil {
		return "", err
	}
	if !a.anyLogin && !a.logins[login] {
		// Safe to name: it is an identifier the proxy already vouched for, not a
		// credential, and knowing which account was refused is the whole value of
		// saying anything at all.
		return "", fmt.Errorf("%s is not in DASH_ALLOWED_LOGINS", login)
	}
	return login, nil
}

// Authorize is MayAct plus the header a cross-site request cannot set. This is
// the one the action handler calls, and the only one that gates a command.
//
// The identity is returned rather than discarded so a log line can answer "who",
// the way vault-mcp's `sub=` does. A dashboard that records only that a deploy
// happened is strictly less useful than one that records who pressed it, and the
// header is right there.
func (a *authenticator) Authorize(r *http.Request) (string, error) {
	login, err := a.MayAct(r)
	if err != nil {
		return "", err
	}
	if r.Header.Get(csrfHeader) == "" {
		return "", errNoAction
	}
	return login, nil
}
