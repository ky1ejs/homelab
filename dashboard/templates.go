// Helpers the page needs.
//
// html/template escapes by default and nothing here returns template.HTML, so
// every value on the page -- container names, status lines, command output --
// is text. That matters more than it looks: `docker ps` output includes strings
// this repo does not author, and one of the stacks on this host is an agent
// that writes files.
package main

import (
	"fmt"
	"html/template"
	"strings"
	"time"
)

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"shortDigest": shortDigest,
		"since":       since,
		"ago":         ago,
		"stateClass":  stateClass,
		"updateLabel": updateLabel,
		"updateClass": updateClass,
		"actions":     actionsFor,
	}
}

// shortDigest trims sha256:abcdef... to abcdef1. Long enough to compare two by
// eye, short enough not to wrap on a phone.
func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// since renders a Unix creation time as an age. Deliberately coarse: the
// difference between 3 and 4 minutes is never what you came to this page for.
func since(unix int64) string {
	if unix == 0 {
		return ""
	}
	d := time.Since(time.Unix(unix, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ago is since() with the trailing word, and without the "just now ago" that
// concatenating them naively produces.
func ago(unix int64) string {
	s := since(unix)
	if s == "" || s == "just now" {
		return s
	}
	return s + " ago"
}

func stateClass(state string) string {
	switch state {
	case "running":
		return "ok"
	case "restarting", "created", "paused":
		return "warn"
	default:
		return "bad"
	}
}

func updateLabel(u *UpdateStatus) string {
	if u == nil {
		return ""
	}
	switch u.State {
	case UpdateAvailable:
		return "update available"
	case UpdateCurrent:
		return "up to date"
	case UpdatePinned:
		return "pinned"
	default:
		return "unknown"
	}
}

func updateClass(u *UpdateStatus) string {
	if u == nil {
		return "muted"
	}
	switch u.State {
	case UpdateAvailable:
		return "warn"
	case UpdateCurrent:
		return "ok"
	default:
		return "muted"
	}
}

// button is one thing the page offers to do.
type button struct {
	Action   Action
	Label    string
	Mutating bool
	// Confirm is the sentence shown before a mutating action runs. Present for
	// everything that interrupts something: this repo's shared contract says a
	// deploy can cut off an agent run in progress, and the click that does it
	// should not be the same weight as the click that reads a log.
	Confirm string
}

// actionsFor decides which buttons a stack gets.
//
// It mirrors actionSpecs rather than deriving from it, because the agent's copy
// is the one that enforces and this one only decides what to draw. A button
// this function invents that the agent does not accept produces a clear error
// in the output pane, not a privilege.
func actionsFor(s Stack) []button {
	if s.Self {
		// See agent.go: the dashboard does not manage the dashboard.
		out := []button{
			{Action: ActionStatus, Label: "status"},
			{Action: ActionLogs, Label: "logs"},
			{Action: ActionEnvCheck, Label: "env check"},
		}
		// Preflight is a read, so it is not caught by the self-stack refusal --
		// and it is the single most useful button on this card. It asserts the
		// loopback bind, the allow list and that `tailscale serve` is running,
		// which is exactly what you want to press when the exposure banner is up.
		if s.HasPreflight {
			out = append(out, button{Action: ActionPreflight, Label: "preflight"})
		}
		return out
	}

	out := []button{
		{Action: ActionDeploy, Label: "deploy", Mutating: true,
			Confirm: "Pull the newest image and recreate " + s.Name + ". This interrupts anything running in it."},
	}

	if s.Name == "obsidian-vault" {
		out = append(out, button{
			Action: ActionDeploySyncOnly, Label: "deploy (sync only)", Mutating: true,
			Confirm: "Update vault-sync and leave vault-claude's live session alone.",
		})
	}

	out = append(out,
		button{Action: ActionRestart, Label: "restart", Mutating: true,
			Confirm: "Recreate every container in " + s.Name + "."},
		button{Action: ActionStatus, Label: "status"},
		button{Action: ActionPs, Label: "ps"},
		button{Action: ActionLogs, Label: "logs"},
		button{Action: ActionEnvCheck, Label: "env check"},
	)

	if s.HasPreflight {
		out = append(out, button{Action: ActionPreflight, Label: "preflight"})
	}
	if s.Name == "vault-mcp" {
		out = append(out, button{Action: ActionURL, Label: "connector url"})
	}

	return out
}
