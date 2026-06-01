// pass_gate — Plex-Pass gate for HW re-acceleration (scaleplex#78, L3).
//
// Plex HW transcoding is a Plex-Pass-only feature: without an active Pass,
// PMS's TPU emits SW-only argv. The ONE path that could hand a non-Pass user
// HW they're not entitled to is SCALEPLEX_FORCE_HW=1 forcing HW onto an argv
// Plex emitted as SW (Plex chose SW → maybe no Pass). This gate confirms an
// active Pass before THAT path runs.
//
// The cross-backend reshape (#77, translate a foreign HW argv onto the
// worker's backend) is deliberately NOT gated: a foreign HW argv can only
// exist if Plex emitted HW, which Plex itself does only for an active Pass —
// so it's proof of entitlement, and retargeting VAAPI→NVENC grants nothing
// new. Gating it also broke external workers, which can't reach the
// in-cluster SCALEPLEX_PMS_BASE_URL for the L3 probe and so fail-closed every
// cross-backend session (#99).
//
// ENFORCE-WHEN-WIRED, FAIL-CLOSED (Frank 2026-05-28): the gate enforces only
// when the worker is wired to a PMS (SCALEPLEX_PMS_BASE_URL + X_PLEX_TOKEN
// present — the shim always sets both for the progress reporter). When wired,
// it's fail-CLOSED: a query failure/timeout, unparseable, or non-"1"
// subscription DENIES re-accel and the session falls back to Plex's emitted
// (SW) pipeline — trading availability (a PMS hiccup degrades to SW until the
// query recovers) for TOS-safety. When NOT wired (no env — a bare/test worker)
// the gate is inert (allow); an operator can't quietly bypass it by dropping
// the env without also breaking progress reporting + manifest delivery.
//
// Honor-source HW passthrough (Plex emitted HW for its OWN Pass-gated
// decision, worker just runs it) and the cross-backend reshape are NOT gated
// here — both run argv Plex already chose to emit as HW. Only
// SCALEPLEX_FORCE_HW=1 on a SW source consults this.

package main

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// passCheck queries PMS for the account's Plex Pass status. Injectable for
// tests; production uses httpPassCheck.
var passCheck = httpPassCheck

// passGateDebug, when SCALEPLEX_PASS_GATE_DEBUG is set (non-empty), makes the
// gate log every decision — which branch (L1 forwarded flag / not-wired /
// cache / L3 probe), the PMS base, the probe HTTP status, the parsed
// subscription value, and the final allow/deny. The token is NEVER logged.
// Added for #125 item 3: a `force-hw:would-honor-sw` denial on an external
// worker was un-diagnosable because the gate was silent about WHY. Read once
// at startup (process env, not per-session).
var passGateDebug = os.Getenv("SCALEPLEX_PASS_GATE_DEBUG") != ""

func pgDebugf(format string, args ...any) {
	if passGateDebug {
		log.Printf("pass-gate: "+format, args...)
	}
}

var reMyPlexSubscription = regexp.MustCompile(`myPlexSubscription="(\d)"`)

type passCacheEntry struct {
	active bool
	at     time.Time
}

var (
	passCacheMu sync.Mutex
	passCache   = map[string]passCacheEntry{}
)

const passCacheTTL = 5 * time.Minute

// hwAccelAllowed reports whether HW re-acceleration is permitted for this
// session — true only with a confirmed active Plex Pass. Fail-closed. Cached
// per PMS base (passCacheTTL) so it's one query per PMS per 5 min, not per
// session. Query failures are NOT cached (next session retries).
func hwAccelAllowed(inputEnv map[string]string) bool {
	// L1 (#78, #126): the shim runs inside the PMS container and asks the local
	// Plex API for the live Pass state, forwarding it as SCALEPLEX_PASS_ACTIVE
	// per session. When present it's authoritative + fresh — trust it directly
	// and skip the per-session HTTP probe (which can flake and fail-closed a
	// legit Pass user into degraded SW). "1" → allow, anything else ("0") →
	// deny. Absent → fall through to the L3 HTTP probe below.
	//
	// Read from the PER-SESSION inputEnv ONLY (not envFrom, which also reads the
	// worker process env): a static worker-level SCALEPLEX_PASS_ACTIVE would
	// defeat the per-spawn freshness contract + could mis-gate every session.
	if v, ok := inputEnv["SCALEPLEX_PASS_ACTIVE"]; ok && v != "" {
		pgDebugf("L1 forwarded SCALEPLEX_PASS_ACTIVE=%q → %s", v, allowDeny(v == "1"))
		return v == "1"
	}

	base := envFrom(inputEnv, "SCALEPLEX_PMS_BASE_URL")
	tok := envFrom(inputEnv, "X_PLEX_TOKEN")
	if base == "" || tok == "" {
		pgDebugf("not wired (base=%q tok-present=%v) → allow (inert)", base, tok != "")
		// Gate not wired: no PMS base/token means the worker isn't connected
		// to a real PMS via the scaleplex shim (which always sets both for the
		// progress reporter). Treat as "enforcement not configured" → allow.
		// The fail-CLOSED behavior applies once the gate IS wired (below): a
		// present-but-unconfirmable Pass (query failure) denies. An operator
		// can't quietly disable the gate by dropping the env without also
		// breaking progress reporting / manifest delivery.
		return true
	}

	// Composite key (base + token): a bad/expired token must not reuse a
	// good token's "active" entry for the same PMS base.
	key := base + "\x00" + tok
	passCacheMu.Lock()
	if e, ok := passCache[key]; ok && time.Since(e.at) < passCacheTTL {
		passCacheMu.Unlock()
		pgDebugf("L3 cache hit base=%s active=%v → %s", base, e.active, allowDeny(e.active))
		return e.active
	}
	passCacheMu.Unlock()

	active, err := passCheck(base, tok)
	if err != nil {
		pgDebugf("L3 probe base=%s err=%v → deny (fail-closed)", base, err)
		return false // fail-closed; don't cache (retry next session)
	}
	passCacheMu.Lock()
	passCache[key] = passCacheEntry{active: active, at: time.Now()}
	passCacheMu.Unlock()
	pgDebugf("L3 probe base=%s active=%v → %s", base, active, allowDeny(active))
	return active
}

func allowDeny(b bool) string {
	if b {
		return "allow"
	}
	return "deny"
}

// httpPassCheck GETs the PMS root (which carries the myPlexSubscription attr)
// and reports whether it's "1". A missing attr is treated as no-Pass (closed),
// not an error — only transport/IO failures are errors (→ fail-closed +
// retry). 5s timeout bounds the cold-path cost.
func httpPassCheck(base, tok string) (bool, error) {
	// QueryEscape defensively (Plex tokens are alphanumeric in practice, but
	// unescaped reserved chars would corrupt the request).
	reqURL := strings.TrimRight(base, "/") + "/?X-Plex-Token=" + url.QueryEscape(tok)
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(reqURL) //nolint:noctx // short fixed-timeout client
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		pgDebugf("probe http=%d (non-200, e.g. 401 bad token) → no-Pass", resp.StatusCode)
		return false, nil // e.g. 401 bad token → no-Pass (closed)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return false, err
	}
	m := reMyPlexSubscription.FindSubmatch(body)
	if m == nil {
		pgDebugf("probe http=200 but myPlexSubscription attr ABSENT (server token? non-account context?) → no-Pass")
		return false, nil // attr absent → treat as no-Pass
	}
	pgDebugf("probe http=200 myPlexSubscription=%q", string(m[1]))
	return string(m[1]) == "1", nil
}

// envFrom prefers the per-session inputEnv, falling back to the process env.
func envFrom(inputEnv map[string]string, k string) string {
	if v, ok := inputEnv[k]; ok && v != "" {
		return v
	}
	return os.Getenv(k)
}
