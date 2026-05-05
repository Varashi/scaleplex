package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatchFirstSegment_FiresOnFirstM4S(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	lw := newLockedWriter(&buf)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchFirstSegment(ctx, dir, "test-session", lw)
	}()

	// Give the watcher a moment to add the inotify hook.
	time.Sleep(50 * time.Millisecond)

	// Decoy: manifest.mpd should be ignored.
	if err := os.WriteFile(filepath.Join(dir, "manifest.mpd"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	// Real segment should trigger one emission.
	if err := os.WriteFile(filepath.Join(dir, "chunk-stream0-00001.m4s"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	wg.Wait()

	got := buf.String()
	if !strings.Contains(got, "segment-ready: chunk-stream0-00001.m4s") {
		t.Fatalf("missing segment-ready emit, got: %q", got)
	}
	if strings.Count(got, "segment-ready") != 1 {
		t.Fatalf("expected exactly one segment-ready, got: %q", got)
	}
}

func TestWatchFirstSegment_IgnoresUninterestingExtensions(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	lw := newLockedWriter(&buf)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchFirstSegment(ctx, dir, "test-session", lw)
	}()

	time.Sleep(50 * time.Millisecond)
	for _, name := range []string{"a.tmp", "manifest.mpd", "x.txt", "ffmpeg.log"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}

	<-done // ctx-cancel exit
	if buf.Len() != 0 {
		t.Fatalf("expected no emit, got: %q", buf.String())
	}
}

func TestWatchFirstSegment_BailsOnEmptyDir(t *testing.T) {
	// Empty dir param → silent no-op (no panic, no goroutine leak).
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	lw := newLockedWriter(&buf)
	watchFirstSegment(ctx, "", "test-session", lw)
	if buf.Len() != 0 {
		t.Fatalf("expected nothing written, got: %q", buf.String())
	}
}

func TestLockedWriter_PrefixedAtomicity(t *testing.T) {
	// Two goroutines hammering writePrefixed must not interleave the
	// prefix with another goroutine's payload.
	var buf bytes.Buffer
	lw := newLockedWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = lw.writePrefixed("[A] ", []byte("hello\n")) }()
		go func() { defer wg.Done(); _, _ = lw.writePrefixed("[B] ", []byte("world\n")) }()
	}
	wg.Wait()
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !(strings.HasPrefix(line, "[A] hello") || strings.HasPrefix(line, "[B] world")) {
			t.Fatalf("interleaved line: %q", line)
		}
	}
}
