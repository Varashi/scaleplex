// Worker → orchestrator self-registration (PUSH discovery).
//
// Activated when SCALEPLEX_ORCHESTRATOR_URL is set. The worker POSTs a
// {host, port, version} envelope to <orchestrator>/register at startup
// and re-POSTs the same envelope every SCALEPLEX_HEARTBEAT_SECONDS
// (default 5). The orchestrator's /register is idempotent — same
// endpoint, same body, no separate heartbeat path.
//
// Designed for the Docker / no-DNS multi-host case where the operator
// can't easily set up a DNS hostname that resolves to every worker.
// See deploy/docker/multi-host.md and project_scaleplex_docker_deployment.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHeartbeatSeconds = 5
	maxBackoffSeconds       = 30
)

// registerPayload is what the worker POSTs to /register. Matches the
// orchestrator's registerRequest struct field-for-field.
type registerPayload struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Version string `json:"version,omitempty"`
}

// startRegisterLoop spawns a goroutine that keeps the worker registered
// with the orchestrator. No-op if SCALEPLEX_ORCHESTRATOR_URL is unset
// (preserves existing k8s/DNS deployments byte-for-byte).
//
// The host the worker advertises is, in order of precedence:
//  1. SCALEPLEX_WORKER_HOST env (operator-supplied; right answer for
//     untrusted networks or when os.Hostname() is unhelpful).
//  2. os.Hostname() — useful on docker compose where the container
//     name resolves on the user-defined bridge.
//
// The port is the worker's listen port (defaultWorkerListenPort) — the
// orchestrator must be able to dial http://host:port for /capability
// + /task. If the worker is behind NAT relative to the orchestrator,
// PUSH discovery won't work; use a reachable host or set up DNS/LIST.
func startRegisterLoop() {
	orch := strings.TrimSpace(os.Getenv("SCALEPLEX_ORCHESTRATOR_URL"))
	if orch == "" {
		return
	}
	orch = strings.TrimRight(orch, "/")

	host := resolveAdvertisedHost()
	if host == "" {
		log.Printf("register: SCALEPLEX_ORCHESTRATOR_URL set but couldn't resolve advertised host — set SCALEPLEX_WORKER_HOST")
		return
	}
	port := defaultWorkerListenPort

	interval := time.Duration(envSecondsOr("SCALEPLEX_HEARTBEAT_SECONDS", defaultHeartbeatSeconds)) * time.Second
	if interval <= 0 {
		interval = defaultHeartbeatSeconds * time.Second
	}

	payload := registerPayload{
		Host:    host,
		Port:    port,
		Version: os.Getenv("SCALEPLEX_VERSION"),
	}

	body, _ := json.Marshal(payload)
	log.Printf("register: PUSH loop start → %s/register (host=%s port=%d every=%s)",
		orch, host, port, interval)

	go registerLoop(context.Background(), orch+"/register", body, interval, http.DefaultClient)
}

// registerLoop is the test-friendly inner loop: ticks at `interval`,
// posts `body` to `url`, retries with exponential backoff on failure
// (capped at maxBackoffSeconds). Exits when ctx is cancelled.
func registerLoop(ctx context.Context, url string, body []byte, interval time.Duration, client *http.Client) {
	backoff := interval
	for {
		err := postRegister(ctx, url, body, client)
		if err == nil {
			backoff = interval
		} else {
			log.Printf("register: %v (retry in %s)", err, backoff)
		}

		sleep := interval
		if err != nil {
			sleep = backoff
			next := backoff * 2
			if next > maxBackoffSeconds*time.Second {
				next = maxBackoffSeconds * time.Second
			}
			backoff = next
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
	}
}

// postRegister sends one register/heartbeat POST. Returns nil on
// 2xx response.
func postRegister(ctx context.Context, url string, body []byte, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("unexpected status %d", resp.StatusCode)
}

// resolveAdvertisedHost picks the host string the worker should send
// to the orchestrator.
func resolveAdvertisedHost() string {
	if v := strings.TrimSpace(os.Getenv("SCALEPLEX_WORKER_HOST")); v != "" {
		return v
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return ""
	}
	return h
}

// envSecondsOr matches the orchestrator's helper. Local copy here so the
// worker doesn't grow a cross-package dep on the orchestrator just for
// this. Returns dflt on missing/malformed input.
func envSecondsOr(k string, dflt int) int {
	v := os.Getenv(k)
	if v == "" {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return dflt
	}
	return n
}

// defaultWorkerListenPort mirrors the constant the agent listens on.
// Hardcoded today (listenAddr = ":3501"); change in lockstep if that
// becomes operator-configurable.
const defaultWorkerListenPort = 3501
