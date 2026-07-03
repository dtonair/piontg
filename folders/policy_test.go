package folders

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"piontg/config"
)

func TestResolveAllowsRootAndDescendants(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	child := mkdir(t, root, "child")
	policy := mustPolicy(t, root)

	resolvedRoot, rootPolicy, err := policy.Resolve(root)
	if err != nil {
		t.Fatalf("Resolve(root) error = %v", err)
	}
	if resolvedRoot != canonical(t, root) {
		t.Fatalf("resolved root = %q", resolvedRoot)
	}
	if rootPolicy.Trust != config.TrustNoApprove {
		t.Fatalf("trust = %q", rootPolicy.Trust)
	}

	resolvedChild, _, err := policy.Resolve(filepath.Join(root, "..", "root", "child"))
	if err != nil {
		t.Fatalf("Resolve(child) error = %v", err)
	}
	if resolvedChild != canonical(t, child) {
		t.Fatalf("resolved child = %q", resolvedChild)
	}
}

func TestResolveRejectsOutsideTraversalAndSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	outside := mkdir(t, dir, "outside")
	policy := mustPolicy(t, root)

	if _, _, err := policy.Resolve(filepath.Join(root, "..", "outside")); err == nil {
		t.Fatal("Resolve(outside traversal) error = nil")
	}

	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, _, err := policy.Resolve(link)
	if err == nil {
		t.Fatal("Resolve(symlink escape) error = nil")
	}
	if !strings.Contains(err.Error(), "outside allowed roots") {
		t.Fatalf("Resolve(symlink escape) error = %v", err)
	}
}

func TestResolveAllowsSymlinkInsideRoot(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	actual := mkdir(t, root, "actual")
	link := filepath.Join(root, "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	policy := mustPolicy(t, root)

	resolved, _, err := policy.Resolve(link)
	if err != nil {
		t.Fatalf("Resolve(internal symlink) error = %v", err)
	}
	if resolved != canonical(t, actual) {
		t.Fatalf("resolved = %q", resolved)
	}
}

func TestResolveRejectsDeletedAndNonDirectoryPaths(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	policy := mustPolicy(t, root)

	if _, _, err := policy.Resolve(filepath.Join(root, "missing")); err == nil {
		t.Fatal("Resolve(missing) error = nil")
	}

	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.Resolve(file); err == nil {
		t.Fatal("Resolve(file) error = nil")
	}
}

func TestResolveUsesLongestMatchingRootPolicy(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	nested := mkdir(t, root, "nested")
	child := mkdir(t, nested, "child")
	cfg := baseConfig(root)
	cfg.Folders.Roots = append(cfg.Folders.Roots, config.FolderRoot{
		Name:         "nested",
		Path:         nested,
		Trust:        config.TrustApprove,
		Tools:        []string{"read"},
		ExcludeTools: []string{"bash"},
	})
	policy, err := NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}

	_, effective, err := policy.Resolve(child)
	if err != nil {
		t.Fatal(err)
	}
	if effective.Trust != config.TrustApprove {
		t.Fatalf("trust = %q", effective.Trust)
	}
	if len(effective.Tools) != 1 || effective.Tools[0] != "read" {
		t.Fatalf("tools = %#v", effective.Tools)
	}
	if len(effective.ExcludeTools) != 1 || effective.ExcludeTools[0] != "bash" {
		t.Fatalf("exclude tools = %#v", effective.ExcludeTools)
	}
}

func mustPolicy(t *testing.T, root string) *Policy {
	t.Helper()
	policy, err := NewPolicy(baseConfig(root))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func baseConfig(root string) config.Config {
	return config.Config{
		Pi: config.PiConfig{DefaultTrust: config.TrustNoApprove},
		Folders: config.FoldersConfig{
			MaxDepth:   4,
			MaxEntries: 200,
			Roots: []config.FolderRoot{{
				Name:  "root",
				Path:  root,
				Trust: config.TrustNoApprove,
			}},
		},
	}
}

func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func canonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
