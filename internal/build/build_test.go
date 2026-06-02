package build

import (
	"slices"
	"testing"
)

func TestBinarySigningArgsNoGrant(t *testing.T) {
	r := &Runner{}
	args, err := r.binarySigningArgs("kernel")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args != nil {
		t.Errorf("a recipe with no grant should get no args, got %v", args)
	}
}

func TestBinarySigningArgsInjectAndSign(t *testing.T) {
	r := &Runner{
		SigningKeys: map[string]SigningKey{
			"tcb": {PrivatePath: "/keys/tcb", PubkeyHex: "abcd", PubkeyEnv: "PKM_KACS_TCB_PUBKEY_HEX"},
		},
		Grants: map[string]Grant{
			"kernel": {InjectPubkey: []string{"tcb"}},
			"loregd": {Sign: []string{"tcb"}},
		},
	}
	got, err := r.binarySigningArgs("kernel")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"--build-env", "PKM_KACS_TCB_PUBKEY_HEX=abcd"}) {
		t.Errorf("kernel args = %v", got)
	}
	got, err = r.binarySigningArgs("loregd")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"--binary-sign-key", "tcb=/keys/tcb"}) {
		t.Errorf("loregd args = %v", got)
	}
}

func TestBinarySigningArgsUnknownKey(t *testing.T) {
	r := &Runner{Grants: map[string]Grant{"x": {Sign: []string{"missing"}}}}
	if _, err := r.binarySigningArgs("x"); err == nil {
		t.Fatal("expected error for grant referencing an unknown signing key")
	}
}

func TestBinarySigningArgsInjectMissingHex(t *testing.T) {
	r := &Runner{
		SigningKeys: map[string]SigningKey{"tcb": {PrivatePath: "/k"}}, // no PubkeyHex/Env
		Grants:      map[string]Grant{"kernel": {InjectPubkey: []string{"tcb"}}},
	}
	if _, err := r.binarySigningArgs("kernel"); err == nil {
		t.Fatal("expected error when an inject_pubkey key has no derived pubkey")
	}
}
