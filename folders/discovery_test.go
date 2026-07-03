package folders

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverReturnsRootAndSortedDescendantsWithinDepth(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	mkdir(t, root, "b")
	a := mkdir(t, root, "a")
	mkdir(t, a, "deep")
	policy := mustPolicy(t, root)
	policy.maxDepth = 1

	choices, err := policy.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	labels := labelsOf(choices)
	want := []string{"root", "root/a", "root/b"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
	if choices[0].Depth != 0 || choices[1].Depth != 1 || choices[2].Depth != 1 {
		t.Fatalf("depths = %#v", []int{choices[0].Depth, choices[1].Depth, choices[2].Depth})
	}
}

func TestDiscoverHonorsMaxDepthAndMaxEntries(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	a := mkdir(t, root, "a")
	mkdir(t, a, "deep")
	mkdir(t, root, "b")
	policy := mustPolicy(t, root)
	policy.maxDepth = 2
	policy.maxEntries = 2

	choices, err := policy.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	labels := labelsOf(choices)
	want := []string{"root", "root/a"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
}

func TestDiscoverSkipsGitNonDirectoriesBrokenLinksAndEscapingSymlinks(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	outside := mkdir(t, dir, "outside")
	mkdir(t, root, ".git")
	mkdir(t, root, "ok")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	policy := mustPolicy(t, root)

	choices, err := policy.Discover()
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	labels := labelsOf(choices)
	want := []string{"root", "root/ok"}
	if !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
}

func TestResolveTokenPerformsFreshValidation(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	child := mkdir(t, root, "child")
	policy := mustPolicy(t, root)
	choices, err := policy.Discover()
	if err != nil {
		t.Fatal(err)
	}
	var token string
	for _, choice := range choices {
		if choice.Path == canonical(t, child) {
			token = choice.Token
		}
	}
	if token == "" {
		t.Fatal("child token not found")
	}

	resolved, _, err := policy.ResolveToken(token)
	if err != nil {
		t.Fatalf("ResolveToken() error = %v", err)
	}
	if resolved != canonical(t, child) {
		t.Fatalf("resolved = %q", resolved)
	}

	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if _, _, err := policy.ResolveToken(token); err == nil {
		t.Fatal("ResolveToken(deleted child) error = nil")
	}
}

func TestTokenForPathIsStableAndMatchesDiscover(t *testing.T) {
	dir := t.TempDir()
	root := mkdir(t, dir, "root")
	child := mkdir(t, root, "child")
	policy := mustPolicy(t, root)

	token, err := policy.TokenForPath(child)
	if err != nil {
		t.Fatal(err)
	}
	choices, err := policy.Discover()
	if err != nil {
		t.Fatal(err)
	}
	for _, choice := range choices {
		if choice.Path == canonical(t, child) && choice.Token == token {
			return
		}
	}
	t.Fatalf("token %q for child not found in choices %#v", token, choices)
}

func labelsOf(choices []Choice) []string {
	labels := make([]string, len(choices))
	for i, choice := range choices {
		labels[i] = choice.Label
	}
	return labels
}
