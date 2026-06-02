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
	Manager     Manager      `toml:"manager"`
	Repo        Repo         `toml:"repo"`
	Signing     Signing      `toml:"signing"`
	Upload      Upload       `toml:"upload"`
	HTTP        HTTP         `toml:"http"`
	Poll        Poll         `toml:"poll"`
	SigningKeys []SigningKey `toml:"signing_key"`
	Grants      []Grant      `toml:"grant"`
}

// SigningKey is a binary-signing key the farm holds — the .peios.sig / PIP
// keys (e.g. the TCB key), distinct from [signing].key_file which signs the
// .peipkg package. Recipes reference it by Name via a [[grant]]; the key
// material stays here, off the (public) recipes repo.
type SigningKey struct {
	Name string `toml:"name"`
	// Private is the path to the Ed25519 private key (PEM or 32-byte raw
	// seed). Passed to peipkg-build for signing; never copied into a recipe.
	Private string `toml:"private"`
	// PubkeyEnv is the build-env variable name under which this key's public
	// hex is injected for inject_pubkey grants (e.g. PKM_KACS_TCB_PUBKEY_HEX).
	// Required only if the key is used in an inject_pubkey grant.
	PubkeyEnv string `toml:"pubkey_env"`
}

// Grant authorizes one recipe (by directory ID) to use named signing keys.
// InjectPubkey lists keys whose public hex is injected into the build env
// (the kernel's catalogue pubkey); Sign lists keys the recipe may embed
// .peios.sig signatures with (TCB daemons). Recipes are public, so this
// farm-side map — not the recipe — is the authorization boundary.
type Grant struct {
	Recipe       string   `toml:"recipe"`
	InjectPubkey []string `toml:"inject_pubkey"`
	Sign         []string `toml:"sign"`
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

	keys := make(map[string]SigningKey, len(c.SigningKeys))
	for i, k := range c.SigningKeys {
		if k.Name == "" {
			return fmt.Errorf("[[signing_key]] #%d: name is required", i)
		}
		if _, dup := keys[k.Name]; dup {
			return fmt.Errorf("[[signing_key]] %s: duplicate name", k.Name)
		}
		if k.Private == "" {
			return fmt.Errorf("[[signing_key]] %s: private is required", k.Name)
		}
		keys[k.Name] = k
	}

	grantRecipes := make(map[string]bool, len(c.Grants))
	for i, g := range c.Grants {
		if g.Recipe == "" {
			return fmt.Errorf("[[grant]] #%d: recipe is required", i)
		}
		if grantRecipes[g.Recipe] {
			return fmt.Errorf("[[grant]] %s: duplicate recipe", g.Recipe)
		}
		grantRecipes[g.Recipe] = true
		for _, name := range g.InjectPubkey {
			k, ok := keys[name]
			if !ok {
				return fmt.Errorf("[[grant]] %s: inject_pubkey references unknown signing key %q", g.Recipe, name)
			}
			if k.PubkeyEnv == "" {
				return fmt.Errorf("[[grant]] %s: inject_pubkey key %q has no pubkey_env", g.Recipe, name)
			}
		}
		for _, name := range g.Sign {
			if _, ok := keys[name]; !ok {
				return fmt.Errorf("[[grant]] %s: sign references unknown signing key %q", g.Recipe, name)
			}
		}
	}

	return nil
}
