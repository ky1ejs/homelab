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

// run serves until SIGTERM, then drains. compose stop sends SIGTERM and waits
// 10s; a deploy triggered from the UI is an in-flight request we would rather
// finish than cut, so the grace period is deliberately shorter than that
// timeout rather than equal to it.
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
	target := "http://127.0.0.1" + addr[strings.LastIndex(addr, ":"):] + "/healthz"

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

func webAddr() string   { return envOr("DASH_ADDR", ":8080") }
func agentAddr() string { return envOr("DASHD_ADDR", ":8090") }
