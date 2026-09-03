// Package arch enforces the import-direction rules documented in CLAUDE.md
// and docs/architecture.md. The rules below are a deliberate, explicit table:
// when a legitimate architecture change trips this test, update the table in
// the same commit and say why — that forced decision is the point.
package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/openconvo/openconvo"

type rules struct {
	// pure packages import no other internal/ or cmd/ package (top-level
	// asset packages such as migrations/ don't count). The archive is the
	// source-agnostic heart; storage and jobs are generic leaves.
	pure []string
	// forbidden lists imports a package must never have.
	forbidden map[string][]string
	// onlyImportedBy restricts who may import a package (empty = nobody,
	// outside _test.go files, which are not scanned).
	onlyImportedBy map[string][]string
}

var boundaries = rules{
	pure: []string{
		"internal/archive",
		"internal/config",
		"internal/database",
		"internal/jobs",
		"internal/storage",
		"internal/updates",
		"internal/version",
		"internal/web",
	},
	forbidden: map[string][]string{
		// ingest depends on discord for the normalized types, never the
		// reverse; Source.Run takes a local Ingester interface instead.
		"internal/discord": {"internal/ingest"},
		// attachments reaches Discord only through the local URLRefresher
		// interface, so the pipeline stays source-agnostic.
		"internal/attachments": {"internal/discord"},
	},
	onlyImportedBy: map[string][]string{
		"internal/http":      {"internal/app"},
		"internal/app":       {"cmd/openconvo"},
		"internal/mcpserver": {"cmd/openconvo", "internal/app"},
		"internal/testutil":  {},
	},
}

func TestImportBoundaries(t *testing.T) {
	graph := importGraph(t)
	for _, v := range violations(graph, boundaries) {
		t.Error(v)
	}
}

func TestViolationsDetectsBreakage(t *testing.T) {
	r := rules{
		pure:           []string{"internal/pure", "internal/gone"},
		forbidden:      map[string][]string{"internal/a": {"internal/b"}},
		onlyImportedBy: map[string][]string{"internal/guarded": {"internal/allowed"}},
	}
	graph := map[string][]string{
		"internal/pure":    {"internal/b"},
		"internal/a":       {"internal/b"},
		"internal/allowed": {"internal/guarded"},
		"internal/sneaky":  {"internal/guarded"},
		"internal/b":       {},
		"internal/guarded": {},
	}
	got := violations(graph, r)
	for _, want := range []string{
		`internal/pure is a pure package but imports internal/b`,
		`internal/a must not import internal/b`,
		`internal/sneaky imports internal/guarded`,
		`internal/gone`,
	} {
		if !containsSubstring(got, want) {
			t.Errorf("expected a violation mentioning %q, got %v", want, got)
		}
	}
	if containsSubstring(got, "internal/allowed imports") {
		t.Errorf("allowed importer flagged: %v", got)
	}
}

// A rule whose subject package has been renamed or removed must be
// reported, not silently skipped — otherwise the rule keeps "passing"
// while enforcing nothing.
func TestViolationsReportsRulesNamingMissingPackages(t *testing.T) {
	r := rules{
		pure:           []string{"internal/gone"},
		forbidden:      map[string][]string{"internal/vanished": {"internal/b"}},
		onlyImportedBy: map[string][]string{"internal/departed": {"internal/a"}},
	}
	graph := map[string][]string{"internal/a": {}, "internal/b": {}}
	got := violations(graph, r)
	for _, pkg := range []string{"internal/gone", "internal/vanished", "internal/departed"} {
		if !containsSubstring(got, pkg+" but no such package exists") {
			t.Errorf("rule naming missing package %s not reported, got %v", pkg, got)
		}
	}
}

func violations(graph map[string][]string, r rules) []string {
	var out []string
	// A rule naming a package that no longer exists is a rule that
	// silently stops enforcing anything, which is the rot this table
	// exists to prevent: report it instead of skipping it.
	exists := func(pkg string) bool {
		if _, ok := graph[pkg]; ok {
			return true
		}
		out = append(out, fmt.Sprintf("rules list %s but no such package exists; update the table in boundaries_test.go", pkg))
		return false
	}
	for _, pkg := range r.pure {
		if !exists(pkg) {
			continue
		}
		for _, imp := range graph[pkg] {
			out = append(out, fmt.Sprintf("%s is a pure package but imports %s", pkg, imp))
		}
	}
	for pkg, banned := range r.forbidden {
		if !exists(pkg) {
			continue
		}
		for _, imp := range graph[pkg] {
			for _, b := range banned {
				if imp == b {
					out = append(out, fmt.Sprintf("%s must not import %s", pkg, b))
				}
			}
		}
	}
	for pkg := range r.onlyImportedBy {
		exists(pkg)
	}
	for pkg, imports := range graph {
		for _, imp := range imports {
			allowed, guarded := r.onlyImportedBy[imp]
			if !guarded {
				continue
			}
			ok := false
			for _, a := range allowed {
				if pkg == a {
					ok = true
				}
			}
			if !ok {
				out = append(out, fmt.Sprintf("%s imports %s, which only %v may import", pkg, imp, allowed))
			}
		}
	}
	sort.Strings(out)
	return out
}

// importGraph maps each package directory under internal/ and cmd/ to the
// internal packages its non-test files import.
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	root := moduleRoot(t)
	graph := map[string][]string{}
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(rel)
			if _, ok := graph[pkg]; !ok {
				graph[pkg] = []string{}
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, spec := range f.Imports {
				imp, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				if !strings.HasPrefix(imp, module+"/") {
					continue
				}
				imp = strings.TrimPrefix(imp, module+"/")
				if !strings.HasPrefix(imp, "internal/") && !strings.HasPrefix(imp, "cmd/") {
					continue
				}
				if imp != pkg && !containsString(graph[pkg], imp) {
					graph[pkg] = append(graph[pkg], imp)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", top, err)
		}
	}
	return graph
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsSubstring(list []string, sub string) bool {
	for _, v := range list {
		if strings.Contains(v, sub) {
			return true
		}
	}
	return false
}
