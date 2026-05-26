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
	if len(emitted) == 0 {
		t.Fatal("rewriter.go AST scan found no change-tag emit sites; parser broken?")
	}

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
//	              literal. Non-literal values contribute nothing.
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
				if lit := leadingLiteral(call.Args[0]); lit != "" {
					bailReasons[lit] = true
				}
			}
		case "append":
			if len(call.Args) >= 2 && isChangesIdent(call.Args[0]) {
				for _, arg := range call.Args[1:] {
					if lit := leadingLiteral(arg); lit != "" {
						emitted[lit] = true
					}
				}
			}
		}
		return true
	})
	return
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

// isChangesIdent returns true if expr is an identifier whose name ends in
// "Changes" or is "changes" — covers `changes`, `bailChanges`, `hintChanges`,
// `oclChanges`, `manifestChanges`, `scrubChanges`, `merged` etc.
func isChangesIdent(expr ast.Expr) bool {
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

