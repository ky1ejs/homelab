// A very small Docker Engine API client, used only by the agent role.
//
// Hand-rolled rather than github.com/docker/docker/client on purpose. That
// module pulls in most of Docker's internals, and this file needs exactly three
// read-only endpoints. vault-mcp's go.mod is short for the same reason: fewer
// dependencies is fewer things to audit in the one process on this host that
// holds the daemon socket.
//
// Everything here is a GET. Mutation goes through bin/homelab in agent.go, so
// there is no code path in this file that can change the state of the host.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type dockerClient struct {
	http *http.Client
	// base is the scheme+host the transport expects. For a unix socket the host
	// is a placeholder Docker ignores; the dialer is what actually routes.
	base string
}

// newDockerClient understands the two forms of DOCKER_HOST this stack can be
// given: unix:///var/run/docker.sock (what compose mounts) and tcp://host:port
// (only useful if you ever put a socket proxy in front, which this stack
// deliberately does not -- see README.md#why-not-a-socket-proxy).
func newDockerClient(host string) (*dockerClient, error) {
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		return &dockerClient{
			base: "http://docker",
			http: &http.Client{
				Timeout: 20 * time.Second,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, "unix", path)
					},
				},
			},
		}, nil
	case strings.HasPrefix(host, "tcp://"):
		return &dockerClient{
			base: "http://" + strings.TrimPrefix(host, "tcp://"),
			http: &http.Client{Timeout: 20 * time.Second},
		}, nil
	default:
		return nil, fmt.Errorf("DOCKER_HOST %q: want unix:// or tcp://", host)
	}
}

func (d *dockerClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- wire types -------------------------------------------------------------

type apiContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

type apiImage struct {
	RepoDigests []string `json:"RepoDigests"`
}

const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
)

// containers lists every container on the host, running or not.
//
// all=1 matters: a stack whose containers have exited is precisely the case the
// dashboard exists to make visible, and the default listing hides them.
func (d *dockerClient) containers(ctx context.Context) ([]Container, error) {
	var raw []apiContainer
	if err := d.get(ctx, "/containers/json?all=1", &raw); err != nil {
		return nil, err
	}

	// One inspect per distinct image rather than per container: the vault stack
	// runs four containers off a single image, and this is called on every page
	// load.
	digests := map[string]string{}
	out := make([]Container, 0, len(raw))
	for _, c := range raw {
		if _, seen := digests[c.ImageID]; !seen && c.ImageID != "" {
			digests[c.ImageID] = d.repoDigest(ctx, c.ImageID)
		}
		out = append(out, Container{
			Name:       containerName(c.Names),
			Stack:      c.Labels[labelProject],
			Service:    c.Labels[labelService],
			Image:      c.Image,
			ImageID:    c.ImageID,
			RepoDigest: digests[c.ImageID],
			State:      c.State,
			Status:     c.Status,
			Health:     healthFromStatus(c.Status),
			Created:    c.Created,
		})
	}
	return out, nil
}

// repoDigest returns the registry digest an image was pulled at, or "" if it
// was never pulled from a registry. A failure here is not fatal: it costs the
// update badge for that image, not the page.
func (d *dockerClient) repoDigest(ctx context.Context, imageID string) string {
	var img apiImage
	if err := d.get(ctx, "/images/"+imageID+"/json", &img); err != nil {
		return ""
	}
	for _, rd := range img.RepoDigests {
		if i := strings.Index(rd, "@"); i >= 0 {
			return rd[i+1:]
		}
	}
	return ""
}

// containerName picks the primary name and strips Docker's leading slash.
func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

// healthFromStatus reads the healthcheck verdict out of the human status line,
// e.g. "Up 3 days (healthy)".
//
// Parsing the string rather than inspecting each container is a deliberate
// trade: /containers/{id}/json would give State.Health.Status structurally, but
// that is one extra round trip per container on every page load to learn
// something the listing already carries. The cost is that an unrecognised
// parenthetical yields "" -- which renders as "no healthcheck", the same as a
// container that genuinely has none, and is therefore never misleading in the
// dangerous direction.
func healthFromStatus(status string) string {
	openIdx := strings.LastIndex(status, "(")
	closeIdx := strings.LastIndex(status, ")")
	if openIdx < 0 || closeIdx < openIdx {
		return ""
	}
	switch inner := strings.TrimSpace(status[openIdx+1 : closeIdx]); inner {
	case "healthy", "unhealthy":
		return inner
	case "health: starting":
		return "starting"
	default:
		// "Exited (0) 2 hours ago" lands here, and correctly reports no health.
		return ""
	}
}
