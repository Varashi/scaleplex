package plex

import "fmt"

// TranscodeSession is one active transcode session (Optimize jobs show
// up here as well as live playback). The generator uses this to detect
// when PMS has actually spawned the worker — the session appears
// shortly after TriggerOptimize when PMS is ready to dispatch ffmpeg.
type TranscodeSession struct {
	Key           string  `json:"key"`
	Throttled     bool    `json:"throttled"`
	Complete      bool    `json:"complete"`
	Progress      float64 `json:"progress"`
	Speed         float64 `json:"speed"`
	Duration      int     `json:"duration"`
	Context       string  `json:"context"` // "static" for Optimize, "streaming" for playback
	SourceVideoCodec string `json:"sourceVideoCodec"`
	SourceAudioCodec string `json:"sourceAudioCodec"`
	VideoCodec    string  `json:"videoCodec"`
	AudioCodec    string  `json:"audioCodec"`
	Container     string  `json:"container"`
}

// TranscodeSessions returns every active session on the server. Optimize
// jobs show up with Context="static"; playback sessions are "streaming".
// The generator filters for "static" while watching for its spawn.
func (c *Client) TranscodeSessions() ([]TranscodeSession, error) {
	var resp struct {
		MediaContainer struct {
			TranscodeSession []TranscodeSession `json:"TranscodeSession"`
		} `json:"MediaContainer"`
	}
	if err := c.do("GET", "/transcode/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return resp.MediaContainer.TranscodeSession, nil
}

// StopTranscodeSession hard-stops one session by key. Used as a belt-
// and-braces companion to CancelOptimize — DELETE on the playlist
// usually stops the session, but on some PMS versions the session
// lingers briefly; this kills it immediately.
func (c *Client) StopTranscodeSession(key string) error {
	if key == "" {
		return fmt.Errorf("StopTranscodeSession: empty key")
	}
	return c.do("DELETE", "/transcode/sessions/"+key, nil, nil)
}
