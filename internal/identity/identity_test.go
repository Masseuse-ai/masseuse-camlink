package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadCreatesAndReloads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	id, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(id.PublicKey()) != ed25519.PublicKeySize {
		t.Fatal("no key")
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(dir, keyFile))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("key mode %o, want 0600", st.Mode().Perm())
		}
		dst, _ := os.Stat(dir)
		if dst.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode %o, want 0700", dst.Mode().Perm())
		}
	}
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.PublicKeyString() != id.PublicKeyString() {
		t.Fatal("reload produced a different key")
	}
	msg := []byte("hello")
	if !ed25519.Verify(again.PublicKey(), msg, id.Sign(msg)) {
		t.Fatal("signature from first load does not verify with the reloaded key")
	}
	pub, err := ParsePublicKey(id.PublicKeyString())
	if err != nil || !pub.Equal(id.PublicKey()) {
		t.Fatalf("ParsePublicKey round trip: %v", err)
	}
}

func TestPairedPersists(t *testing.T) {
	dir := t.TempDir()
	id, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	h1 := hex.EncodeToString(sha256.New().Sum(nil))
	h2 := strings.Repeat("ab", 32)
	if err := id.AddPaired(strings.ToUpper(h2)); err != nil {
		t.Fatal(err)
	}
	if err := id.AddPaired(h1); err != nil {
		t.Fatal(err)
	}
	if err := id.AddPaired(h1); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if err := id.AddPaired("zz"); err == nil {
		t.Fatal("accepted a bad hash")
	}
	if err := id.AddPaired(strings.Repeat("zz", 32)); err == nil {
		t.Fatal("accepted non-hex")
	}
	got := id.PairedHashes()
	if len(got) != 2 || got[0] != h2 || got[1] != h1 {
		// sorted: "ab.." < "e3.." (sha256 of empty)
		t.Fatalf("paired %v", got)
	}
	again, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(again.PairedHashes(), ",") != strings.Join(got, ",") {
		t.Fatalf("reloaded %v, want %v", again.PairedHashes(), got)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(filepath.Join(dir, pairedFile))
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("paired.json mode %o", st.Mode().Perm())
		}
	}
}

func TestPairedLimit(t *testing.T) {
	id, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxPaired; i++ {
		h := sha256.Sum256([]byte{byte(i)})
		if err := id.AddPaired(hex.EncodeToString(h[:])); err != nil {
			t.Fatal(err)
		}
	}
	h := sha256.Sum256([]byte("one too many"))
	if err := id.AddPaired(hex.EncodeToString(h[:])); err == nil {
		t.Fatal("exceeded MaxPaired")
	}
}

func TestCorruptFilesAreRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, keyFile), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("loaded a corrupt key")
	}
	dir = t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pairedFile), []byte(`["short"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("loaded a corrupt pairing list")
	}
}

func TestMessages(t *testing.T) {
	got := string(HelloMessage(1757400000, "KEY", []string{"aa", "bb"}))
	if got != "camlink-hello-v1|1757400000|KEY|aa,bb" {
		t.Fatalf("hello: %q", got)
	}
	if got := string(HelloMessage(1, "KEY", nil)); got != "camlink-hello-v1|1|KEY|" {
		t.Fatalf("hello without pairings: %q", got)
	}
	proof := string(TunnelProof("slot-3.tee.masseuse.ai", "abcd", []byte{0xff, 0xfe}))
	if proof != "camlink-tunnel-v1|slot-3.tee.masseuse.ai|abcd|__4" {
		t.Fatalf("proof: %q", proof)
	}
	if _, err := ParsePublicKey("dG9vc2hvcnQ"); err == nil {
		t.Fatal("accepted a short key")
	}
}
