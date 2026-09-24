// Package arch_test enforces the dependency direction described in
// docs/history/PLAN.md section 0 by parsing the import lists of every package.
package arch_test

import (
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

// modulePath is the import prefix of this repository.
const modulePath = "github.com/wesbragagt/syngo"

// sourceFile is one parsed Go file and the imports it declares.
type sourceFile struct {
	// pkgDir is the directory of the file, relative to the repository root,
	// using forward slashes (for example "internal/domain/device").
	pkgDir string
	// path is the file path relative to the repository root.
	path string
	// isTest reports whether the file is a _test.go file.
	isTest bool
	// imports are the quoted import paths, unquoted.
	imports []string
}

// bannedInDomainAndApp are the stdlib packages that would let pure logic reach
// the outside world directly instead of going through a port.
var bannedInDomainAndApp = []string{"os", "net", "database/sql", "syscall", "os/exec"}

// loadFiles parses every Go file under the given repository-relative roots.
func loadFiles(t *testing.T, roots ...string) []sourceFile {
	t.Helper()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}

	var files []sourceFile
	fset := token.NewFileSet()
	for _, root := range roots {
		absRoot := filepath.Join(repoRoot, filepath.FromSlash(root))
		if _, err := os.Stat(absRoot); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			sf := sourceFile{
				pkgDir: filepath.ToSlash(filepath.Dir(rel)),
				path:   rel,
				isTest: strings.HasSuffix(d.Name(), "_test.go"),
			}
			for _, spec := range parsed.Imports {
				unquoted, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				sf.imports = append(sf.imports, unquoted)
			}
			files = append(files, sf)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(files) == 0 {
		t.Fatalf("no Go files found under %v", roots)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files
}

// filesUnder returns the parsed files whose package directory is prefix or
// lies beneath it.
func filesUnder(files []sourceFile, prefix string) []sourceFile {
	var out []sourceFile
	for _, f := range files {
		if f.pkgDir == prefix || strings.HasPrefix(f.pkgDir, prefix+"/") {
			out = append(out, f)
		}
	}
	return out
}

// isInternal reports whether an import path belongs to this module.
func isInternal(imp string) bool {
	return imp == modulePath || strings.HasPrefix(imp, modulePath+"/")
}

// isThirdParty reports whether an import path is an external module. Standard
// library paths never contain a dot in their first segment.
func isThirdParty(imp string) bool {
	if isInternal(imp) {
		return false
	}
	first, _, _ := strings.Cut(imp, "/")
	return strings.Contains(first, ".")
}

// localPath strips the module prefix from an internal import path.
func localPath(imp string) string {
	return strings.TrimPrefix(strings.TrimPrefix(imp, modulePath), "/")
}

// isBanned reports whether imp is one of the stdlib packages that domain and
// app code may not use.
func isBanned(imp string) bool {
	for _, banned := range bannedInDomainAndApp {
		if imp == banned || strings.HasPrefix(imp, banned+"/") {
			return true
		}
	}
	return false
}

func TestDomainImportsStdlibOnly(t *testing.T) {
	for _, f := range filesUnder(loadFiles(t, "internal", "cmd"), "internal/domain") {
		for _, imp := range f.imports {
			switch {
			case isThirdParty(imp):
				t.Errorf("%s imports third-party %q; domain is stdlib only", f.path, imp)
			case isInternal(imp):
				if !strings.HasPrefix(localPath(imp), "internal/domain/") {
					t.Errorf("%s imports %q; domain may only import other domain packages", f.path, imp)
				}
			case isBanned(imp):
				t.Errorf("%s imports %q; domain must stay free of I/O", f.path, imp)
			}
		}
	}
}

func TestAppDoesNotImportOSOrAdapters(t *testing.T) {
	for _, f := range filesUnder(loadFiles(t, "internal", "cmd"), "internal/app") {
		for _, imp := range f.imports {
			switch {
			case isThirdParty(imp):
				t.Errorf("%s imports third-party %q; app depends on ports, not technologies", f.path, imp)
			case isInternal(imp):
				local := localPath(imp)
				if !strings.HasPrefix(local, "internal/domain/") && !strings.HasPrefix(local, "internal/app/") {
					t.Errorf("%s imports %q; app may only import domain and app packages", f.path, imp)
				}
			case isBanned(imp):
				t.Errorf("%s imports %q; app must reach the outside world through a port", f.path, imp)
			}
		}
	}
}

func TestAdaptersDoNotImportEachOther(t *testing.T) {
	files := filesUnder(loadFiles(t, "internal", "cmd"), "internal/adapters")

	for _, f := range files {
		own := f.pkgDir
		for _, imp := range f.imports {
			if !isInternal(imp) {
				continue
			}
			local := localPath(imp)
			switch {
			case strings.HasPrefix(local, "internal/adapters/"):
				if local != own {
					t.Errorf("%s imports adapter %q; adapters are wired together by internal/compose", f.path, imp)
				}
			case strings.HasPrefix(local, "internal/domain/"), local == "internal/app/ports":
			case local == "internal/app/fakes":
				if !f.isTest {
					t.Errorf("%s imports %q outside a test file", f.path, imp)
				}
			default:
				t.Errorf("%s imports %q; adapters may only import domain, app/ports and third-party code", f.path, imp)
			}
		}
	}
}

func TestCLIImportsAllowedPackagesOnly(t *testing.T) {
	for _, f := range filesUnder(loadFiles(t, "internal", "cmd"), "internal/cli") {
		for _, imp := range f.imports {
			if isThirdParty(imp) && !strings.HasPrefix(imp, "github.com/spf13/") {
				t.Errorf("%s imports third-party %q; the CLI may only use cobra", f.path, imp)
			}
			if !isInternal(imp) {
				continue
			}
			local := localPath(imp)
			allowed := strings.HasPrefix(local, "internal/app/") ||
				strings.HasPrefix(local, "internal/domain/") ||
				local == "internal/compose" ||
				local == "internal/cli" ||
				local == "internal/tui"
			if !allowed {
				t.Errorf("%s imports %q; the CLI may only import app, domain, compose, the TUI and itself", f.path, imp)
			}
		}
	}
}

// TestTUIImportsNothingOfTheApplication keeps the terminal interface a
// presentation layer. It reaches the daemon only through the tui.Runner the
// CLI implements for it, which means every screen is a view of a command
// docs/CLI.md documents and there is no second copy of an application rule
// inside it. The dependency runs cli -> tui and never the other way, so this
// rule and the one above together forbid a cycle as well.
func TestTUIImportsNothingOfTheApplication(t *testing.T) {
	// The Bubble Tea set, and only it: docs/TUI.md states the three
	// packages the interface is built from.
	allowedThirdParty := []string{
		"github.com/charmbracelet/bubbletea",
		"github.com/charmbracelet/bubbles/",
		"github.com/charmbracelet/lipgloss",
	}
	for _, f := range filesUnder(loadFiles(t, "internal", "cmd"), "internal/tui") {
		for _, imp := range f.imports {
			if isThirdParty(imp) {
				ok := false
				for _, prefix := range allowedThirdParty {
					if imp == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(imp, prefix) {
						ok = true
					}
				}
				if !ok {
					t.Errorf("%s imports third-party %q; the TUI may only use bubbletea, bubbles and lipgloss", f.path, imp)
				}
				continue
			}
			if !isInternal(imp) {
				continue
			}
			if local := localPath(imp); local != "internal/tui" {
				t.Errorf("%s imports %q; the TUI is a presentation layer and may reach the daemon only through tui.Runner", f.path, imp)
			}
		}
	}
}

func TestCmdImportsOnlyCLI(t *testing.T) {
	for _, f := range filesUnder(loadFiles(t, "internal", "cmd"), "cmd/syngo") {
		for _, imp := range f.imports {
			if isThirdParty(imp) {
				t.Errorf("%s imports third-party %q; the entry point only wires up the CLI", f.path, imp)
			}
			if isInternal(imp) && localPath(imp) != "internal/cli" {
				t.Errorf("%s imports %q; the entry point may only import internal/cli", f.path, imp)
			}
		}
	}
}

// TestBenchCmdImportsOnlyItsRunner keeps cmd/syngo-bench as thin as
// cmd/syngo: it parses flags and calls internal/bench/runner, nothing
// else internal, and no third-party package (see docs/history/PLAN.md section 0's
// closed dependency list).
func TestBenchCmdImportsOnlyItsRunner(t *testing.T) {
	for _, f := range filesUnder(loadFiles(t, "internal", "cmd"), "cmd/syngo-bench") {
		for _, imp := range f.imports {
			if isThirdParty(imp) {
				t.Errorf("%s imports third-party %q; cmd/syngo-bench only wires up internal/bench/runner", f.path, imp)
			}
			if isInternal(imp) {
				local := localPath(imp)
				if local != "internal/bench/runner" && local != "internal/bench/report" {
					t.Errorf("%s imports %q; cmd/syngo-bench may only import internal/bench/runner and internal/bench/report", f.path, imp)
				}
			}
		}
	}
}

// TestTestutilIsImportedOnlyByTests keeps the test helpers in
// internal/testutil out of the shipped binary. They are normal packages so
// that two separate test packages can share them, which means only the import
// list can say that nothing but a test uses them.
func TestTestutilIsImportedOnlyByTests(t *testing.T) {
	for _, f := range loadFiles(t, "internal", "cmd") {
		if f.isTest || strings.HasPrefix(f.pkgDir, "internal/testutil") {
			continue
		}
		for _, imp := range f.imports {
			if isInternal(imp) && strings.HasPrefix(localPath(imp), "internal/testutil/") {
				t.Errorf("%s imports %q outside a test file; test helpers never ship", f.path, imp)
			}
		}
	}
}
