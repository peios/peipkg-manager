package config

import (
	"strings"
	"testing"
)

const baseConfigBody = `
[manager]
id = "f"
recipes_dir = "/r"
state_dir = "/s"
[repo]
name = "n"
[signing]
key_file = "/k"
[upload]
backend = "none"
[poll]
default_interval = "1h"
`

func TestLoadSigningKeysAndGrants(t *testing.T) {
	path := writeConfig(t, baseConfigBody+`
[[signing_key]]
name = "tcb"
private = "/etc/peipkg-manager/keys/tcb.ed25519"
pubkey_env = "PKM_KACS_TCB_PUBKEY_HEX"

[[grant]]
recipe = "kernel"
inject_pubkey = ["tcb"]

[[grant]]
recipe = "loregd"
sign = ["tcb"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SigningKeys) != 1 || cfg.SigningKeys[0].Name != "tcb" {
		t.Fatalf("signing keys = %+v", cfg.SigningKeys)
	}
	if len(cfg.Grants) != 2 {
		t.Fatalf("grants = %+v", cfg.Grants)
	}
}

func TestLoadRejectsBadGrants(t *testing.T) {
	cases := []struct{ name, extra, want string }{
		{
			"unknown inject key",
			"[[grant]]\nrecipe=\"kernel\"\ninject_pubkey=[\"nope\"]\n",
			"unknown signing key",
		},
		{
			"unknown sign key",
			"[[grant]]\nrecipe=\"loregd\"\nsign=[\"nope\"]\n",
			"unknown signing key",
		},
		{
			"inject without pubkey_env",
			"[[signing_key]]\nname=\"k\"\nprivate=\"/p\"\n[[grant]]\nrecipe=\"kernel\"\ninject_pubkey=[\"k\"]\n",
			"no pubkey_env",
		},
		{
			"duplicate key name",
			"[[signing_key]]\nname=\"d\"\nprivate=\"/a\"\n[[signing_key]]\nname=\"d\"\nprivate=\"/b\"\n",
			"duplicate name",
		},
		{
			"signing key missing private",
			"[[signing_key]]\nname=\"x\"\n",
			"private is required",
		},
		{
			"duplicate grant recipe",
			"[[grant]]\nrecipe=\"kernel\"\n[[grant]]\nrecipe=\"kernel\"\n",
			"duplicate recipe",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, baseConfigBody+"\n"+c.extra))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want containing %q", err, c.want)
			}
		})
	}
}
