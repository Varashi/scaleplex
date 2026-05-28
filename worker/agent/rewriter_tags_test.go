package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// TestRewriterTagInventory enforces the tag-canon contract: every string
// literal rewriter.go feeds into `append(<*>Changes, ...)` (or into the
// bail() helper, which converts to "skip:"+reason) must trace to a Tag*
// full constant or a TagPrefix* prefix constant in rewriter_tags.go.
//
// Catches "added new tag without inventory entry" at PR time. Failure
// surfaces the exact literals that drifted.
func TestRewriterTagInventory(t *testing.T) {
	full, prefixes, bailReasons := loadTagInventory()
	if len(full) == 0 || len(prefixes) == 0 {
		t.Fatal("inventory load found no constants; rewriter_tags.go not parseable")
	}

	emitted, bailed := extractEmittedLiterals(t, "rewriter.go")
	// Post-PR-B the canonical state is zero string-literal emit sites — every
	// tag is a Tag* / TagPrefix* / TagBailReason* reference. Empty sets here
	// are correct, not "parser broken." If a future PR re-introduces a
	// literal that doesn't trace to the inventory, the un-coverage loop
	// below catches it.

	// Change-tag emissions: must match a Tag* full value, or a TagPrefix*
	// prefix value, or — for emit-sites with concatenated suffix — the
	// LITERAL part must match a TagPrefix* value verbatim.
	var unmatched []string
	for lit := range emitted {
		if isCovered(lit, full, prefixes) {
			continue
		}
		unmatched = append(unmatched, lit)
	}
	if len(unmatched) > 0 {
		sort.Strings(unmatched)
		t.Errorf("change-tag literals in rewriter.go not covered by inventory (%d):", len(unmatched))
		for _, l := range unmatched {
			t.Errorf("  %q", l)
		}
		t.Logf("Add each to rewriter_tags.go: a static literal becomes a Tag* const; a prefix used with `+` becomes a TagPrefix* const.")
	}

	// Bail-reason literals: must match a TagBailReason* value (full
	// strings) or a TagPrefix* whose value, prefixed with "skip:", is
	// known to flow through bail(); for the static-reason variants we
	// match against TagBailReason* directly. Concatenated bail reasons
	// like bail("unknown-decoder:"+swDecoder) — the literal is the
	// prefix.
	var unmatchedBail []string
	for lit := range bailed {
		if isCovered(lit, bailReasons, prefixes) {
			continue
		}
		// Some bail reasons re-use change-tag prefixes (e.g.
		// "hw-decode-sub:no-inlineass-filter" is a bail reason whose
		// prefix appears in TagPrefixBailHWDecodeSubUnmodeled). Also
		// accept any full Tag* / prefix from the change-tag pool —
		// drift between the two pools is fine for now.
		if isCovered(lit, full, prefixes) {
			continue
		}
		unmatchedBail = append(unmatchedBail, lit)
	}
	if len(unmatchedBail) > 0 {
		sort.Strings(unmatchedBail)
		t.Errorf("bail() reason literals not covered by inventory (%d):", len(unmatchedBail))
		for _, l := range unmatchedBail {
			t.Errorf("  %q", l)
		}
		t.Logf("Add each to rewriter_tags.go: a static reason becomes a TagBailReason* const; a prefix used with `+` becomes a TagPrefixBail* const.")
	}
}

// isCovered reports whether lit matches any full string in `fulls` exactly,
// or has any string in `prefixes` as a non-empty prefix.
func isCovered(lit string, fulls map[string]bool, prefixes map[string]bool) bool {
	if fulls[lit] {
		return true
	}
	for p := range prefixes {
		if p != "" && strings.HasPrefix(lit, p) {
			return true
		}
	}
	return false
}

// loadTagInventory parses rewriter_tags.go and returns the sets of full,
// prefix and bail-reason constant values keyed by their string value.
func loadTagInventory() (full, prefixes, bailReasons map[string]bool) {
	full = map[string]bool{}
	prefixes = map[string]bool{}
	bailReasons = map[string]bool{}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "rewriter_tags.go", nil, 0)
	if err != nil {
		return
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			name := vs.Names[0].Name
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			val := strings.Trim(lit.Value, "`\"")
			switch {
			case strings.HasPrefix(name, "TagPrefix"):
				prefixes[val] = true
			case strings.HasPrefix(name, "TagBailReason"):
				bailReasons[val] = true
			case strings.HasPrefix(name, "Tag"):
				full[val] = true
			}
		}
	}
	return
}

// extractEmittedLiterals AST-parses rewriter.go and returns:
//
//	emitted     — the LEADING string literal of every value fed to
//	              append(<*>Changes, ...). For `"prefix:"+x` the leading
//	              literal is "prefix:"; for fmt.Sprintf("fmt-string", ...)
//	              it's the format string; for a bare "literal" it's that
//	              literal. For a bare *ident* whose value was assigned
//	              from a string literal (one or more times across the
//	              file), the set of those literals is included — handles
//	              the `tag := "A"; if cond { tag = "B" }; append(..., tag)`
//	              shape. Non-literal values contribute nothing.
//	bailReasons — the LEADING literal of the first arg to bail(...). Same
//	              rules apply: bail("prefix:"+x) → "prefix:".
//
// Capturing only the leading literal avoids false positives from concat
// trailers like `"video:hdr-source("+t+")"` where ")" is structural —
// the prefix carries the meaning, the trailer is just a close-paren.
func extractEmittedLiterals(t *testing.T, path string) (emitted, bailReasons map[string]bool) {
	t.Helper()
	emitted = map[string]bool{}
	bailReasons = map[string]bool{}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	// Pre-pass: collect every `name := "lit"`, `name = "lit"`, and
	// `var name = "lit"` so the second pass can resolve bare-ident
	// emit sites like `tag := "..."; append(*changes, tag)`. Multiple
	// literals per name accumulate (covers the `tag := "A"; tag = "B"`
	// branching pattern). File-scoped, not function-scoped — collisions
	// are deliberate: an unused literal in another function still
	// counts as covered, which is the conservative behavior (false
	// negatives in the test result only, never false positives).
	varLiterals := collectVarLiterals(f)

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		switch fn.Name {
		case "bail":
			if len(call.Args) >= 1 {
				for _, lit := range allLeadingLiterals(call.Args[0], varLiterals) {
					bailReasons[lit] = true
				}
			}
		case "append":
			if len(call.Args) >= 2 && isChangesIdent(call.Args[0]) {
				for _, arg := range call.Args[1:] {
					for _, lit := range allLeadingLiterals(arg, varLiterals) {
						emitted[lit] = true
					}
				}
			}
		}
		return true
	})
	return
}

// collectVarLiterals AST-walks f and returns a map of identifier name →
// set of string-literal values ever assigned to it. Picks up:
//
//	tag := "literal"            (short var decl)
//	tag = "literal"             (assignment)
//	var tag = "literal"         (var decl with initializer)
//
// and assignments that look like `name := <known-tag-const>` or
// `name = <known-tag-const>` (resolves package-level const idents to
// their string value if they're TagPrefix*/Tag* in rewriter_tags.go).
// File-scoped — same-named vars in different functions all funnel into
// the same key. That's a deliberate over-collection: the inventory
// test asks "is this string ever emitted?", and a literal assigned to
// `tag` anywhere counts the moment we see `append(..., tag)`.
func collectVarLiterals(f *ast.File) map[string][]string {
	// First pass: pick up known const idents so `tag := TagFoo`
	// resolves to TagFoo's value.
	constLits := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			if lit, ok := vs.Values[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				constLits[vs.Names[0].Name] = strings.Trim(lit.Value, "`\"")
			}
		}
	}

	out := map[string][]string{}
	add := func(name string, rhs ast.Expr) {
		switch v := rhs.(type) {
		case *ast.BasicLit:
			if v.Kind == token.STRING {
				out[name] = append(out[name], strings.Trim(v.Value, "`\""))
			}
		case *ast.Ident:
			if lit, ok := constLits[v.Name]; ok {
				out[name] = append(out[name], lit)
			}
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			// `name := "lit"`, `name = "lit"`, or paired
			// `name1, name2 := ...` (we only walk per-position pairs).
			if len(s.Lhs) != len(s.Rhs) {
				return true
			}
			for i := range s.Lhs {
				if id, ok := s.Lhs[i].(*ast.Ident); ok {
					add(id.Name, s.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			// `var name = "lit"` (and same-position lists).
			if len(s.Values) != len(s.Names) {
				return true
			}
			for i, id := range s.Names {
				add(id.Name, s.Values[i])
			}
		}
		return true
	})
	return out
}

// allLeadingLiterals returns every leading literal value of expr, looking
// through the var map for bare *ast.Ident references. Returns nil if
// none. Wrapper around leadingLiteral that adds the ident-resolution
// step.
func allLeadingLiterals(expr ast.Expr, varMap map[string][]string) []string {
	if id, ok := expr.(*ast.Ident); ok {
		if lits := varMap[id.Name]; len(lits) > 0 {
			return lits
		}
	}
	if lit := leadingLiteral(expr); lit != "" {
		return []string{lit}
	}
	// Concat / Sprintf with a leading Ident: also resolve.
	if be, ok := expr.(*ast.BinaryExpr); ok && be.Op == token.ADD {
		return allLeadingLiterals(be.X, varMap)
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" {
				if strings.HasPrefix(sel.Sel.Name, "Sprint") && len(call.Args) > 0 {
					return allLeadingLiterals(call.Args[0], varMap)
				}
			}
		}
	}
	return nil
}

// leadingLiteral returns the leftmost string literal value of expr, peeling
// string-concatenation and fmt.Sprintf-style wrappers. Returns "" if no
// usable literal is found (e.g. expr is a single variable reference).
//
// Handled shapes:
//
//	"foo"                            → "foo"
//	"foo" + x                        → "foo"
//	"foo" + x + "bar"                → "foo"   (trailer ignored — see comment in caller)
//	fmt.Sprintf("foo=%s", x)         → "foo=%s"
//	fmt.Sprintf("foo=%d", n)         → "foo=%d"
//	x                                 → ""      (no literal anchor → skip)
//
// For ident-anchored shapes (`tag := "A"; append(..., tag)`), use
// allLeadingLiterals with a populated var map instead.
func leadingLiteral(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			return strings.Trim(e.Value, "`\"")
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return leadingLiteral(e.X)
		}
	case *ast.CallExpr:
		// fmt.Sprintf(format, args...) — extract the format string.
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" {
				if strings.HasPrefix(sel.Sel.Name, "Sprint") && len(e.Args) > 0 {
					return leadingLiteral(e.Args[0])
				}
			}
		}
	}
	return ""
}

// isChangesIdent returns true if expr is an identifier — or a deref
// of one (`*changes`) — whose name ends in "Changes" or is "changes" /
// "merged". Covers `changes`, `bailChanges`, `hintChanges`,
// `oclChanges`, `manifestChanges`, `scrubChanges`, `merged`, and the
// pointer-receiver form used by rewriteSegmentList:
// `*changes = append(*changes, ...)`. The deref form was a v1.7.0
// release-gate finding — the two segment-list change tags emitted via
// `*changes = append(*changes, tag)` slipped past the walker.
func isChangesIdent(expr ast.Expr) bool {
	// Pointer deref `*changes` parses to *ast.StarExpr in Go AST,
	// not UnaryExpr. (UnaryExpr covers `!x`, `-x`, `&x` etc; the `*`
	// for pointer-deref-as-expression has its own node type.)
	if s, ok := expr.(*ast.StarExpr); ok {
		return isChangesIdent(s.X)
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	if id.Name == "changes" || id.Name == "merged" {
		return true
	}
	return strings.HasSuffix(id.Name, "Changes")
}

// TestRewriterTagInventory_ConstsAreUnique guards against typo-driven
// dupes where two named constants accidentally share the same value.
func TestRewriterTagInventory_ConstsAreUnique(t *testing.T) {
	full, prefixes, bail := loadTagInventory()
	seen := map[string]string{}
	add := func(pool string, set map[string]bool) {
		for v := range set {
			if prev, dup := seen[v]; dup {
				t.Errorf("duplicate tag value %q (in %s and %s)", v, prev, pool)
				continue
			}
			seen[v] = pool
		}
	}
	add("Tag*", full)
	add("TagPrefix*", prefixes)
	add("TagBailReason*", bail)
}

// TestTagValues_Stable pins every Tag* / TagPrefix* / TagBailReason* constant
// to its expected string value. Locks the canon against silent renames done
// in rewriter_tags.go itself — the inventory test catches a literal in
// rewriter.go that drifts from the canon, but it can't catch a rename done
// in lockstep on both sides. This map is the immovable contract; updating a
// value here is the explicit "yes, this rename is intentional" gate.
//
// Loaded via the same AST helper as the inventory test so a const renamed
// from Tag* to something else (or accidentally moved into a non-const decl)
// surfaces as a "missing const" failure, not a silent skip.
func TestTagValues_Stable(t *testing.T) {
	want := map[string]string{
		// Tag* full literals
		"TagAddMapInlineass":                  "add:-map_inlineass",
		"TagBailSegmentListRewriteToRelay":    "bail:segment_list:rewrite-to-relay",
		"TagCanThrottleDisabledByEnv":         "canthrottle:disabled-by-env",
		"TagDropEAEPrefixBail":                "drop:-eae_prefix(bail)",
		"TagDropInlineassDecodeSink":          "drop:inlineass-decode-sink",
		"TagDropNostats":                      "drop:-nostats",
		"TagDropProgressurlBail":              "drop:-progressurl(bail)",
		"TagEnvHOME":                          "env:HOME",
		"TagEnvLIBVA":                         "env:LIBVA",
		"TagFilterTonemapOpenCLNormalized":    "filter:tonemap_opencl-normalized",
		"TagFilterTonemapOpenCLToVAAPI":       "filter:tonemap_opencl->tonemap_vaapi",
		"TagForceHWWouldHonorHWDecSWEnc":      "force-hw:would-honor-hwdec-swenc",
		"TagForceHWWouldHonorSW":              "force-hw:would-honor-sw",
		"TagPassGateDenied":                   "pass-gate:denied-honor-sw",
		"TagHLSSegmentListRewriteToRelay":     "hls:segment_list:rewrite-to-relay",
		"TagHWDecodeFilterBitmapInlineassVA":  "hw-decode:filter:bitmap-inlineass-vaapi",
		"TagHWDecodeFilterInlineassVA":        "hw-decode:filter:inlineass-vaapi",
		"TagHWDecodeFilterOCLToVAAPIIA":       "hw-decode:filter:opencl-tonemap->vaapi:inlineass-vaapi",
		"TagHWDecodeMapLabelUpdate":           "hw-decode:map-label-update",
		"TagHonorPlexHWDecSWEnc":              "honor:plex-hwdec-swenc",
		"TagHonorPlexSW":                      "honor:plex-sw",
		"TagInjectCanThrottleURL":             "inject:-canthrottleurl(scaleplex-ffmpeg7-canThrottle)",
		"TagInjectInitHWDevice":               "inject:init_hw_device+filter_hw_device",
		"TagInjectSEIA53CC":                   "inject:sei+a53_cc",
		"TagLoglevelInfo":                     "loglevel:->info",
		"TagMapLabelUpdate":                   "map-label-update",
		"TagProgressAppendXPlexToken":         "progress:append-X-Plex-Token",
		"TagProgressURLCapturedForReporter":   "progressurl:captured-for-reporter",
		"TagSubsSideChannelSegListToRelay":    "subs:side-channel-segment_list:rewrite-to-relay",
		"TagTonemapOCLCollapseRevmapDownload": "tonemap:ocl:collapse-revmap-download",
		"TagTonemapOCLDropLeadHWUpload":       "tonemap:ocl:drop-lead-hwupload",
		"TagTonemapOCLForceOutputFormatVA":    "tonemap:ocl:force-output-format-vaapi",
		"TagTonemapOCLInjectOpenCLDevice":     "tonemap:ocl:inject-opencl-device",

		// TagPrefix* dynamic-suffix prefixes
		"TagPrefixAudio":                       "audio:",
		"TagPrefixBailFilterPattern":           "filter-pattern:",
		"TagPrefixBailHWDecodeSubUnmodeled":    "hw-decode-sub:unmodeled-graph:",
		"TagPrefixBailUnexpectedEncoder":       "hw-decode:unexpected-encoder:",
		"TagPrefixBailUnknownDecoder":          "unknown-decoder:",
		"TagPrefixBailUnknownEncoder":          "unknown-encoder:",
		"TagPrefixDecode":                      "decode:",
		"TagPrefixDecodeBareHWUpgrade":         "decode:bare-hw-upgrade:",
		"TagPrefixDecodeHWPassthrough":         "decode:hw-passthrough:",
		"TagPrefixDrop":                        "drop:",
		"TagPrefixEncode":                      "encode:",
		"TagPrefixEncodeHWPassthrough":         "encode:hw-passthrough:",
		"TagPrefixEnvStrip":                    "env:strip:",
		"TagPrefixFilter":                      "filter:",
		"TagPrefixForceHWReshapeHybrid":        "force-hw:reshape-hybrid:",
		"TagPrefixHWDecodeFilterBitmapHDRTM":   "hw-decode:filter:bitmap-inlineass-vaapi:hdr-tonemap(",
		"TagPrefixHWDecodeSubTonemapPreserved": "hw-decode-sub:tonemap-preserved(",
		"TagPrefixSeekOffsetCaptured":          "seek-offset:captured=%.3fs",
		"TagPrefixSkip":                        "skip:",
		"TagPrefixSkipToSegmentPassthrough":    "skip_to_segment:passthrough=",
		"TagPrefixSubtitleBitmap":              "subtitle:bitmap:",
		"TagPrefixVideoHDRSource":              "video:hdr-source(",

		// TagBailReason* static bail() arguments
		"TagBailReasonHWDecodeSubBitmapUnsupported": "hw-decode-sub:bitmap-unsupported",
		"TagBailReasonHWDecodeSubNoInlineass":       "hw-decode-sub:no-inlineass-filter",
		"TagBailReasonNoDecoder":                    "no-decoder",
		"TagBailReasonNoEncoder":                    "no-encoder",
		"TagBailReasonNoInput":                      "no-input",
		"TagBailReasonNoVideoFilter":                "no-video-filter",
		"TagBailReasonSubtitlesBurnIn":              "subtitles-burn-in",
	}

	got := loadTagInventoryByName()
	for name, expected := range want {
		actual, ok := got[name]
		if !ok {
			t.Errorf("missing constant %s — was it renamed or removed?", name)
			continue
		}
		if actual != expected {
			t.Errorf("%s = %q, want %q — value drifted; update TestTagValues_Stable IF intentional",
				name, actual, expected)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("new constant %s = %q in rewriter_tags.go not pinned here — add to TestTagValues_Stable",
				name, got[name])
		}
	}
}

// loadTagInventoryByName parses rewriter_tags.go and returns every
// Tag* / TagPrefix* / TagBailReason* constant keyed by its name.
func loadTagInventoryByName() map[string]string {
	out := map[string]string{}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "rewriter_tags.go", nil, 0)
	if err != nil {
		return out
	}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) == 0 || len(vs.Values) == 0 {
				continue
			}
			name := vs.Names[0].Name
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			if !strings.HasPrefix(name, "Tag") {
				continue
			}
			out[name] = strings.Trim(lit.Value, "`\"")
		}
	}
	return out
}


// TestWalker_TracksAssignedLiterals exercises the := / = / var
// assignment-tracking added to extractEmittedLiterals so an emit
// site that goes through a local variable (`tag := "foo"; append(c,
// tag)`) is still recognised as covered by the inventory.
//
// Reproduces the v1.7.0 release-gate finding: rewriter.go's
// rewriteSegmentList emitted "hls:segment_list:rewrite-to-relay" and
// "subs:side-channel-segment_list:rewrite-to-relay" through a local
// `tag` that was conditionally reassigned — the walker missed both
// because it only followed direct literals or `"prefix:" + x`.
func TestWalker_TracksAssignedLiterals(t *testing.T) {
	src := `package x

const TagKnown = "known:const"

func emit(changes *[]string) {
	// 1. := literal
	a := "shape-a:literal"
	*changes = append(*changes, a)

	// 2. := literal + later = literal reassignment
	b := "shape-b:default"
	if true {
		b = "shape-b:override"
	}
	*changes = append(*changes, b)

	// 3. := <const ident>
	c := TagKnown
	*changes = append(*changes, c)

	// 4. var name = literal
	var d = "shape-d:var"
	*changes = append(*changes, d)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse synthetic: %v", err)
	}
	varLits := collectVarLiterals(f)

	for name, want := range map[string][]string{
		"a": {"shape-a:literal"},
		"b": {"shape-b:default", "shape-b:override"},
		"c": {"known:const"},
		"d": {"shape-d:var"},
	} {
		got := varLits[name]
		// Compare as sets (order is collection-dependent).
		gotSet := map[string]bool{}
		for _, s := range got {
			gotSet[s] = true
		}
		for _, w := range want {
			if !gotSet[w] {
				t.Errorf("var %q: missing literal %q (got %v)", name, w, got)
			}
		}
		if len(got) != len(want) {
			t.Errorf("var %q: literal count = %d, want %d (got %v)", name, len(got), len(want), got)
		}
	}

	// Sweep every append(*changes, X) in the synthetic and assert
	// all 5 distinct literals (1 + 2 + 1 + 1) surface in the
	// emitted set.
	emitted := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "append" {
			return true
		}
		if len(call.Args) < 2 || !isChangesIdent(call.Args[0]) {
			return true
		}
		for _, arg := range call.Args[1:] {
			for _, lit := range allLeadingLiterals(arg, varLits) {
				emitted[lit] = true
			}
		}
		return true
	})

	for _, want := range []string{
		"shape-a:literal",
		"shape-b:default",
		"shape-b:override",
		"known:const",
		"shape-d:var",
	} {
		if !emitted[want] {
			t.Errorf("emitted set missing %q (have %v)", want, emitted)
		}
	}
}

// TestWalker_LeadingLiteralUnchanged pins the simpler leadingLiteral
// contract — bare ident still resolves to "" (the var-aware paths
// live in allLeadingLiterals).
func TestWalker_LeadingLiteralUnchanged(t *testing.T) {
	src := `package x
func f() string {
	x := "tag-x"
	_ = x
	return ""
}
`
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, "synthetic2.go", src, 0)
	var idents []*ast.Ident
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "x" {
			idents = append(idents, id)
		}
		return true
	})
	if len(idents) == 0 {
		t.Fatal("no x idents found in synthetic")
	}
	// leadingLiteral on a bare ident must still return "" (legacy
	// contract — callers route ident shapes through allLeadingLiterals).
	if got := leadingLiteral(idents[0]); got != "" {
		t.Errorf("leadingLiteral on bare ident = %q, want \"\"", got)
	}
}
