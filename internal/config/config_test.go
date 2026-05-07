package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "peipkg-config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMinimal(t *testing.T) {
	path := writeConfig(t, `
[manager]
id = "peios-build-1"
recipes_dir = "/etc/peipkg-manager/recipes"
state_dir = "/var/lib/peipkg-manager"

[repo]
name = "peios-official"
description = "test"

[signing]
key_file = "/etc/peipkg-manager/farm.ed25519"

[upload]
backend = "rclone"
remote = "r2:pkgs.peios.org"

[poll]
default_interval = "1h"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Manager.ID != "peios-build-1" {
		t.Errorf("Manager.ID = %q", cfg.Manager.ID)
	}
	if cfg.Poll.DefaultInterval.Duration != time.Hour {
		t.Errorf("Poll.DefaultInterval = %v, want 1h", cfg.Poll.DefaultInterval.Duration)
	}
	if cfg.Upload.Backend != "rclone" {
		t.Errorf("Upload.Backend = %q", cfg.Upload.Backend)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{
			name:    "no manager.id",
			body:    `[manager]` + "\n" + `recipes_dir="/r"` + "\n" + `state_dir="/s"` + "\n[repo]\nname=\"x\"\n[signing]\nkey_file=\"/k\"\n[poll]\ndefault_interval=\"1h\"\n",
			wantSub: "[manager].id",
		},
		{
			name:    "no signing.key_file",
			body:    `[manager]` + "\n" + `id="i"` + "\n" + `recipes_dir="/r"` + "\n" + `state_dir="/s"` + "\n[repo]\nname=\"x\"\n[poll]\ndefault_interval=\"1h\"\n",
			wantSub: "[signing].key_file",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeConfig(t, c.body)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load accepted invalid config")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q did not mention %q", err.Error(), c.wantSub)
			}
		})
	}
}

func TestLoadRejectsTinyPollInterval(t *testing.T) {
	path := writeConfig(t, `
[manager]
id = "i"
recipes_dir = "/r"
state_dir = "/s"
[repo]
name = "x"
[signing]
key_file = "/k"
[poll]
default_interval = "10s"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "1-minute minimum") {
		t.Errorf("expected 1-minute minimum rejection, got %v", err)
	}
}

func TestLoadRejectsUnknownTopLevelKey(t *testing.T) {
	path := writeConfig(t, `
[manager]
id = "i"
recipes_dir = "/r"
state_dir = "/s"
[repo]
name = "x"
[signing]
key_file = "/k"
[poll]
default_interval = "1h"
[bogus]
field = 1
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("expected unknown-key rejection, got %v", err)
	}
}

func TestLoadRejectsRcloneWithoutRemote(t *testing.T) {
	path := writeConfig(t, `
[manager]
id = "i"
recipes_dir = "/r"
state_dir = "/s"
[repo]
name = "x"
[signing]
key_file = "/k"
[upload]
backend = "rclone"
[poll]
default_interval = "1h"
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "remote") {
		t.Errorf("expected remote-required rejection, got %v", err)
	}
}

func TestDurationUnmarshalsHumanReadable(t *testing.T) {
	var d Duration
	if err := d.UnmarshalText([]byte("90m")); err != nil {
		t.Fatal(err)
	}
	if d.Duration != 90*time.Minute {
		t.Errorf("got %v, want 1h30m", d.Duration)
	}
}
