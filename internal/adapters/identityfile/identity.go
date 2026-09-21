// Package identityfile stores this device's ed25519 key and self-signed
// certificate on disk and derives the device ID from them.
package identityfile

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

var _ ports.Identity = (*Identity)(nil)

// ErrNotInitialized means the data directory holds no identity yet.
var ErrNotInitialized = errors.New("device identity not initialized")

// File names inside the data directory.
const (
	KeyFile  = "identity.key"
	CertFile = "identity.crt"
)

// certLifetime is how long a generated certificate stays valid. Identities are
// long-lived; rotation would change the device ID.
const certLifetime = 100 * 365 * 24 * time.Hour

// Identity is this device's key pair, certificate and derived ID.
type Identity struct {
	id   device.ID
	name string
	cert tls.Certificate
}

// DeviceID returns the ID derived from the certificate public key.
func (i *Identity) DeviceID() device.ID { return i.id }

// Name returns the human-readable device name stored in the certificate.
func (i *Identity) Name() string { return i.name }

// TLSCertificate returns the certificate to present to peers.
func (i *Identity) TLSCertificate() tls.Certificate { return i.cert }

// LoadOrCreate loads the identity in dir, creating a new one under the given
// device name if none exists. The second result reports whether a new identity
// was generated.
func LoadOrCreate(dir, name string) (*Identity, bool, error) {
	id, err := Load(dir)
	switch {
	case err == nil:
		return id, false, nil
	case !errors.Is(err, ErrNotInitialized):
		return nil, false, err
	}
	id, err = create(dir, name)
	if err != nil {
		return nil, false, err
	}
	return id, true, nil
}

// Load reads the identity from dir. It returns ErrNotInitialized if either the
// key or the certificate is missing.
func Load(dir string) (*Identity, error) {
	keyPath := filepath.Join(dir, KeyFile)
	certPath := filepath.Join(dir, CertFile)
	for _, p := range []string{keyPath, certPath} {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %s missing", ErrNotInitialized, p)
			}
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load identity from %s: %w", dir, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", certPath, err)
	}
	cert.Leaf = leaf

	return &Identity{
		id:   device.IDFromPublicKey(leaf.RawSubjectPublicKeyInfo),
		name: nameOf(leaf),
		cert: cert,
	}, nil
}

// create generates a key pair and certificate and writes them to dir.
func create(dir, name string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "syncthis",
			Organization: []string{name},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(filepath.Join(dir, KeyFile), keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", KeyFile, err)
	}
	if err := os.WriteFile(filepath.Join(dir, CertFile), certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", CertFile, err)
	}

	return Load(dir)
}

// nameOf returns the device name carried by a certificate.
func nameOf(cert *x509.Certificate) string {
	if len(cert.Subject.Organization) == 0 {
		return ""
	}
	return cert.Subject.Organization[0]
}
