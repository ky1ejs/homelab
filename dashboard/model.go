// Types shared by both roles of this binary.
//
// The web role never imports a Docker client and the agent role never renders
// HTML; this file is the whole of the contract between them, and it is JSON on
// purpose. Keeping it narrow is the point of the split described in
// README.md#trust-boundary: if the only thing the web role can say to the agent
// is one of these structs, a bug in the HTML layer cannot become arbitrary
// Docker access.
package main

import "time"

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
	Tail    int    `json:"tail,omitempty"`
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
}
