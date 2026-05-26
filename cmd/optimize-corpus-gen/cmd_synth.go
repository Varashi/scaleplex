package main

import (
	"flag"
	"log"
	"os"

	"github.com/Varashi/scaleplex/cmd/optimize-corpus-gen/synth"
)

// runSynth builds the synthetic source-clip matrix. Idempotent.
// outDir is the path in the ffmpeg-side environment (kubectl-exec'd
// container's view, when remote).
//
// Typical homelab invocation:
//
//	optimize-corpus-gen synth \
//	    -out-dir /mnt/media/media/scaleplex-test-clips \
//	    -ffmpeg-exec "kubectl exec -n media-toolkit <pod> -- ffmpeg"
//
// (The /mnt/media/media path is the media-toolkit pod's view of the
// NFS share PMS sees as /media — pre-translated, since the ffmpeg
// invocation happens inside the pod.)
func runSynth() {
	fs := flag.NewFlagSet("synth", flag.ExitOnError)
	outDir := fs.String("out-dir", "", "Output directory (in ffmpeg's view) — e.g. /mnt/media/media/scaleplex-test-clips for the media-toolkit pod")
	ffmpegExec := fs.String("ffmpeg-exec", "", "Wrapper command for ffmpeg (e.g. 'kubectl exec -n media-toolkit POD -- ffmpeg'); empty = local ffmpeg")
	fs.Parse(os.Args[1:])

	if *outDir == "" {
		log.Fatal("--out-dir is required")
	}

	b := synth.NewBuilder(*ffmpegExec, *outDir)
	specs := synth.DefaultMatrix()
	log.Printf("synthesizing %d clips into %s", len(specs), *outDir)

	paths, err := b.EnsureAll(specs, func(msg string) { log.Println(msg) })
	if err != nil {
		fatalf("ensure: %v", err)
	}
	log.Printf("done; %d clips total in %s", len(paths), *outDir)
}
