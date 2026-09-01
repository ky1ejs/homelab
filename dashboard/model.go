// Types shared by both roles of this binary.
//
// The web role never imports a Docker client and the agent role never renders
// HTML; this file is the whole of the contract between them, and it is JSON on
// purpose. Keeping it narrow is the point of the split described in
// README.md#trust-boundary: if the only thing the web role can say to the agent
// is one of these structs, a bug in the HTML layer cannot become arbitrary
// Docker access.
package main

import (
	"fmt"
	"net"
	"time"
)

// Action is a verb the agent will run. The set is closed -- see actionSpecs in
// agent.go, which is the authority, not this type.
type Action string

const (
	ActionStatus    Action = "status"
	ActionPs        Action = "ps"
	ActionLogs      Action = "logs"
	ActionEnvCheck  Action = "env-check"
	ActionPreflight Action = "preflight"
	ActionURL       Action = "url"
	ActionRestart   Action = "restart"
	ActionDeploy    Action = "deploy"
	// Only meaningful for obsidian-vault: recreate vault-sync and leave the
	// agent's live tmux session alone. See actionSpecs in agent.go.
	ActionDeploySyncOnly Action = "deploy-sync-only"
)

// ActionRequest is the only shape the agent accepts on /v1/action. There is
// deliberately no field for free-form arguments: every flag the agent passes to
// bin/homelab is a constant in actionSpecs, so nothing a browser sends can turn
// into an argv element other than an already-validated stack or service name.
type ActionRequest struct {
	Action  Action `json:"action"`
	Stack   string `json:"stack"`
	Service string `json:"service,omitempty"`
	// No Tail field: it was accepted here and read by nothing, so a caller
	// setting it got bin/homelab's built-in --tail 200 and no hint that its
	// request had been ignored. A field the closed verb list cannot act on does
	// not belong in the contract.
}

// ActionResult carries the command's combined output back verbatim. The output
// is rendered into the page as text, never as HTML -- bin/homelab prints .env
// key names and container status lines, and neither is trusted markup.
type ActionResult struct {
	Action   Action        `json:"action"`
	Stack    string        `json:"stack"`
	Service  string        `json:"service,omitempty"`
	ExitCode int           `json:"exitCode"`
	Output   string        `json:"output"`
	Duration time.Duration `json:"duration"`
	Err      string        `json:"error,omitempty"`
}

// Container is one running (or stopped) container as the agent sees it.
type Container struct {
	Name    string `json:"name"`
	Stack   string `json:"stack"`   // compose project, "" when unmanaged
	Service string `json:"service"` // compose service, "" when unmanaged
	Image   string `json:"image"`   // the reference the container was created from
	ImageID string `json:"imageId"` // sha256:... of the local image
	// RepoDigest is the registry digest of the image this container is actually
	// running, which is what an update check has to compare against. It is empty
	// for images built locally and never pushed.
	RepoDigest string `json:"repoDigest"`
	State      string `json:"state"`  // running, exited, ...
	Status     string `json:"status"` // "Up 3 days (healthy)"
	Health     string `json:"health"` // healthy, unhealthy, starting, "" if no healthcheck
	Created    int64  `json:"created"`
	// Update is filled in by the web role for containers this repo does not
	// manage -- home-assistant, esphome, matter-server. Knowing one of them is
	// behind is useful even though nothing here can deploy it: the lifecycle is
	// Container Station's, the version drift is still yours to notice.
	//
	// Stacks carry their own Update instead, on the stack's own image; this
	// field stays nil for their containers rather than answering the same
	// question twice in two places.
	Update *UpdateStatus `json:"update,omitempty"`
	// NO Published FIELD. Port bindings are read straight off the daemon's
	// listing by the exposure check in agent.go, which is the only thing that
	// asks. Carrying them across the agent/web contract as well would be a field
	// nothing reads -- the same reason ActionRequest has no Tail.
}

// PortBinding is one published port as the daemon reports it. HostIP is the
// field that matters: "127.0.0.1" means only `tailscale serve` (or something
// else on the host) can reach it, and "0.0.0.0" means anything on the LAN can.
type PortBinding struct {
	HostIP        string `json:"hostIp"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
}

// Loopback reports whether this binding is reachable only from the host itself.
//
// An empty HostIP is treated as NOT loopback. The daemon reports "0.0.0.0" for
// a wildcard bind, so empty means a shape this code did not anticipate -- and an
// unrecognised binding must never be the one that passes, because the whole
// value of this check is that it fails closed.
func (p PortBinding) Loopback() bool {
	ip := net.ParseIP(p.HostIP)
	return ip != nil && ip.IsLoopback()
}

// String renders a binding the way docker ps does, for error messages naming
// the offending publish.
func (p PortBinding) String() string {
	host := p.HostIP
	if host == "" {
		host = "?"
	}
	return fmt.Sprintf("%s:%d->%d", host, p.HostPort, p.ContainerPort)
}

// Managed reports whether this container belongs to a stack in the checkout.
// Unmanaged containers (home-assistant, esphome, matter-server) are shown
// because `docker ps` is the ground truth for the host -- see the root
// README -- but they get no action buttons, because this repo does not own
// their lifecycle.
func (c Container) Managed() bool { return c.Stack != "" }

// Stack is one directory in the checkout with a docker-compose.yml.
type Stack struct {
	Name string `json:"name"`
	// Image is the IMAGE value from the stack's .env, i.e. what a deploy would
	// pull. Empty when .env is missing or does not set it.
	Image      string      `json:"image"`
	EnvMode    string      `json:"envMode"` // "600", "" when unreadable
	EnvPresent bool        `json:"envPresent"`
	Containers []Container `json:"containers"`
	// HasPreflight reports whether the stack ships scripts/preflight.sh, which
	// is what decides whether the page draws a preflight button. Derived from
	// the checkout rather than from a list of stack names, so a stack that
	// grows one gets the button without an edit here.
	HasPreflight bool `json:"hasPreflight"`
	// Self marks the stack this dashboard is itself running as. The agent
	// refuses to act on it; see README.md#it-will-not-deploy-itself.
	Self bool `json:"self"`
	// Update is filled in by the web role, not the agent: the registry lookup is
	// an outbound HTTPS call and there is no reason to make it from the process
	// holding the Docker socket.
	Update *UpdateStatus `json:"update,omitempty"`
}

// UpdateState is the outcome of comparing a running image against its registry.
type UpdateState string

const (
	UpdateUnknown   UpdateState = "unknown"   // could not be determined
	UpdateCurrent   UpdateState = "current"   // running digest == registry digest
	UpdateAvailable UpdateState = "available" // registry has moved on
	UpdatePinned    UpdateState = "pinned"    // IMAGE names a digest; nothing to check
)

type UpdateStatus struct {
	State     UpdateState `json:"state"`
	Running   string      `json:"running"` // digest the container is on
	Latest    string      `json:"latest"`  // digest the tag resolves to now
	CheckedAt time.Time   `json:"checkedAt"`
	Err       string      `json:"error,omitempty"`
}

// Exposure answers "is the identity header trustworthy at all".
//
// auth.go treats Tailscale-User-Login as a credential, which it is ONLY while
// `tailscale serve` is the sole route to the web listener. This is that premise
// checked against the daemon rather than assumed from the compose file, because
// the compose file is exactly the kind of thing this dashboard makes it easy to
// change. See agent.go's exposure().
type Exposure struct {
	// OK is false when the premise does not hold, or could not be checked. The
	// agent refuses every mutating and sensitive action while it is false.
	OK bool `json:"ok"`
	// Reason is written to be shown on the page and read by a human who now has
	// to go and fix something.
	Reason string `json:"reason,omitempty"`
	// Bindings names the offending publishes, so the fix is obvious.
	Bindings []string `json:"bindings,omitempty"`
}

// Checkout is the git state of the repo the agent runs commands out of.
//
// It exists because of a wrong answer this page used to give confidently: the
// checkout is mounted read-only, so `homelab update` is not reachable from the
// browser, and pressing deploy therefore pulls a NEW IMAGE AGAINST WHATEVER
// COMPOSE FILE IS ALREADY ON DISK. Nothing said so. Now something does.
//
// Read straight out of .git by the agent -- no git binary, no exec, and no
// write to a read-only mount. How far behind the checkout is cannot be answered
// from here, because answering it needs a fetch and a fetch is a write; the web
// role asks GitHub instead. See github.go.
type Checkout struct {
	// Head is the commit the checkout is on, or "" when .git could not be read.
	Head string `json:"head"`
	// Branch is the branch HEAD points at, "" when detached.
	Branch string `json:"branch"`
	// Slug is owner/repo parsed from origin's URL, which is what the web role
	// needs to ask GitHub anything. Empty when there is no origin remote.
	Slug string `json:"slug"`
	// Err explains why the fields above are empty.
	Err string `json:"error,omitempty"`
	// Behind is filled in by the WEB role, not the agent: it is an outbound
	// HTTPS call, and registry.go already establishes that those are not made
	// from the process holding the Docker socket.
	Behind *BehindStatus `json:"behind,omitempty"`
}

// BehindStatus is how far the checkout is from the branch it tracks.
type BehindStatus struct {
	// Status is GitHub's comparison verdict: identical, behind, ahead, diverged.
	Status string `json:"status"`
	// Count is how many commits the checkout is missing.
	Count int `json:"count"`
	// Commits are the missing ones, newest first, subject lines only and capped.
	Commits []CommitSummary `json:"commits,omitempty"`
	// Stacks names the stacks whose files those commits touch, so the page can
	// say "deploy vault-mcp after applying this" rather than only "you are
	// behind".
	Stacks    []string  `json:"stacks,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	Err       string    `json:"error,omitempty"`
}

type CommitSummary struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// State is the whole snapshot the agent hands to the web role.
type State struct {
	Stacks    []Stack     `json:"stacks"`
	Unmanaged []Container `json:"unmanaged"`
	// StacksErr is set when bin/homelab could not be asked which stacks exist.
	// Distinct from DockerErr: this one means the page cannot list stacks at
	// all, and every button would have failed anyway.
	StacksErr string `json:"stacksError,omitempty"`
	// DockerErr is set when the daemon could not be reached at all. The page
	// still renders -- knowing the dashboard cannot see Docker is more useful
	// than an error page that hides which stacks exist.
	DockerErr string    `json:"dockerError,omitempty"`
	TakenAt   time.Time `json:"takenAt"`
	// Exposure is the agent's verdict on whether this stack is published the way
	// the auth design requires. Never omitempty: a missing verdict must render
	// as "not OK", not as an absent field the template quietly skips.
	Exposure Exposure `json:"exposure"`
	Checkout Checkout `json:"checkout"`
}
