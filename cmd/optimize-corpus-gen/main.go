// optimize-corpus-gen drives a Plex Media Server through Optimize jobs
// across a matrix of {source × target × prefs}, capturing the ffmpeg
// argv each cell produces via the worker's existing WORKER_DUMP_ARGV
// instrumentation. Output is a representative corpus of Plex Optimize
// argvs that the rewriter's parity harness can consume.
//
// Subcommands:
//
//	smoke   — one-cell end-to-end test (Phase 1). Validates plumbing.
//	synth   — build the synthetic source-clip matrix into a dir.
//	library — create+scan the Plex library section that holds the synth clips.
//	sweep   — run the full matrix sweep (Phase 2). Resumable.
//	clean   — sweep-cancel pending corpus jobs + stuck static sessions.
//
// See README.md for usage examples per subcommand.
package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	sub := os.Args[1]
	os.Args = append([]string{os.Args[0] + " " + sub}, os.Args[2:]...)
	switch sub {
	case "smoke":
		runSmoke()
	case "synth":
		runSynth()
	case "library":
		runLibrary()
	case "sweep":
		runSweep()
	case "clean":
		runClean()
	case "analyze":
		runAnalyze()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `optimize-corpus-gen — Plex Optimize corpus generator

Subcommands:
  smoke    one-cell end-to-end test (validates plumbing)
  synth    build synthetic source-clip matrix into a dir
  library  create+scan the Plex library section holding the synth clips
  sweep    run the full matrix sweep (resumable)
  clean    sweep-cancel pending corpus jobs + stuck static sessions
  analyze  cluster the captured corpus by argv-shape fingerprint

Run with %s <subcommand> -h for per-subcommand flags.
`, os.Args[0])
}

// fatalf is log.Fatalf with the package's standard prefix.
func fatalf(format string, args ...interface{}) {
	log.Fatalf(format, args...)
}
