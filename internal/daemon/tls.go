package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/itskills/kubedrill/internal/brand"
)

// The simulator serves HTTPS with a locally generated CA because kubectl
// (client-go) does not send bearer tokens over plain HTTP. The CA is only
// trusted through the simulator's kubeconfig entries, never system-wide.

func (h Home) caCertFile() string     { return filepath.Join(h.Dir, "pki", "ca.crt") }
func (h Home) caKeyFile() string      { return filepath.Join(h.Dir, "pki", "ca.key") }
func (h Home) serverCertFile() string { return filepath.Join(h.Dir, "pki", "server.crt") }
func (h Home) serverKeyFile() string  { return filepath.Join(h.Dir, "pki", "server.key") }

// CACert returns the PEM encoded CA certificate.
func (h Home) CACert() ([]byte, error) { return os.ReadFile(h.caCertFile()) }

// EnsureTLS returns a server certificate, creating the CA and certificate
// if they are missing or about to expire.
func (h Home) EnsureTLS() (tls.Certificate, error) {
	if cert, err := tls.LoadX509KeyPair(h.serverCertFile(), h.serverKeyFile()); err == nil {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil && time.Until(leaf.NotAfter) > 30*24*time.Hour {
			if _, err := h.CACert(); err == nil {
				return cert, nil
			}
		}
	}
	caCert, caKey, err := h.loadOrCreateCA()
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: brand.Binary + "-apiserver", Organization: []string{brand.Name + " (local simulator)"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "kubernetes", "kubernetes.default", "kubernetes.default.svc"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(h.serverCertFile(), "CERTIFICATE", der, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := writePEM(h.serverKeyFile(), "EC PRIVATE KEY", kb, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	return tls.LoadX509KeyPair(h.serverCertFile(), h.serverKeyFile())
}

func (h Home) loadOrCreateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err1 := os.ReadFile(h.caCertFile())
	keyPEM, err2 := os.ReadFile(h.caKeyFile())
	if err1 == nil && err2 == nil {
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb != nil && kb != nil {
			cert, err := x509.ParseCertificate(cb.Bytes)
			key, err2 := x509.ParseECPrivateKey(kb.Bytes)
			if err == nil && err2 == nil && time.Until(cert.NotAfter) > 60*24*time.Hour {
				return cert, key, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: brand.Binary + "-ca", Organization: []string{brand.Name + " (local simulator)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(h.caKeyFile(), "EC PRIVATE KEY", kb, 0o600); err != nil {
		return nil, nil, err
	}
	if err := writePEM(h.caCertFile(), "CERTIFICATE", der, 0o644); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, key, err
}

func randomSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}

func writePEM(path, typ string, der []byte, perm os.FileMode) error {
	return WriteFileAtomic(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), perm)
}

// HTTPClient returns a client that trusts only the simulator CA.
func (h Home) HTTPClient(timeout time.Duration) (*httpClient, error) {
	ca, err := h.CACert()
	if err != nil {
		return nil, fmt.Errorf("simulator CA missing: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid simulator CA certificate")
	}
	return newHTTPClient(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, timeout), nil
}

type httpClient = http.Client

func newHTTPClient(cfg *tls.Config, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: cfg, Proxy: nil}}
}
