package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certValidity is how long a generated certificate lasts. The enclave pins
// its fingerprint for one session at a time, so a renewal costs nothing
// more than the next session probing again.
const certValidity = 10 * 365 * 24 * time.Hour

// LoadOrCreateCertificate returns the stream's TLS certificate, generating
// and saving one (ECDSA P-256, self-signed, mode 0600) when the state
// directory has none or the saved one no longer parses or has expired. The
// fingerprint therefore stays the same across restarts: the enclave pins it
// when a session starts, and a connector restarted in the middle of one
// still matches.
func LoadOrCreateCertificate(stateDir string) (tls.Certificate, error) {
	path := filepath.Join(stateDir, CertFile)
	if b, err := os.ReadFile(path); err == nil {
		cert, err := parseCertificate(b)
		if err == nil {
			return cert, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, fmt.Errorf("serve: read %s: %w", path, err)
	}
	cert, pemBytes, err := generateCertificate()
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("serve: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pemBytes, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("serve: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return tls.Certificate{}, fmt.Errorf("serve: write %s: %w", path, err)
	}
	return cert, nil
}

func generateCertificate() (tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("serve: key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("serve: serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "masseuse-camlink camera"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("serve: certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("serve: key: %w", err)
	}
	pemBytes := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...,
	)
	cert, err := parseCertificate(pemBytes)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return cert, pemBytes, nil
}

func parseCertificate(pemBytes []byte) (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serve: certificate file: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("serve: certificate file: %w", err)
	}
	if time.Now().After(leaf.NotAfter.Add(-24 * time.Hour)) {
		return tls.Certificate{}, errors.New("serve: certificate expired")
	}
	cert.Leaf = leaf
	return cert, nil
}

// Fingerprint is the SHA-256 of a certificate's DER encoding, lowercase
// hex: what the enclave pins after probing the stream through the tunnel.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
