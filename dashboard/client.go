// The web role's client for the agent role.
//
// Small on purpose. The web process cannot ask the agent for anything that is
// not one of these two calls, which is the enforcement point of the split: even
// if the HTML layer is entirely compromised, the reachable surface is
// "snapshot" and "one verb from a closed list".
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type agentClient struct {
	base  string
	token string
	http  *http.Client
}

func newAgentClient(base, token string) *agentClient {
	return &agentClient{
		base:  base,
		token: token,
		// No global timeout: a deploy holds the connection open for as long as
		// the pull takes, and the agent already bounds that per action. Per-call
		// contexts below supply the shorter deadlines.
		http: &http.Client{},
	}
}

func (c *agentClient) State(ctx context.Context) (State, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/state", nil)
	if err != nil {
		return State{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return State{}, fmt.Errorf("agent unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return State{}, fmt.Errorf("agent /v1/state: %s", resp.Status)
	}

	var st State
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&st); err != nil {
		return State{}, err
	}
	return st, nil
}

// Do runs one action. The deadline is the caller's: the handler gives a deploy
// longer than a restart, matching the agent's own per-verb timeouts.
func (c *agentClient) Do(ctx context.Context, action ActionRequest) (ActionResult, error) {
	body, err := json.Marshal(action)
	if err != nil {
		return ActionResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/action", bytes.NewReader(body))
	if err != nil {
		return ActionResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return ActionResult{}, fmt.Errorf("agent unreachable: %w", err)
	}
	defer resp.Body.Close()

	var res ActionResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&res); err != nil {
		return ActionResult{}, fmt.Errorf("agent /v1/action: %s", resp.Status)
	}
	return res, nil
}
