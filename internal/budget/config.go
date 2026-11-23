package budget

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// ValidateCapUSD is the single source of truth for the cap_usd > 0 rule: a
// non-positive cap makes every handler deny (ReserveWindowed compares against
// it) and Load refuses it, so a writer must never persist one. Load enforces it
// on read, `fuse init` on write, and Save on every other write path.
func ValidateCapUSD(v float64) error {
	if v <= 0 {
		return fmt.Errorf("cap_usd must be > 0 (got %.2f)", v)
	}
	return nil
}

// Providers is the closed set of provider families the launcher can route (see
// the switch in internal/cli/run.go). Config.Provider must name one of them.
var Providers = []string{"anthropic", "openai", "gemini", "deepseek", "openai_compat"}

// ValidateProvider is the single source of truth for the provider rule. The
// launcher routes on this value with no fallback, so an unrecognised name leaves
// the child talking to its own default endpoint — around the proxy, the ledger
// and the cap. Load enforces it so a typo fails closed instead of silently
// disabling enforcement.
func ValidateProvider(v string) error {
	for _, p := range Providers {
		if v == p {
			return nil
		}
	}
	return fmt.Errorf("provider must be one of %s (got %q)",
		strings.Join(Providers, ", "), v)
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
	if err := ValidateProvider(cfg.Provider); err != nil {
		return nil, err
	}
	if err := ValidateCapUSD(cfg.CapUSD); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Save writes the config atomically: write to .tmp then rename. It refuses a
// cap Load would reject, so no writer can strand a project with a .fuse.toml
// that cannot be loaded (`fuse cap` used to round a positive cap to $0.00 and
// persist it; init refuses to overwrite, so the file was unrecoverable in-CLI).
func Save(path string, cfg *Config) error {
	if err := ValidateCapUSD(cfg.CapUSD); err != nil {
		return err
	}
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
