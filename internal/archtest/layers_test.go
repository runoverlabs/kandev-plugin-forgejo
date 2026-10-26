// Package archtest holds the layer rules of docs/architecture.md as a test, so
// an import that crosses a layer fails `go test ./...` rather than waiting for
// a reviewer to notice.
package archtest

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	module = "kandev-plugin-forgejo"
	sdk    = "github.com/kandev/kandev/pkg/pluginsdk"
)

// layer is what one package may import beyond the standard library: the
// module's own packages, the SDK, and any other third-party prefix.
type layer struct {
	internal []string
	sdk      bool
	external []string
}

// Foundation, features, adapters, transport, entry: an import only ever points
// to a package listed for it, which by construction is a lower layer.
var (
	features = []string{"internal/sourcecontrol", "internal/watches", "internal/automations", "internal/prautomation"}

	layers = map[string]layer{
		// Foundation.
		"internal/scm":               {},
		"internal/kandev":            {sdk: true},
		"internal/kandev/kandevtest": {internal: []string{"internal/kandev"}, sdk: true},

		// Features. sourcecontrol is the base feature the others may build on;
		// no feature imports another otherwise.
		"internal/sourcecontrol": {internal: []string{"internal/scm", "internal/kandev"}, sdk: true},
		"internal/watches":       {internal: []string{"internal/scm", "internal/kandev", "internal/sourcecontrol"}, sdk: true},
		"internal/automations":   {internal: []string{"internal/scm", "internal/kandev", "internal/sourcecontrol"}, sdk: true},
		"internal/prautomation":  {internal: []string{"internal/scm", "internal/kandev", "internal/sourcecontrol"}, sdk: true},

		// Adapters. The wire client knows nothing of Kandev or the features.
		"internal/forgejo/rest":          {internal: []string{"internal/scm"}},
		"internal/forgejo/rest/resttest": {},
		"internal/forgejo": {
			internal: append([]string{"internal/scm", "internal/kandev", "internal/forgejo/rest"}, features...),
			sdk:      true,
		},

		// Transport. It reaches the instance through ports; the one file that
		// names an adapter is checked separately below.
		"internal/plugin": {
			internal: append([]string{"internal/scm", "internal/kandev"}, features...),
			sdk:      true,
			external: []string{"google.golang.org/grpc/codes", "google.golang.org/grpc/status"},
		},

		// Entry.
		"server": {internal: []string{"internal/plugin"}, sdk: true},
	}

	// compositionRoots may also import the adapter: they build the graph.
	compositionRoots = map[string][]string{
		"internal/plugin/wire.go": {"internal/forgejo"},
	}

	// testSupport packages exist for tests and must not reach production code.
	testSupport = []string{"internal/kandev/kandevtest", "internal/forgejo/rest/resttest"}
)

func TestImportsFollowTheLayers(t *testing.T) {
	root := moduleRoot(t)
	files := goFiles(t, root)
	if len(files) == 0 {
		t.Fatal("found no Go files; is the module root right?")
	}
	seen := map[string]bool{}
	for _, file := range files {
		pkg := filepath.ToSlash(filepath.Dir(file.rel))
		if pkg == "internal/archtest" {
			continue
		}
		rule, known := layers[pkg]
		if !known {
			t.Errorf("%s: package %s is not in the layer table; add it to internal/archtest and docs/architecture.md", file.rel, pkg)
			continue
		}
		seen[pkg] = true
		for _, path := range file.imports {
			if reason := check(rule, file, path); reason != "" {
				t.Errorf("%s imports %s: %s", file.rel, path, reason)
			}
		}
	}
	for pkg := range layers {
		if !seen[pkg] {
			t.Errorf("layer table lists %s, which has no Go files any more", pkg)
		}
	}
}

func check(rule layer, file goFile, path string) string {
	switch {
	case strings.HasPrefix(path, module+"/"):
		target := strings.TrimPrefix(path, module+"/")
		if slices.Contains(testSupport, target) {
			if !file.test {
				return "test support may only be imported from a _test.go file"
			}
			return ""
		}
		if file.test {
			// A test may reach across layers to set up what it drives.
			return ""
		}
		if slices.Contains(rule.internal, target) || slices.Contains(compositionRoots[file.rel], target) {
			return ""
		}
		return "crosses a layer (see docs/architecture.md)"
	case path == sdk:
		if rule.sdk || file.test {
			return ""
		}
		return "this layer must not depend on the Kandev SDK"
	case !strings.Contains(strings.SplitN(path, "/", 2)[0], "."):
		return "" // standard library
	case file.test:
		return ""
	case slices.Contains(rule.external, path):
		return ""
	default:
		return "a third-party import this layer does not list"
	}
}

type goFile struct {
	rel     string
	test    bool
	imports []string
}

func goFiles(t *testing.T, root string) []goFile {
	t.Helper()
	var out []goFile
	fset := token.NewFileSet()
	for _, top := range []string{"internal", "server"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if name := entry.Name(); name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			file := goFile{rel: filepath.ToSlash(rel), test: strings.HasSuffix(path, "_test.go")}
			for _, spec := range parsed.Imports {
				unquoted, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					return err
				}
				file.imports = append(file.imports, unquoted)
			}
			out = append(out, file)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
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
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
