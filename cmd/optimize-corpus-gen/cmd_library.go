package main

import (
	"flag"
	"log"
	"os"
	"time"

	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/plex"
)

// runLibrary creates+scans the Plex library section that holds the
// synthetic corpus clips. Idempotent — if a section with the chosen
// title already exists, just refreshes it.
//
// The location flag is the path PMS sees (not the ffmpeg-side path),
// because PMS does the scan — e.g. /media/scaleplex-test-clips when
// the worker NFS-mounts the share at /media.
func runLibrary() {
	fs := flag.NewFlagSet("library", flag.ExitOnError)
	cf := addCommonFlags(fs)
	name := fs.String("name", "scaleplex-test-corpus", "Section title")
	location := fs.String("location", "", "Section location (PMS-view path, e.g. /media/scaleplex-test-clips)")
	expectedItems := fs.Int("expected-items", 0, "Wait for at least this many items to appear after scan (0 = no wait)")
	waitTimeout := fs.Duration("wait-timeout", 5*time.Minute, "Max wait for scan to pick up items")
	fs.Parse(os.Args[1:])

	cf.resolve()
	if *location == "" {
		log.Fatal("--location is required (PMS-view path, e.g. /media/scaleplex-test-clips)")
	}
	c := plex.New(cf.plexURL, cf.token)

	sec, err := c.CreateOrFindSection(*name, *location)
	if err != nil {
		fatalf("create-or-find: %v", err)
	}
	log.Printf("section: key=%s uuid=%s title=%q type=%s", sec.Key, sec.UUID, sec.Title, sec.Type)

	if err := c.RefreshSection(sec.Key); err != nil {
		fatalf("refresh: %v", err)
	}
	log.Printf("refresh triggered")

	if *expectedItems > 0 {
		log.Printf("waiting up to %s for >=%d items", *waitTimeout, *expectedItems)
		items, err := c.WaitForSectionItems(sec.Key, *expectedItems, *waitTimeout)
		if err != nil {
			log.Printf("WARN: %v", err)
		} else {
			log.Printf("scan picked up %d items", len(items))
		}
	}
}
