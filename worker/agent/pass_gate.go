// pass_gate — Plex-Pass gate for HW re-acceleration (scaleplex#78, L3).
//
// Plex HW transcoding is a Plex-Pass-only feature: without an active Pass,
// PMS's TPU emits SW-only argv. scaleplex's HW RE-ACCELERATION paths —
// SCALEPLEX_FORCE_HW=1 (force HW on a SW argv) and the cross-backend reshape
// (translate a foreign HW argv onto the worker's backend) — would hand a
// non-Pass user HW transcoding they're not entitled to (TOS violation). This
// gate confirms an active Pass before either path runs.
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
// decision, worker just runs it) is NOT gated here — that's Plex's call, not a
// re-acceleration. Only FORCE_HW + cross-backend reshape consult this.

package main

import (
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// passCheck queries PMS for the account's Plex Pass status. Injectable for
// tests; production uses httpPassCheck.
var passCheck = httpPassCheck

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
	base := envFrom(inputEnv, "SCALEPLEX_PMS_BASE_URL")
	tok := envFrom(inputEnv, "X_PLEX_TOKEN")
	if base == "" || tok == "" {
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
		return e.active
	}
	passCacheMu.Unlock()

	active, err := passCheck(base, tok)
	if err != nil {
		return false // fail-closed; don't cache (retry next session)
	}
	passCacheMu.Lock()
	passCache[key] = passCacheEntry{active: active, at: time.Now()}
	passCacheMu.Unlock()
	return active
}

// httpPassCheck GETs the PMS root (which carries the myPlexSubscription attr)
// and reports whether it's "1". A missing attr is treated as no-Pass (closed),
// not an error — only transport/IO failures are errors (→ fail-closed +
// retry). 5s timeout bounds the cold-path cost.
func httpPassCheck(base, tok string) (bool, error) {
	url := strings.TrimRight(base, "/") + "/?X-Plex-Token=" + tok
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url) //nolint:noctx // short fixed-timeout client
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, nil // e.g. 401 bad token → no-Pass (closed)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return false, err
	}
	m := reMyPlexSubscription.FindSubmatch(body)
	if m == nil {
		return false, nil // attr absent → treat as no-Pass
	}
	return string(m[1]) == "1", nil
}

// envFrom prefers the per-session inputEnv, falling back to the process env.
func envFrom(inputEnv map[string]string, k string) string {
	if v, ok := inputEnv[k]; ok && v != "" {
		return v
	}
	return os.Getenv(k)
}
