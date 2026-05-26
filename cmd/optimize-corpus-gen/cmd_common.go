package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

// commonFlags holds the flags shared across subcommands so subcommand
// constructors stay terse. Each subcommand adds its own flags on top.
type commonFlags struct {
	plexURL string
	token   string
}

func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	cf := &commonFlags{}
	fs.StringVar(&cf.plexURL, "plex", "", "PMS base URL (e.g. http://172.16.4.106:32400 — $PLEX_TEST_URL from ~/.config/plex.env)")
	fs.StringVar(&cf.token, "token", "", "X-Plex-Token; default $PLEX_TOKEN env")
	return cf
}

func (cf *commonFlags) resolve() {
	if cf.token == "" {
		cf.token = os.Getenv("PLEX_TOKEN")
	}
	if cf.plexURL == "" {
		log.Fatal("--plex is required (e.g. $PLEX_TEST_URL from ~/.config/plex.env)")
	}
	if cf.token == "" {
		log.Fatal("--token or $PLEX_TOKEN is required")
	}
}

// pathTranslateFlag is a repeatable -path-translate flag for ffprobe.
// Each invocation appends one FROM=TO rule; ffprobe applies them
// first-match-wins.
type pathTranslateFlag []string

func (p *pathTranslateFlag) String() string {
	return fmt.Sprintf("%v", []string(*p))
}

func (p *pathTranslateFlag) Set(v string) error {
	*p = append(*p, v)
	return nil
}
