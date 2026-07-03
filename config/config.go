package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	TrustApprove   = "approve"
	TrustNoApprove = "no-approve"

	StreamingFollowUp = "follow_up"
	StreamingSteer    = "steer"
)

// Config is the complete application configuration loaded from YAML plus env.
type Config struct {
	Telegram TelegramConfig `yaml:"telegram"`
	Pi       PiConfig       `yaml:"pi"`
	Folders  FoldersConfig  `yaml:"folders"`
	State    StateConfig    `yaml:"state"`
}

type TelegramConfig struct {
	Token         string `yaml:"token"`
	TokenEnv      string `yaml:"tokenEnv"`
	AllowedUserID int64  `yaml:"allowedUserId"`
}

type PiConfig struct {
	Binary                   string   `yaml:"binary"`
	SessionDir               string   `yaml:"sessionDir"`
	DefaultTrust             string   `yaml:"defaultTrust"`
	DefaultStreamingBehavior string   `yaml:"defaultStreamingBehavior"`
	Tools                    []string `yaml:"tools"`
	ExcludeTools             []string `yaml:"excludeTools"`
}

type FoldersConfig struct {
	MaxDepth   int          `yaml:"maxDepth"`
	MaxEntries int          `yaml:"maxEntries"`
	Roots      []FolderRoot `yaml:"roots"`
}

type FolderRoot struct {
	Name         string   `yaml:"name"`
	Path         string   `yaml:"path"`
	Trust        string   `yaml:"trust"`
	Tools        []string `yaml:"tools"`
	ExcludeTools []string `yaml:"excludeTools"`
}

type StateConfig struct {
	Dir string `yaml:"dir"`
}

// Load reads, normalizes, and validates a config file.
func Load(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("config path is required")
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()

	var cfg Config
	dec := yaml.NewDecoder(file)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) normalize() error {
	if c.Pi.Binary == "" {
		c.Pi.Binary = "pi"
	}
	if c.Pi.DefaultTrust == "" {
		c.Pi.DefaultTrust = TrustNoApprove
	}
	if c.Pi.DefaultStreamingBehavior == "" {
		c.Pi.DefaultStreamingBehavior = StreamingFollowUp
	}
	if c.State.Dir == "" {
		c.State.Dir = "~/.local/state/piontg"
	}
	stateDir, err := expandPath(c.State.Dir)
	if err != nil {
		return fmt.Errorf("state.dir: %w", err)
	}
	c.State.Dir = stateDir

	if c.Pi.SessionDir == "" {
		c.Pi.SessionDir = filepath.Join(c.State.Dir, "pi-sessions")
	} else {
		sessionDir, err := expandPath(c.Pi.SessionDir)
		if err != nil {
			return fmt.Errorf("pi.sessionDir: %w", err)
		}
		c.Pi.SessionDir = sessionDir
	}

	if c.Folders.MaxDepth == 0 {
		c.Folders.MaxDepth = 4
	}
	if c.Folders.MaxEntries == 0 {
		c.Folders.MaxEntries = 200
	}

	for i := range c.Folders.Roots {
		root := &c.Folders.Roots[i]
		if root.Trust == "" {
			root.Trust = c.Pi.DefaultTrust
		}
		if root.Path != "" {
			expanded, err := expandPath(root.Path)
			if err != nil {
				return fmt.Errorf("folders.roots[%d].path: %w", i, err)
			}
			root.Path = expanded
		}
		if root.Name == "" && root.Path != "" {
			root.Name = filepath.Base(root.Path)
		}
	}

	if c.Telegram.Token == "" && c.Telegram.TokenEnv != "" {
		c.Telegram.Token = os.Getenv(c.Telegram.TokenEnv)
	}

	return nil
}

// Validate verifies semantic config requirements after normalization.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Telegram.Token) == "" {
		errs = append(errs, errors.New("telegram token is required via telegram.token or telegram.tokenEnv"))
	}
	if c.Telegram.AllowedUserID <= 0 {
		errs = append(errs, errors.New("telegram.allowedUserId must be positive"))
	}
	if strings.TrimSpace(c.Pi.Binary) == "" {
		errs = append(errs, errors.New("pi.binary is required"))
	}
	if !validTrust(c.Pi.DefaultTrust) {
		errs = append(errs, fmt.Errorf("pi.defaultTrust must be %q or %q", TrustNoApprove, TrustApprove))
	}
	if c.Pi.DefaultStreamingBehavior != StreamingFollowUp && c.Pi.DefaultStreamingBehavior != StreamingSteer {
		errs = append(errs, fmt.Errorf("pi.defaultStreamingBehavior must be %q or %q", StreamingFollowUp, StreamingSteer))
	}
	if c.Pi.SessionDir == "" {
		errs = append(errs, errors.New("pi.sessionDir is required"))
	}
	if c.State.Dir == "" {
		errs = append(errs, errors.New("state.dir is required"))
	}
	if c.Folders.MaxDepth < 1 {
		errs = append(errs, errors.New("folders.maxDepth must be at least 1"))
	}
	if c.Folders.MaxEntries < 1 {
		errs = append(errs, errors.New("folders.maxEntries must be at least 1"))
	}
	if len(c.Folders.Roots) == 0 {
		errs = append(errs, errors.New("at least one folders.root is required"))
	}
	for i, root := range c.Folders.Roots {
		if strings.TrimSpace(root.Path) == "" {
			errs = append(errs, fmt.Errorf("folders.roots[%d].path is required", i))
		}
		if !filepath.IsAbs(root.Path) {
			errs = append(errs, fmt.Errorf("folders.roots[%d].path must resolve to an absolute path", i))
		}
		if !validTrust(root.Trust) {
			errs = append(errs, fmt.Errorf("folders.roots[%d].trust must be %q or %q", i, TrustNoApprove, TrustApprove))
		}
	}
	return errors.Join(errs...)
}

func validTrust(value string) bool {
	return value == TrustNoApprove || value == TrustApprove
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	path = os.ExpandEnv(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}
