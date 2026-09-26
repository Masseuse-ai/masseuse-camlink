// Package identity is the connector's persistent Ed25519 identity and the
// list of phones it has been paired with (docs/PROTOCOL.md, section 1).
//
// The private key lives in <state dir>/identity.key with mode 0600; the
// paired phone token hashes live in <state dir>/paired.json. Deleting the
// directory revokes every pairing and gives the connector a new identity.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	keyFile    = "identity.key"
	pairedFile = "paired.json"
	// MaxPaired bounds the pairings a connector reports and stores.
	MaxPaired = 32
)

// Identity is a loaded connector identity.
type Identity struct {
	dir  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	mu     sync.Mutex
	paired map[string]struct{}
}

// Load reads the identity in dir, creating the directory (0700), the key
// (0600) and an empty pairing list on first use.
func Load(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	id := &Identity{dir: dir, paired: map[string]struct{}{}}
	keyPath := filepath.Join(dir, keyFile)
	raw, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		seed := base64.RawURLEncoding.EncodeToString(priv.Seed()) + "\n"
		if err := writeFileAtomic(keyPath, []byte(seed), 0o600); err != nil {
			return nil, fmt.Errorf("write identity key: %w", err)
		}
		id.priv = priv
	case err != nil:
		return nil, fmt.Errorf("read identity key: %w", err)
	default:
		seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("%s is not a base64url Ed25519 seed", keyPath)
		}
		id.priv = ed25519.NewKeyFromSeed(seed)
	}
	id.pub = id.priv.Public().(ed25519.PublicKey)

	pairedRaw, err := os.ReadFile(filepath.Join(dir, pairedFile))
	if err == nil {
		var list []string
		if err := json.Unmarshal(pairedRaw, &list); err != nil {
			return nil, fmt.Errorf("%s: %w", pairedFile, err)
		}
		for _, h := range list {
			if err := validHash(h); err != nil {
				return nil, fmt.Errorf("%s: %w", pairedFile, err)
			}
			id.paired[h] = struct{}{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", pairedFile, err)
	}
	return id, nil
}

// Dir is the state directory.
func (id *Identity) Dir() string { return id.dir }

// PublicKey is the raw Ed25519 public key.
func (id *Identity) PublicKey() ed25519.PublicKey { return id.pub }

// PublicKeyString is the connector's identity string: base64url of the raw
// public key, without padding.
func (id *Identity) PublicKeyString() string {
	return base64.RawURLEncoding.EncodeToString(id.pub)
}

// Sign signs msg with the identity key.
func (id *Identity) Sign(msg []byte) []byte { return ed25519.Sign(id.priv, msg) }

// PairedHashes returns the paired phone token hashes, sorted.
func (id *Identity) PairedHashes() []string {
	id.mu.Lock()
	defer id.mu.Unlock()
	out := make([]string, 0, len(id.paired))
	for h := range id.paired {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// AddPaired records a phone token hash (hex SHA-256) and persists the list.
func (id *Identity) AddPaired(hash string) error {
	hash = strings.ToLower(hash)
	if err := validHash(hash); err != nil {
		return err
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if _, ok := id.paired[hash]; ok {
		return nil
	}
	if len(id.paired) >= MaxPaired {
		return fmt.Errorf("already paired with %d phones", MaxPaired)
	}
	id.paired[hash] = struct{}{}
	return id.persistLocked()
}

func (id *Identity) persistLocked() error {
	list := make([]string, 0, len(id.paired))
	for h := range id.paired {
		list = append(list, h)
	}
	sort.Strings(list)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(id.dir, pairedFile), append(data, '\n'), 0o600)
}

func validHash(h string) error {
	if len(h) != 64 {
		return fmt.Errorf("phone token hash must be 64 hex characters, got %d", len(h))
	}
	if _, err := hex.DecodeString(h); err != nil {
		return fmt.Errorf("phone token hash is not hex")
	}
	if strings.ToLower(h) != h {
		return errors.New("phone token hash must be lowercase hex")
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// HelloMessage is the string the connector signs for POST /api/camlink/hello.
func HelloMessage(ts int64, key string, paired []string) []byte {
	return []byte("camlink-hello-v1|" + strconv.FormatInt(ts, 10) + "|" + key + "|" + strings.Join(paired, ","))
}

// TunnelProof is the string the connector signs to answer the gateway's
// CHALLENGE: host is the origin host it dialed, ticketHash the hex SHA-256
// of the ticket, nonce the challenge payload in base64url.
func TunnelProof(host, ticketHash string, nonce []byte) []byte {
	return []byte("camlink-tunnel-v1|" + host + "|" + ticketHash + "|" + base64.RawURLEncoding.EncodeToString(nonce))
}

// ParsePublicKey decodes a connector identity string.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("connector key is not a base64url 32-byte Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}
