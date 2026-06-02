package manager

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestPubkeyHexFromRawSeed(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "k.seed")
	if err := os.WriteFile(p, priv.Seed(), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pubkeyHexFromKeyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(pub); got != want {
		t.Errorf("hex = %s, want %s", got, want)
	}
}

// TestPubkeyHexFromPKCS8PEM covers the form the farm TCB key is actually
// stored in (openssl genpkey -algorithm ed25519).
func TestPubkeyHexFromPKCS8PEM(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	p := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := pubkeyHexFromKeyFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := hex.EncodeToString(pub); got != want {
		t.Errorf("hex = %s, want %s", got, want)
	}
}

func TestPubkeyHexRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(p, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pubkeyHexFromKeyFile(p); err == nil {
		t.Fatal("expected error for non-key file")
	}
}
