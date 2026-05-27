package main

import "testing"

// Plex's transcode-session cwd layout is
//
//	/transcode/Transcode/Sessions/plex-transcode-<TOKEN>-<UUID>
//
// where <TOKEN> is whatever opaque client-derived identifier PMS hands
// out and <UUID> is its own RFC 4122 lowercase tag for the worker spawn.
// extractPlexSessionToken must lift both halves so a downstream tool
// (vcflogs ↔ argv corpus cross-ref) can map back to PMS's
// `?session=<TOKEN>` request-URL parameter.
func TestExtractPlexSessionToken(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cwd        string
		wantToken  string
		wantUUID   string
		wantOK     bool
	}{
		{
			name:      "24-char lowercase alnum token (Plex Web, mobile)",
			cwd:       "/transcode/Transcode/Sessions/plex-transcode-01xtsbm57otmikj51elqu64g-956b6aef-d06f-434c-84e0-f10d8675d3ee",
			wantToken: "01xtsbm57otmikj51elqu64g",
			wantUUID:  "956b6aef-d06f-434c-84e0-f10d8675d3ee",
			wantOK:    true,
		},
		{
			name:      "uppercase hex token (Apple TV, PS4)",
			cwd:       "/transcode/Transcode/Sessions/plex-transcode-0328B846-43F3-48BF-8AA4-DD70713CBBDF-75a867cf-4398-4863-ac7e-9a7f5b2f88ce",
			wantToken: "0328B846-43F3-48BF-8AA4-DD70713CBBDF",
			wantUUID:  "75a867cf-4398-4863-ac7e-9a7f5b2f88ce",
			wantOK:    true,
		},
		{
			name:      "vendor-suffixed token (Android com-plexapp shape)",
			cwd:       "/transcode/Transcode/Sessions/plex-transcode-b27fb75f45fb3869-com-plexapp-android-1d28e677-f14b-404b-95e2-967c3f01c5ea",
			wantToken: "b27fb75f45fb3869-com-plexapp-android",
			wantUUID:  "1d28e677-f14b-404b-95e2-967c3f01c5ea",
			wantOK:    true,
		},
		{
			name:      "cwd with trailing slash (some shim invocations)",
			cwd:       "/transcode/Transcode/Sessions/plex-transcode-01xtsbm57otmikj51elqu64g-956b6aef-d06f-434c-84e0-f10d8675d3ee/",
			wantToken: "01xtsbm57otmikj51elqu64g",
			wantUUID:  "956b6aef-d06f-434c-84e0-f10d8675d3ee",
			wantOK:    true,
		},
		{
			name:   "empty cwd",
			cwd:    "",
			wantOK: false,
		},
		{
			name:   "non-transcode cwd",
			cwd:    "/tmp",
			wantOK: false,
		},
		{
			name:   "transcode-like cwd without UUID half",
			cwd:    "/transcode/Transcode/Sessions/plex-transcode-just-a-token",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotToken, gotUUID, gotOK := extractPlexSessionToken(tc.cwd)
			if gotOK != tc.wantOK {
				t.Fatalf("ok = %v, want %v (token=%q uuid=%q)", gotOK, tc.wantOK, gotToken, gotUUID)
			}
			if !tc.wantOK {
				return
			}
			if gotToken != tc.wantToken {
				t.Errorf("token = %q, want %q", gotToken, tc.wantToken)
			}
			if gotUUID != tc.wantUUID {
				t.Errorf("uuid = %q, want %q", gotUUID, tc.wantUUID)
			}
		})
	}
}
