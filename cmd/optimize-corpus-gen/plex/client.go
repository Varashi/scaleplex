// Package plex is a minimal REST client for the subset of the Plex Media
// Server HTTP API that the optimize-corpus generator needs: prefs,
// library sections + items, Optimize jobs, transcode sessions.
//
// All endpoints accept ?X-Plex-Token=<token> as the auth mechanism (the
// X-Plex-Token header works too; we use the query form because Plex's
// own internal callbacks do the same and it keeps URLs grep-able in
// captures). All endpoints return XML by default; we always send
// `Accept: application/json` so PMS responds with its JSON variant.
package plex

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client holds the base URL + token for a single PMS instance.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New constructs a Client. baseURL is the scheme://host:port form (no
// trailing slash); token is the X-Plex-Token value.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// do issues an HTTP request to <baseURL><path>?<query>&X-Plex-Token=...
// and decodes the JSON response into v (when non-nil). For PUT/POST/DELETE
// without a meaningful response body, pass v=nil. method is one of "GET",
// "PUT", "POST", "DELETE".
func (c *Client) do(method, path string, query url.Values, v interface{}) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("X-Plex-Token", c.token)
	u := c.baseURL + path + "?" + query.Encode()

	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s %s: %s — %s", method, path, resp.Status, truncate(string(body), 200))
	}
	if v == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode %s %s: %w — body=%s", method, path, err, truncate(string(body), 200))
	}
	return nil
}

// Identity GETs /identity — a free ping that confirms the token works and
// returns the server's machineIdentifier + version. Cheapest way to fail
// fast on a bad token / unreachable PMS.
type Identity struct {
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
	APIVersion        string `json:"apiVersion"`
}

func (c *Client) Identity() (*Identity, error) {
	var resp struct {
		MediaContainer Identity `json:"MediaContainer"`
	}
	if err := c.do("GET", "/identity", nil, &resp); err != nil {
		return nil, err
	}
	return &resp.MediaContainer, nil
}

// truncate returns s truncated to n runes, with an ellipsis appended when
// the string was longer. Used for error messages that quote response bodies
// — keeps logs readable without losing the leading status hint.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
