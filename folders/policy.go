package folders

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"piontg/config"
)

// EffectivePolicy is the Pi launch policy inherited from the matching allowed root.
type EffectivePolicy struct {
	Trust        string
	Tools        []string
	ExcludeTools []string
}

// Root is a canonical allowed parent folder.
type Root struct {
	Index        int
	Name         string
	Path         string
	Trust        string
	Tools        []string
	ExcludeTools []string
}

// Policy validates and resolves user-selected folders against configured roots.
type Policy struct {
	roots      []Root
	maxDepth   int
	maxEntries int
}

func NewPolicy(cfg config.Config) (*Policy, error) {
	roots := make([]Root, 0, len(cfg.Folders.Roots))
	for i, rootCfg := range cfg.Folders.Roots {
		canonical, err := canonicalDir(rootCfg.Path)
		if err != nil {
			return nil, fmt.Errorf("folders root %q: %w", rootCfg.Path, err)
		}
		roots = append(roots, Root{
			Index:        i,
			Name:         rootCfg.Name,
			Path:         canonical,
			Trust:        rootCfg.Trust,
			Tools:        append([]string(nil), rootCfg.Tools...),
			ExcludeTools: append([]string(nil), rootCfg.ExcludeTools...),
		})
	}
	if len(roots) == 0 {
		return nil, errors.New("at least one allowed root is required")
	}
	return &Policy{roots: roots, maxDepth: cfg.Folders.MaxDepth, maxEntries: cfg.Folders.MaxEntries}, nil
}

func (p *Policy) Roots() []Root {
	roots := make([]Root, len(p.roots))
	copy(roots, p.roots)
	return roots
}

// Resolve validates selectedPath, canonicalizes it, and returns the matching root policy.
func (p *Policy) Resolve(selectedPath string) (string, EffectivePolicy, error) {
	canonical, err := canonicalDir(selectedPath)
	if err != nil {
		return "", EffectivePolicy{}, err
	}
	root, ok := p.matchingRoot(canonical)
	if !ok {
		return "", EffectivePolicy{}, fmt.Errorf("folder %q is outside allowed roots", selectedPath)
	}
	return canonical, EffectivePolicy{
		Trust:        root.Trust,
		Tools:        append([]string(nil), root.Tools...),
		ExcludeTools: append([]string(nil), root.ExcludeTools...),
	}, nil
}

func (p *Policy) TokenForPath(path string) (string, error) {
	canonical, _, err := p.Resolve(path)
	if err != nil {
		return "", err
	}
	return tokenForCanonicalPath(canonical), nil
}

func (p *Policy) matchingRoot(canonicalPath string) (Root, bool) {
	var best Root
	bestLen := -1
	for _, root := range p.roots {
		if isWithin(canonicalPath, root.Path) && len(root.Path) > bestLen {
			best = root
			bestLen = len(root.Path)
		}
	}
	return best, bestLen >= 0
}

func canonicalDir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return filepath.Clean(resolved), nil
}

func isWithin(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func tokenForCanonicalPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:16]
}
