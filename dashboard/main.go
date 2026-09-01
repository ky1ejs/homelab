// homelab-dash: a web overview of the stacks on the NAS, with the buttons to
// update and restart them.
//
// ONE BINARY, TWO ROLES, and that is the whole design:
//
//	homelab-dash -role=web     serves the HTML. No Docker socket. No checkout.
//	homelab-dash -role=agent   holds the Docker socket. Serves no HTML.
//
// Everything the browser can cause to happen has to survive a trip through the
// agent's closed verb list first. The reasoning, and why this repo now mounts
// /var/run/docker.sock at all after refusing to three times, is in
// README.md#trust-boundary and obsidian-vault/DECISIONS.md#the-dashboard-and-the-docker-socket.
//
// Go rather than bash, unlike bin/homelab: this one parses untrusted HTTP input
// and renders HTML, which is exactly the case DECISIONS.md#runtime-node-not-bun-or-go
// says a compiler earns its keep on. It shares vault-mcp's conventions -- stdlib
// only, tests as the security assertion, -version and -healthcheck flags.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Version is stamped by the Dockerfile with -ldflags. "dev" outside a build.
var Version = "dev"

func main() {
	var (
		role        = flag.String("role", envOr("DASH_ROLE", "web"), "web or agent")
		showVersion = flag.Bool("version", false, "print version and exit")
		healthcheck = flag.Bool("healthcheck", false, "probe this container's own listener and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		return
	}

	// Same convention as vault-mcp: the binary is its own healthcheck, so the
	// runtime image needs no curl.
	if *healthcheck {
		if err := probeSelf(*role); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("[dash] ")

	var (
		srv *http.Server
		err error
	)
	switch *role {
	case "web":
		srv, err = newWebServer()
	case "agent":
		srv, err = newAgentServer()
	default:
		err = fmt.Errorf("unknown -role %q: want web or agent", *role)
	}
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("role=%s version=%s listening on %s", *role, Version, srv.Addr)
	run(srv)
}

// run serves until SIGTERM, then drains.
//
// The 8s is sized against compose stop's 10s SIGKILL and nothing else: it lets
// in-flight REQUESTS return rather than having their connections cut, and
// finishes before the kill.
//
// It emphatically does NOT save an in-flight deploy. Stopping this stack while
// one is running kills it, because the whole container goes away -- the 20
// minutes an action is allowed only ever protects it from the clock and from
// the caller hanging up, never from the host stopping the stack. Deploying the
// dashboard is the one thing it will not do to itself, so the case only arises
// from an SSH session, where you can see it happen.
func run(srv *http.Server) {
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(idle)
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-idle
}

func probeSelf(role string) error {
	addr := webAddr()
	if role == "agent" {
		addr = agentAddr()
	}
	// The listener binds :8080; the probe has to dial a host, not a bare port.
	// SplitHostPort rather than slicing on the last colon: a DASH_ADDR of "8080"
	// -- the likeliest way to get this wrong -- has no colon at all, and slicing
	// on an index of -1 panics. A healthcheck that crashes tells you nothing;
	// one that names the variable tells you everything.
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("healthcheck: %q is not a listen address (want host:port or :port): %w", addr, err)
	}
	target := "http://127.0.0.1:" + port + "/healthz"

	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(target)
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: %s returned %s", target, resp.Status)
	}
	return nil
}

// --- environment ------------------------------------------------------------

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("%s=%q is not a positive duration, using %s", key, v, fallback)
		return fallback
	}
	return d
}

func envIntOr(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("%s=%q is not a positive integer, using %d", key, v, fallback)
		return fallback
	}
	return n
}

// envBool reads a flag that must be set deliberately.
//
// Only "1", "true" and "yes" enable it, and everything else -- including an
// empty value -- is false. That asymmetry is the point: every caller of this is
// a variable whose permissive setting must be something the operator states
// rather than something they get by leaving a value blank or mistyped. Same
// reasoning as vault-mcp's OAUTH_ALLOW_ANY_SUBJECT.
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func webAddr() string   { return envOr("DASH_ADDR", ":8080") }
func agentAddr() string { return envOr("DASHD_ADDR", ":8090") }
