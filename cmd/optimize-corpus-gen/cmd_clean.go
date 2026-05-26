package main

import (
	"flag"
	"log"
	"os"

	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/plex"
)

// runClean sweeps the queue clean: cancels every Optimize job whose
// title carries the scaleplex-corpus-gen prefix + stops every stuck
// static transcode session. Use this when a previous run was
// interrupted before its own cleanup ran.
func runClean() {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	cf := addCommonFlags(fs)
	fs.Parse(os.Args[1:])

	cf.resolve()
	c := plex.New(cf.plexURL, cf.token)

	bgKey, err := c.BackgroundProcessingKey()
	if err != nil {
		fatalf("bg key: %v", err)
	}
	cancelAllCorpusJobs(c, bgKey)
	log.Printf("clean complete")
}
