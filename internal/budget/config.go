package budget

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the on-disk shape of .fuse.toml.
//
// v0.2.0 adds Provider + UpstreamURL so a project can route through any of the
// five supported provider families. Provider defaults to "anthropic" for
// backwards compat with v0.1.0 configs that omit it.
type Config struct {
	CapUSD      float64 `toml:"cap_usd"`
	Window      string  `toml:"window"`
	Account     string  `toml:"account"`
	Provider    string  `toml:"provider"`
	UpstreamURL string  `toml:"upstream_url"`
}

const FileName = ".fuse.toml"

// FindProjectRoot walks up from start until it finds a .fuse.toml.
// Returns the directory containing the config, or "" if none found.
func FindProjectRoot(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if _, err := os.Stat(candidate); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Load reads the .fuse.toml at the given project root directory.
func Load(projectRoot string) (*Config, error) {
	path := filepath.Join(projectRoot, FileName)
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	if cfg.Window == "" {
		cfg.Window = "project"
	}
	if cfg.Window != "daily" && cfg.Window != "project" {
		return nil, fmt.Errorf(`window must be "daily" or "project" (got %q)`, cfg.Window)
	}
	if cfg.Provider == "" {
		cfg.Provider = "anthropic"
	}
	if cfg.CapUSD <= 0 {
		return nil, fmt.Errorf("cap_usd must be > 0 (got %.2f)", cfg.CapUSD)
	}
	return &cfg, nil
}

// Save writes the config atomically: write to .tmp then rename.
func Save(path string, cfg *Config) error {
	if cfg.Window == "" {
		cfg.Window = "project"
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// LoadOrInit loads cfg if present, otherwise returns ErrNoConfig.
var ErrNoConfig = errors.New("no .fuse.toml found walking up from cwd")

func LoadFromCwd() (string, *Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	root := FindProjectRoot(cwd)
	if root == "" {
		return "", nil, ErrNoConfig
	}
	cfg, err := Load(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil, ErrNoConfig
		}
		return "", nil, err
	}
	return root, cfg, nil
}
