package plex

import (
	"fmt"
	"net/url"
	"strconv"
)

// Section is one library section (Movies, Anime, …).
type Section struct {
	Key   string `json:"key"`
	Type  string `json:"type"`  // "movie", "show", …
	Title string `json:"title"`
	UUID  string `json:"uuid"`  // used in Optimize Item[Location][uri] = library://<uuid>/item/...
}

// Item is one library item (a movie, or — for shows — a series). For
// generator purposes we only care about ratingKey + the file path of
// the actual media (so we can ffprobe it for ground-truth metadata).
type Item struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Type      string `json:"type"` // "movie", "episode", …
	Media     []Media `json:"Media"`
}

// Media is one Plex Media block (1:N items have multiple — Optimized
// versions show up as siblings of the original). Part holds the
// on-disk file path the worker actually opens.
type Media struct {
	ID                int64  `json:"id"`
	Container         string `json:"container"`
	VideoCodec        string `json:"videoCodec"`
	VideoProfile      string `json:"videoProfile"`
	VideoResolution   string `json:"videoResolution"`
	OptimizedForStreaming int `json:"optimizedForStreaming"`
	Part              []Part `json:"Part"`
}

// Part is one physical file under a Media block. File is the filesystem
// path (NFS-mounted at the worker).
type Part struct {
	ID   int64  `json:"id"`
	Key  string `json:"key"`
	File string `json:"file"`
	Size int64  `json:"size"`
	Container string `json:"container"`
}

// Sections lists every library section the server exposes.
func (c *Client) Sections() ([]Section, error) {
	var resp struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.do("GET", "/library/sections", nil, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Directory, nil
}

// SectionItems lists every item under one section. For movie sections
// this is the movies themselves; for show sections this returns series
// (use SeriesEpisodes to descend into episodes).
func (c *Client) SectionItems(sectionKey string) ([]Item, error) {
	var resp struct {
		MediaContainer struct {
			Metadata []Item `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	path := fmt.Sprintf("/library/sections/%s/all", sectionKey)
	if err := c.do("GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// SeriesEpisodes lists every episode under one series (series ratingKey
// from SectionItems on a show section). Used when the test library is
// organized as shows-with-episodes rather than flat movies.
func (c *Client) SeriesEpisodes(seriesRatingKey string) ([]Item, error) {
	var resp struct {
		MediaContainer struct {
			Metadata []Item `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	path := fmt.Sprintf("/library/metadata/%s/allLeaves", seriesRatingKey)
	if err := c.do("GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.Metadata, nil
}

// Metadata returns full metadata for one item. The SectionItems response
// already contains Media+Part, but for episode-level access via ratingKey
// (e.g. after a search) this is the canonical lookup.
func (c *Client) Metadata(ratingKey string) (*Item, error) {
	var resp struct {
		MediaContainer struct {
			Metadata []Item `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	path := fmt.Sprintf("/library/metadata/%s", ratingKey)
	q := url.Values{}
	q.Set("includeChildren", "1")
	if err := c.do("GET", path, q, &resp); err != nil {
		return nil, err
	}
	if len(resp.MediaContainer.Metadata) == 0 {
		return nil, fmt.Errorf("metadata %s: no item returned", ratingKey)
	}
	return &resp.MediaContainer.Metadata[0], nil
}

// FindSectionByTitle returns the section whose title matches exactly,
// or an error listing what was available. Convenience for the
// dedicated test-library workflow ("optimize-corpus-test" section).
func (c *Client) FindSectionByTitle(title string) (*Section, error) {
	secs, err := c.Sections()
	if err != nil {
		return nil, err
	}
	for _, s := range secs {
		if s.Title == title {
			return &s, nil
		}
	}
	var titles []string
	for _, s := range secs {
		titles = append(titles, strconv.Quote(s.Title))
	}
	return nil, fmt.Errorf("section %q not found; available: [%s]", title, joinStrings(titles, ", "))
}

func joinStrings(s []string, sep string) string {
	if len(s) == 0 {
		return ""
	}
	out := s[0]
	for _, x := range s[1:] {
		out += sep + x
	}
	return out
}
