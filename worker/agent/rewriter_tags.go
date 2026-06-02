package main

import "strings"

// relabelCrossBackendTags rewrites VAAPI-canonical filter-tag substrings to the
// target backend's names after a cross-backend reshape (#113). The Tag* / TagPrefix*
// constants are VAAPI-canonical (e.g. `inlineass-vaapi`, `opencl-tonemap->vaapi`)
// because VAAPI was the first backend implemented; a cross-backend retarget to
// NVENC actually executes the CUDA branch (`inlineass(CUDA)`, `tonemap_cuda`), so
// the emitted tags misrepresented the executed graph (320/320 nvenc PASS still
// logged `inlineass-vaapi`). This post-processes res.Changes in place; only the
// log + matrix consumers see tags so a string-level rewrite is safe.
//
// Mutates `tags` in place. No-op when there's no `cross-backend:*->X` marker or
// the target backend is the canonical one (vaapi).
func relabelCrossBackendTags(tags []string) {
	target := ""
	for _, t := range tags {
		if strings.HasPrefix(t, "cross-backend:") {
			if i := strings.LastIndex(t, "->"); i > 0 {
				target = t[i+2:]
			}
			break
		}
	}
	if target == "" || target == "vaapi" {
		return
	}
	var rep [][2]string
	switch target {
	case "nvenc":
		// Order matters: replace the longer/more-specific strings first so a
		// later replacement doesn't accidentally rewrite a subspan.
		rep = [][2]string{
			{"bitmap-inlineass-vaapi", "bitmap-inlineass-cuda"},
			{"opencl-tonemap->vaapi", "tonemap_cuda"},
			{"tonemap_opencl-normalized", "tonemap_cuda-normalized"},
			{"inlineass-vaapi", "inlineass-cuda"},
		}
	default:
		return
	}
	for i, t := range tags {
		for _, kv := range rep {
			if strings.Contains(t, kv[0]) {
				t = strings.ReplaceAll(t, kv[0], kv[1])
			}
		}
		tags[i] = t
	}
}

// Rewriter change-tag inventory — single source of truth for the strings the
// rewriter emits via res.Changes (and the bail()/scrub() variants). Live
// docs (docs/CLIENT_TEST_MATRIX.md, docs/REWRITER.md) and runtime tools
// (release-gate log greps, T4 worker-side PASS verification) reference
// these tags by string; without a pinned canon, renaming a tag in
// rewriter.go silently breaks the contract downstream — already happened
// at 64ac0f7 (2026-05-26, `overlay-vaapi-bitmap` → `bitmap-inlineass-vaapi`).
//
// rewriter_tags_test.go enforces the contract by AST-parsing rewriter.go
// and asserting every change-tag literal either matches a Tag* full
// constant or starts with a TagPrefix* prefix constant.
//
// PR-A introduces the inventory + the assertion test. PR-B replaces the
// literals in rewriter.go with references to these constants so a rename
// updates one place, not 50.

// Full-literal tags — exact strings rewriter.go emits via
// `append(changes, "<literal>")`.
const (
	TagAddMapInlineass                   = "add:-map_inlineass"
	TagBailSegmentListRewriteToRelay     = "bail:segment_list:rewrite-to-relay"
	TagCanThrottleDisabledByEnv          = "canthrottle:disabled-by-env"
	TagDropEAEPrefixBail                 = "drop:-eae_prefix(bail)"
	TagDropInlineassDecodeSink           = "drop:inlineass-decode-sink"
	TagDropNostats                       = "drop:-nostats"
	TagDropProgressurlBail               = "drop:-progressurl(bail)"
	TagEnvHOME                           = "env:HOME"
	TagEnvLIBVA                          = "env:LIBVA"
	TagFilterInlineassScrubPlexFontPaths = "filter:inlineass:scrub-plex-font-paths"
	TagBailManifestNameRewriteToRelay    = "bail:manifest_name:rewrite-to-relay"
	TagBailManifestNameDrop              = "bail:manifest_name:drop(no-pms-base-or-non-loopback)"
	TagFilterTonemapOpenCLNormalized     = "filter:tonemap_opencl-normalized"
	TagFilterTonemapOpenCLToVAAPI        = "filter:tonemap_opencl->tonemap_vaapi"
	TagForceHWWouldHonorHWDecSWEnc       = "force-hw:would-honor-hwdec-swenc"
	TagForceHWWouldHonorSW               = "force-hw:would-honor-sw"
	TagPassGateDenied                    = "pass-gate:denied-honor-sw" // #78 — no active Plex Pass → HW re-accel denied

	TagHLSSegmentListRewriteToRelay     = "hls:segment_list:rewrite-to-relay"
	TagHWDecodeFilterBitmapInlineassVA  = "hw-decode:filter:bitmap-inlineass-vaapi"
	TagHWDecodeFilterInlineassVA        = "hw-decode:filter:inlineass-vaapi"
	TagHWDecodeFilterOCLToVAAPIIA       = "hw-decode:filter:opencl-tonemap->vaapi:inlineass-vaapi"
	TagHWDecodeMapLabelUpdate           = "hw-decode:map-label-update"
	TagHonorPlexHWDecSWEnc              = "honor:plex-hwdec-swenc"
	TagHonorPlexSW                      = "honor:plex-sw"
	TagInjectCanThrottleURL             = "inject:-canthrottleurl(scaleplex-ffmpeg7-canThrottle)"
	TagInjectInitHWDevice               = "inject:init_hw_device+filter_hw_device"
	TagReplaceForeignInitHWDevice       = "replace:foreign-init_hw_device" // #85 — drop a foreign-backend -init_hw_device so it's re-injected for the worker dialect
	TagStripVAAPIDriverParam            = "strip:init_hw_device:driver"    // #124 — strip Plex's hardcoded `,driver=iHD` from same-backend VAAPI init_hw_device; libva picks via LIBVA_DRIVER_NAME env (vendor-aware default from detectVAAPIDriver)
	TagToSWStripHWDecode                = "to-sw:strip-hwdecode"           // #77 PR3 — swDialect stripped HW decode flags + device init
	TagInjectSEIA53CC                   = "inject:sei+a53_cc"
	TagLoglevelInfo                     = "loglevel:->info"
	TagMapLabelUpdate                   = "map-label-update"
	TagProgressAppendXPlexToken         = "progress:append-X-Plex-Token"
	TagProgressURLCapturedForReporter   = "progressurl:captured-for-reporter"
	TagSubsSideChannelSegListToRelay    = "subs:side-channel-segment_list:rewrite-to-relay"
	TagTonemapOCLCollapseRevmapDownload = "tonemap:ocl:collapse-revmap-download"
	TagTonemapOCLDropLeadHWUpload       = "tonemap:ocl:drop-lead-hwupload"
	TagTonemapOCLForceOutputFormatVA    = "tonemap:ocl:force-output-format-vaapi"
	TagTonemapOCLInjectOpenCLDevice     = "tonemap:ocl:inject-opencl-device"
)

// Prefix tags — rewriter.go emits these as `"<prefix>" + <runtime-value>`.
// A literal that matches any of these as a prefix is considered covered.
//
// Suffix shape comments document what the runtime value looks like; they
// are not enforced (the assertion test only checks the prefix).
const (
	TagPrefixAudio                       = "audio:"                                               // <src>-><dst>  or  <src>-><dst>(bail)
	TagPrefixBailFilterPattern           = "filter-pattern:"                                      // <filter-string>  (only as bail reason)
	TagPrefixBailHWDecodeSubUnmodeled    = "hw-decode-sub:unmodeled-graph:"                       // <graph-string>  (only as bail reason)
	TagPrefixBailUnexpectedEncoder       = "hw-decode:unexpected-encoder:"                        // <encoder>       (only as bail reason)
	TagPrefixBailUnknownDecoder          = "unknown-decoder:"                                     // <decoder>       (only as bail reason)
	TagPrefixBailUnknownEncoder          = "unknown-encoder:"                                     // <encoder>       (only as bail reason)
	TagPrefixDecode                      = "decode:"                                              // <swDecoder>-><hwDecoder>
	TagPrefixDecodeBareHWUpgrade         = "decode:bare-hw-upgrade:"                              // <swDecoder>
	TagPrefixDecodeHWPassthrough         = "decode:hw-passthrough:"                               // <swDecoder>
	TagPrefixDrop                        = "drop:"                                                // <arg> or <arg>(bail)
	TagPrefixEncode                      = "encode:"                                              // <swEncoder>-><hwEncoder>
	TagPrefixEncodeHWPassthrough         = "encode:hw-passthrough:"                               // <swEncoder>
	TagPrefixEnvStrip                    = "env:strip:"                                           // <env-var-name>
	TagPrefixFilter                      = "filter:"                                              // <mode>  (composeMode → bitmap-inlineass-vaapi | text-inlineass-vaapi | hdr-tonemap-vaapi | plain)
	TagPrefixForceHWReshapeHybrid        = "force-hw:reshape-hybrid:"                             // <swDecoder>
	TagPrefixHWDecodeFilterBitmapHDRTM   = "hw-decode:filter:bitmap-inlineass-vaapi:hdr-tonemap(" // <algo>)
	TagPrefixHWDecodeSubTonemapPreserved = "hw-decode-sub:tonemap-preserved("                     // <algo>)
	TagPrefixSeekOffsetCaptured          = "seek-offset:captured=%.3fs"                           // fmt.Sprintf format — emitted via Sprintf, prefix matches the literal format string
	TagPrefixSkip                        = "skip:"                                                // <reason>
	TagPrefixSkipToSegmentPassthrough    = "skip_to_segment:passthrough="                         // <segment-number>
	TagPrefixSubtitleBitmap              = "subtitle:bitmap:"                                     // <StreamSpec>[(<Codec>)]
	TagPrefixVideoHDRSource              = "video:hdr-source("                                    // <transfer>)
	TagPrefixToSW                        = "to-sw:"                                               // <sourceBackend>  (#77 PR3 — swDialect downgrade)
	TagPrefixToSWFilter                  = "to-sw:filter:"                                        // <composeMode>
	TagPrefixToSWEncode                  = "to-sw:encode:"                                        // <hwEncoder>-><swEncoder>
	TagPrefixToSWDecode                  = "to-sw:decode:"                                        // <shortCodec>-><swDecoderLib>
)

// Bail-only reason strings — passed to bail() which produces `"skip:" + reason`.
// Listed here as full strings (the leading "skip:" comes from TagPrefixSkip)
// so the assertion test can recognise them when they appear as bare string
// literals in `return bail("...")` calls. Static (no concat) values only;
// concatenated bail reasons like `unknown-decoder:` + swDecoder are covered
// by TagPrefixBailUnknownDecoder above.
const (
	TagBailReasonHWDecodeSubBitmapUnsupported = "hw-decode-sub:bitmap-unsupported"
	TagBailReasonHWDecodeSubNoInlineass       = "hw-decode-sub:no-inlineass-filter"
	TagBailReasonNoDecoder                    = "no-decoder"
	TagBailReasonNoEncoder                    = "no-encoder"
	TagBailReasonNoInput                      = "no-input"
	TagBailReasonNoVideoFilter                = "no-video-filter"
	TagBailReasonSubtitlesBurnIn              = "subtitles-burn-in"
)
