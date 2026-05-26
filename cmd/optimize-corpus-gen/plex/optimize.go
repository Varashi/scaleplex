package plex

import (
	"fmt"
	"net/url"
)

// OptimizeTarget is one Plex Optimize preset (Original Quality, Mobile,
// TV, custom user-defined targets, …). TagID is the parameter the
// trigger endpoint takes (Item[targetTagID]=<N>).
//
// Built-in Plex targets on a fresh install:
//   TagID=1  "Optimized for Mobile"   1280x720 @ 4 Mbps
//   TagID=2  "Optimized for TV"       1920x1080 @ 8 Mbps
//   TagID=3  "Original Quality"       no transcode (remux-only target)
// User-defined custom targets append to this list.
type OptimizeTarget struct {
	TagID         int    `json:"id"`
	Title         string `json:"tag"`
	MediaSettings struct {
		VideoQuality   int    `json:"videoQuality"`
		VideoBitrate   int    `json:"videoBitrate"`
		VideoResolution string `json:"videoResolution"`
	} `json:"MediaSettings"`
	Device struct {
		Profile string `json:"profile"`
	} `json:"Device"`
}

// OptimizedItem is one Item element from <backgroundKey> — an
// Optimize job (queued / running / completed). The generator creates
// these via TriggerOptimize and cancels them via CancelOptimize.
//
// Note: ID is the Item id (unique within the background-processing
// playlist), NOT a top-level Plex ratingKey. CancelOptimize takes it
// as the path component for DELETE.
type OptimizedItem struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Target      string `json:"target"`
	TargetTagID int    `json:"targetTagID"`
}

// OptimizeTargets lists every Optimize preset the server exposes (the
// "Quality" dropdown in the Optimize UI). Built-ins (Mobile / TV /
// Original) plus any user-defined custom targets.
//
// The endpoint is GET /library/tags?type=42 — Plex models Optimize
// presets as media-processing-target tags (tagType=42), same mechanism
// python-plexapi uses (library.tags('mediaProcessingTarget')).
func (c *Client) OptimizeTargets() ([]OptimizeTarget, error) {
	var resp struct {
		MediaContainer struct {
			Tag []OptimizeTarget `json:"Tag"`
		} `json:"MediaContainer"`
	}
	if err := c.do("GET", "/library/tags", url.Values{"type": []string{"42"}}, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Tag, nil
}

// BackgroundProcessingKey returns the path Optimize jobs PUT to —
// `/playlists/<N>/items`, where N is the ratingKey of PMS's
// "Background Processing List" pseudo-playlist (playlistType=42). The
// ratingKey is per-PMS-install — Tautulli hardcodes 1066 because their
// PMS happens to have that; ours might differ. Query at startup once.
func (c *Client) BackgroundProcessingKey() (string, error) {
	var resp struct {
		MediaContainer struct {
			Metadata []struct {
				Key string `json:"key"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.do("GET", "/playlists", url.Values{"type": []string{"42"}}, &resp); err != nil {
		return "", err
	}
	if len(resp.MediaContainer.Metadata) == 0 || resp.MediaContainer.Metadata[0].Key == "" {
		return "", fmt.Errorf("no background-processing playlist found")
	}
	return resp.MediaContainer.Metadata[0].Key, nil
}

// TriggerOptimize creates an Optimize job for one item against one
// target. Returns nil on success — the resulting optimize-playlist
// ratingKey is fetched separately via OptimizedItems (matched by
// jobTitle) so cancellation can target it.
//
// The PUT shape mirrors what python-plexapi sends from `media.optimize()`:
//
//	PUT <backgroundProcessingKey>?           # e.g. /playlists/1066/items
//	  Item[type]=42
//	  Item[title]=<title>
//	  Item[target]=
//	  Item[targetTagID]=<N>
//	  Item[locationID]=-1
//	  Item[Location][uri]=library://<sectionUUID>/item/%2Flibrary%2Fmetadata%2F<ratingKey>
//	  Item[Policy][scope]=all
//	  Item[Policy][value]=0
//	  Item[Policy][unwatched]=0
//
// Item[type]=42 is Plex's magic "optimize playlist" type. After the PUT,
// the job appears as a sibling playlist (playlistType=video, title from
// jobTitle), and PMS spawns ffmpeg shortly thereafter (worker captures
// argv via WORKER_DUMP_ARGV=1).
//
// Item[Location][uri] = `library://<sectionUUID>/item/<url-encoded-metadata-key>`
// scopes the optimize to ONE specific item. The URL-encoding of the
// metadata key is critical — Plex parses it as a single path component.
func (c *Client) TriggerOptimize(backgroundKey, ratingKey, sectionUUID, jobTitle string, targetTagID int) error {
	if backgroundKey == "" || sectionUUID == "" {
		return fmt.Errorf("TriggerOptimize: backgroundKey and sectionUUID required")
	}
	uri := "library://" + sectionUUID + "/item/" + url.QueryEscape("/library/metadata/"+ratingKey)
	q := url.Values{}
	q.Set("Item[type]", "42")
	q.Set("Item[title]", jobTitle)
	q.Set("Item[target]", "")
	q.Set("Item[targetTagID]", fmt.Sprintf("%d", targetTagID))
	q.Set("Item[locationID]", "-1")
	q.Set("Item[Location][uri]", uri)
	q.Set("Item[Policy][scope]", "all")
	q.Set("Item[Policy][value]", "0")
	q.Set("Item[Policy][unwatched]", "0")
	return c.do("PUT", backgroundKey, q, nil)
}

// OptimizedItems lists every active Optimize job under the
// background-processing playlist (both queued and running). The
// generator uses this to find the job it just created (matched by
// jobTitle) so it can cancel after the worker captures the argv.
//
// Endpoint: GET <backgroundKey>. Items come as <Item id=... title=...>
// children of the response MediaContainer.
func (c *Client) OptimizedItems(backgroundKey string) ([]OptimizedItem, error) {
	var resp struct {
		MediaContainer struct {
			Metadata []OptimizedItem `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.do("GET", backgroundKey, nil, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// CancelOptimize removes an Optimize job by its Item id. The endpoint
// is DELETE `<backgroundKey>/<id>` — mirrors python-plexapi's
// Optimized.remove(). Safe to call on already-completed jobs.
//
// Distinct from a regular playlist delete: Optimize jobs live as
// children of the Background Processing List (playlist 1066), not as
// top-level playlists, so DELETE /playlists/<id> 404s.
func (c *Client) CancelOptimize(backgroundKey string, optimizedItemID int) error {
	if backgroundKey == "" || optimizedItemID == 0 {
		return fmt.Errorf("CancelOptimize: backgroundKey and id required")
	}
	return c.do("DELETE", fmt.Sprintf("%s/%d", backgroundKey, optimizedItemID), nil, nil)
}
