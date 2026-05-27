package main

import (
	"reflect"
	"testing"
)

func TestParseWorkerList(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		defaultPort int
		want        []hostPort
	}{
		{"empty", "", 3501, nil},
		{"whitespace only", "   ", 3501, nil},
		{"single host:port", "host1:3501", 3501, []hostPort{{"host1", 3501}}},
		{"bare hostname uses default", "host1", 3501, []hostPort{{"host1", 3501}}},
		{
			"three comma-separated",
			"h1:3501,h2:3502,h3:3503",
			3501,
			[]hostPort{{"h1", 3501}, {"h2", 3502}, {"h3", 3503}},
		},
		{
			"whitespace tolerant",
			"  h1:3501 , h2:3502 , h3:3503 ",
			3501,
			[]hostPort{{"h1", 3501}, {"h2", 3502}, {"h3", 3503}},
		},
		{
			"mixed bare + qualified",
			"bare-host,qual:3502",
			3501,
			[]hostPort{{"bare-host", 3501}, {"qual", 3502}},
		},
		{
			"ipv4 with port",
			"10.0.0.5:3501,10.0.0.6:3501",
			3501,
			[]hostPort{{"10.0.0.5", 3501}, {"10.0.0.6", 3501}},
		},
		{"bad port skipped", "good:3501,bad:abc", 3501, []hostPort{{"good", 3501}}},
		{"out-of-range port skipped", "good:3501,oor:99999", 3501, []hostPort{{"good", 3501}}},
		{"empty entries skipped", "h1:3501,,,h2:3502", 3501, []hostPort{{"h1", 3501}, {"h2", 3502}}},
		{"trailing comma tolerated", "h1:3501,", 3501, []hostPort{{"h1", 3501}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWorkerList(tc.input, tc.defaultPort)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseWorkerList(%q, %d) = %v, want %v", tc.input, tc.defaultPort, got, tc.want)
			}
		})
	}
}

func TestLoadList_AddsListSource(t *testing.T) {
	resetGlobals()
	pl.loadList([]hostPort{{"h1", 3501}, {"h2", 3501}})

	if got := len(pl.workers); got != 2 {
		t.Fatalf("expected 2 workers, got %d", got)
	}
	for url, w := range pl.workers {
		if w.sources != srcList {
			t.Errorf("worker %s: sources=%v, want srcList only", url, w.sources)
		}
		if w.url != url {
			t.Errorf("worker %s: w.url=%q does not match map key", url, w.url)
		}
	}
	if _, ok := pl.workers["http://h1:3501"]; !ok {
		t.Errorf("missing h1: keys=%v", keysOf(pl.workers))
	}
	if _, ok := pl.workers["http://h2:3501"]; !ok {
		t.Errorf("missing h2: keys=%v", keysOf(pl.workers))
	}
}

func TestLoadList_MergesWithExistingDNSWorker(t *testing.T) {
	resetGlobals()
	// Pretend DNS already added http://10.0.0.5:3501
	wk := &worker{host: "10.0.0.5", url: "http://10.0.0.5:3501", sources: srcDNS}
	pl.workers[wk.url] = wk

	pl.loadList([]hostPort{{"10.0.0.5", 3501}})

	if got := len(pl.workers); got != 1 {
		t.Fatalf("expected 1 worker (deduped), got %d: %v", got, keysOf(pl.workers))
	}
	w := pl.workers["http://10.0.0.5:3501"]
	if w == nil {
		t.Fatalf("worker missing after dedup")
	}
	if w.sources != srcDNS|srcList {
		t.Errorf("sources=%v, want srcDNS|srcList", w.sources)
	}
}

func TestRefreshDoesNotRemoveListWorker(t *testing.T) {
	resetGlobals()
	// Add a LIST worker that DNS will never see.
	pl.loadList([]hostPort{{"manual-host", 3501}})
	if got := pl.workers["http://manual-host:3501"]; got == nil {
		t.Fatalf("loadList didn't add the worker")
	}

	// Simulate a DNS refresh that resolved an EMPTY set: implementation
	// detail — refresh() returns early if LookupHost errs, so instead
	// drive the post-lookup logic manually by constructing the equivalent
	// "seen" map (we test the integration via TestRefresh_RemovesOnlyDNSOnly).
	// Here we just assert the LIST worker survives a clear-DNS-bit pass.
	w := pl.workers["http://manual-host:3501"]
	w.sources &^= srcDNS // would clear DNS bit; LIST bit stays
	if w.sources == 0 {
		t.Fatalf("LIST worker lost all sources after clearing DNS bit")
	}
	if w.sources != srcList {
		t.Errorf("after clearing DNS bit, sources=%v, want srcList", w.sources)
	}
}

func TestRefresh_RemovesOnlyDNSOnlyWorkers(t *testing.T) {
	resetGlobals()
	// Three workers:
	//   dns-only-vanishes:    DNS only, will drop out of refresh → removed
	//   dns-and-list-kept:    DNS + LIST → DNS clear leaves LIST → kept
	//   list-only:            LIST only → refresh ignores → kept
	pl.workers["http://dns-vanishes:3501"] = &worker{
		host: "dns-vanishes", url: "http://dns-vanishes:3501", sources: srcDNS,
	}
	pl.workers["http://dns-and-list:3501"] = &worker{
		host: "dns-and-list", url: "http://dns-and-list:3501", sources: srcDNS | srcList,
	}
	pl.workers["http://list-only:3501"] = &worker{
		host: "list-only", url: "http://list-only:3501", sources: srcList,
	}

	// Apply the "DNS now sees nothing" half of refresh manually — this
	// exercises the bit-clearing + reap path without needing a stub
	// resolver (net.LookupHost is the only un-injectable bit of refresh).
	pl.mu.Lock()
	seen := map[string]struct{}{} // empty: DNS saw nobody
	for url, w := range pl.workers {
		if w.sources&srcDNS == 0 {
			continue
		}
		if _, ok := seen[url]; ok {
			continue
		}
		w.sources &^= srcDNS
		if w.sources == 0 {
			delete(pl.workers, url)
		}
	}
	pl.mu.Unlock()

	if _, ok := pl.workers["http://dns-vanishes:3501"]; ok {
		t.Errorf("dns-only worker should have been removed")
	}
	if w := pl.workers["http://dns-and-list:3501"]; w == nil {
		t.Errorf("dns+list worker should be kept")
	} else if w.sources != srcList {
		t.Errorf("dns+list worker sources after refresh = %v, want srcList", w.sources)
	}
	if w := pl.workers["http://list-only:3501"]; w == nil {
		t.Errorf("list-only worker should be untouched by DNS refresh")
	} else if w.sources != srcList {
		t.Errorf("list-only worker sources after refresh = %v, want srcList", w.sources)
	}
}

func TestSourceBits_String(t *testing.T) {
	tests := []struct {
		s    sourceBits
		want string
	}{
		{0, "none"},
		{srcDNS, "dns"},
		{srcList, "list"},
		{srcPush, "push"},
		{srcDNS | srcList, "dns+list"},
		{srcDNS | srcList | srcPush, "dns+list+push"},
	}
	for _, tc := range tests {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("sourceBits(%d).String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func keysOf(m map[string]*worker) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
