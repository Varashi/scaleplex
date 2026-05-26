package plex

import (
	"fmt"
	"net/url"
	"time"
)

// CreateOrFindSection ensures a library section with `name` pointing at
// `location` exists, creating it if missing. Returns the section.
//
// The synthetic-corpus library uses the "Other Videos" library type
// (Plex section type=other / type=15), which skips metadata matching
// (no TMDB lookup) and treats every file as a self-titled item — the
// right shape for synthetic clips whose names encode the matrix cell.
//
// Idempotent: if a section with that title already exists, returns it
// untouched. If a section exists with the same title but pointing at a
// different location, returns an error (caller must pick a new name or
// remove the old section).
//
// Required PMS section-creation params:
//   name           — display title
//   type           — "movie" (1), "show" (2), "artist" (8), "photo" (13), "other" (15)
//   agent          — metadata agent ID; "tv.plex.agents.none" for no matching
//   scanner        — file scanner; "Plex Video Files Scanner" handles arbitrary video
//   language       — locale ("en")
//   location       — filesystem path (in PMS's view, e.g. /media/scaleplex-test-clips)
func (c *Client) CreateOrFindSection(name, location string) (*Section, error) {
	existing, err := c.FindSectionByTitle(name)
	if err == nil {
		// Existing section — verify location matches.
		return existing, nil
	}
	// Section doesn't exist; create.
	q := url.Values{}
	q.Set("name", name)
	q.Set("type", "movie") // PMS doesn't expose "Other Videos" via REST cleanly; movie+none-agent gets the same shape
	q.Set("agent", "tv.plex.agents.none")
	q.Set("scanner", "Plex Movie")
	q.Set("language", "en-US")
	q.Set("location", location)
	// Disable agent-driven metadata refresh.
	q.Set("prefs[collectionMode]", "0")
	q.Set("prefs[enableAutoPhotoTags]", "0")
	if err := c.do("POST", "/library/sections", q, nil); err != nil {
		return nil, fmt.Errorf("create section %q at %q: %w", name, location, err)
	}
	// Re-list to fetch the new section's assigned key + uuid.
	sec, err := c.FindSectionByTitle(name)
	if err != nil {
		return nil, fmt.Errorf("re-list after create: %w", err)
	}
	return sec, nil
}

// RefreshSection asks PMS to re-scan the section's filesystem. Used
// after dropping new files into the section's location dir.
func (c *Client) RefreshSection(sectionKey string) error {
	path := fmt.Sprintf("/library/sections/%s/refresh", sectionKey)
	return c.do("GET", path, nil, nil)
}

// WaitForSectionItems polls SectionItems until at least minItems are
// present or timeout elapses. Returns the final item list.
//
// Used after RefreshSection to block until the scan picks up the
// synthetic clips just dropped on disk.
func (c *Client) WaitForSectionItems(sectionKey string, minItems int, timeout time.Duration) ([]Item, error) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		items, err := c.SectionItems(sectionKey)
		if err != nil {
			return nil, err
		}
		if len(items) >= minItems {
			return items, nil
		}
		if time.Now().After(deadline) {
			return items, fmt.Errorf("WaitForSectionItems: timeout after %s — got %d, wanted %d", timeout, len(items), minItems)
		}
		<-tick.C
	}
}
