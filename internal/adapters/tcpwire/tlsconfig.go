package tcpwire

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/wesbragagt/syncthis/internal/app/ports"
	"github.com/wesbragagt/syncthis/internal/domain/device"
)

// ALPN is the protocol name negotiated during the TLS handshake. A peer
// speaking another version fails the handshake instead of the Hello exchange.
const ALPN = "syncthis/1"

// ServerTLS is the configuration the listener presents. Certificates are
// self-signed, so chain verification is skipped: the device ID derived from
// the leaf public key is the identity, and the handshake checks it.
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{ALPN},
		// The client certificate is not signed by a CA; it is identified
		// by its public key, which the handshake compares with the Hello.
		InsecureSkipVerify: true,
	}
}

// ClientTLS is the configuration the dialer uses. When expect is not empty the
// handshake fails with ErrDeviceMismatch unless the peer's leaf certificate
// derives exactly that device ID.
func ClientTLS(cert tls.Certificate, expect device.ID) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if expect == "" {
				return nil
			}
			id, err := idFromRaw(rawCerts)
			if err != nil {
				return err
			}
			if id != expect {
				return fmt.Errorf("%w: peer is %s, want %s", ports.ErrDeviceMismatch, id, expect)
			}
			return nil
		},
	}
}

// PeerIDFromConn derives the peer's device ID from the leaf certificate it
// presented. The connection must have completed its handshake.
func PeerIDFromConn(c *tls.Conn) (device.ID, error) {
	certs := c.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("peer presented no certificate")
	}
	return device.IDFromPublicKey(certs[0].RawSubjectPublicKeyInfo), nil
}

// idFromRaw derives a device ID from the DER certificates of a handshake.
func idFromRaw(rawCerts [][]byte) (device.ID, error) {
	if len(rawCerts) == 0 {
		return "", errors.New("peer presented no certificate")
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "", fmt.Errorf("parse peer certificate: %w", err)
	}
	return device.IDFromPublicKey(leaf.RawSubjectPublicKeyInfo), nil
}
