// Package config parses peipkg-config.toml — the operator's environment
// description: where recipes live, where state goes, signing key,
// upload backend, listener address. Distinct from the recipe roster:
// recipes describe what to build; this file describes how the farm
// should run.
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the parsed peipkg-config.toml.
type Config struct {
	Manager Manager `toml:"manager"`
	Repo    Repo    `toml:"repo"`
	Signing Signing `toml:"signing"`
	Upload  Upload  `toml:"upload"`
	HTTP    HTTP    `toml:"http"`
	Poll    Poll    `toml:"poll"`
}

// Manager carries identity and on-disk paths.
type Manager struct {
	// ID is the farm identifier recorded in every package's
	// manifest.build.farm_id (PSD-009 §3.3.4).
	ID string `toml:"id"`

	// RecipesDir is scanned at startup; every subdirectory containing
	// a peipkg.toml file is loaded as a recipe.
	RecipesDir string `toml:"recipes_dir"`

	// StateDir is the manager's writable working area. The manager
	// creates and owns subdirectories sources/, stage/, repo/.
	StateDir string `toml:"state_dir"`
}

// Repo carries the descriptor identity used at first-run init.
type Repo struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
}

// Signing locates the producer's Ed25519 private key.
type Signing struct {
	KeyFile string `toml:"key_file"`
}

// Upload describes where the published repo state is synced to. An
// empty Backend disables uploading (useful for local-only testing).
type Upload struct {
	Backend string `toml:"backend"` // "rclone", "none"
	Remote  string `toml:"remote"`  // backend-specific destination
}

// HTTP configures the optional webhook receiver. Empty Addr disables
// the HTTP server entirely; polling is then the only watch mechanism.
type HTTP struct {
	Addr              string `toml:"addr"`
	WebhookSecretFile string `toml:"webhook_secret_file"`
}

// Poll sets the default per-package poll cadence. Recipes may override
// this with their own [watch].poll_interval; this value is the floor.
type Poll struct {
	DefaultInterval Duration `toml:"default_interval"`
}

// Duration wraps time.Duration with TOML decoding from human-readable
// strings ("1h", "30m", "5m"). The bare time.Duration type does not
// implement TextUnmarshaler in a way TOML libraries pick up uniformly.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	parsed, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", string(b), err)
	}
	d.Duration = parsed
	return nil
}

// Load reads and validates a peipkg-config.toml file.
func Load(path string) (Config, error) {
	var cfg Config

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}

	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if extras := md.Undecoded(); len(extras) > 0 {
		return cfg, fmt.Errorf("parse %s: unknown keys %v (typo?)", path, extras)
	}

	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("validate %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks structural invariants. It does not check filesystem
// existence — the manager itself creates state directories on demand
// and signing keys are loaded lazily.
func (c Config) Validate() error {
	switch {
	case c.Manager.ID == "":
		return fmt.Errorf("[manager].id is required")
	case c.Manager.RecipesDir == "":
		return fmt.Errorf("[manager].recipes_dir is required")
	case c.Manager.StateDir == "":
		return fmt.Errorf("[manager].state_dir is required")
	case c.Repo.Name == "":
		return fmt.Errorf("[repo].name is required")
	case c.Signing.KeyFile == "":
		return fmt.Errorf("[signing].key_file is required")
	}

	switch c.Upload.Backend {
	case "", "none", "rclone":
		// ok
	default:
		return fmt.Errorf("[upload].backend %q not recognised (want one of: rclone, none, or empty)", c.Upload.Backend)
	}
	if c.Upload.Backend == "rclone" && c.Upload.Remote == "" {
		return fmt.Errorf("[upload].remote is required when backend is rclone")
	}

	if c.Poll.DefaultInterval.Duration < time.Minute {
		return fmt.Errorf("[poll].default_interval %v is less than the 1-minute minimum (use 1m or greater)", c.Poll.DefaultInterval.Duration)
	}

	return nil
}
