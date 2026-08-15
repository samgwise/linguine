package mesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// ServerTLSConfig loads a server certificate/key for a wss:// listener. The
// mesh is plaintext ws:// by default (fronted by a TLS-terminating reverse
// proxy); a wss:// listener with this config terminates TLS in-process.
func ServerTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mesh: load server keypair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// ClientTLSConfig builds a client TLS config for a wss:// dial. Verification
// mode is chosen by the arguments:
//   - caFile set: the router cert must chain to that CA (private-CA path).
//   - fingerprint set (format "sha256/base64"): the router leaf cert's DER
//     hash must match — SSH-style trust-on-first-use pinning. This disables
//     system/hostname verification and checks only the pin.
//   - both empty: system roots with normal hostname verification (the public-
//     CA path, e.g. a Let's Encrypt cert behind a reverse proxy).
//
// caFile and fingerprint are mutually exclusive in practice; if both are set
// the fingerprint pin takes over verification (InsecureSkipVerify + pin).
func ClientTLSConfig(caFile, serverName, fingerprint string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if serverName != "" {
		cfg.ServerName = serverName
	}
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("mesh: read ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("mesh: no certs in ca file")
		}
		cfg.RootCAs = pool
	}
	if fp := strings.TrimSpace(fingerprint); fp != "" {
		if err := validateFingerprint(fp); err != nil {
			return nil, err
		}
		// Pin the peer leaf cert and take over verification. Setting
		// InsecureSkipVerify skips the standard chain/hostname check; the
		// pin callback then enforces that the presented cert is exactly the
		// one the operator recorded.
		cfg.InsecureSkipVerify = true
		pin := fp
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("mesh: server presented no certificate")
			}
			got := leafFingerprint(rawCerts[0])
			if got != pin {
				return fmt.Errorf("mesh: certificate fingerprint mismatch (want %s, got %s)", pin, got)
			}
			return nil
		}
	}
	return cfg, nil
}

// LoadOrCreateSelfSignedCert loads a cert/key pair from the given paths, or
// generates a fresh self-signed ECDSA P-256 certificate (valid 10 years,
// SANs for localhost and the loopback IPs) and persists both files (mode
// 0600) when either file is missing. It returns the loaded/generated
// certificate, its leaf fingerprint (sha256/base64), and a flag reporting
// whether a new certificate was generated. The fingerprint is what a worker
// pins in router.tls_fingerprint for the no-reverse-proxy deployment.
func LoadOrCreateSelfSignedCert(certFile, keyFile string) (tls.Certificate, string, bool, error) {
	if certPEM, err := os.ReadFile(certFile); err == nil {
		keyPEM, kerr := os.ReadFile(keyFile)
		if kerr == nil {
			cert, cerr := tls.X509KeyPair(certPEM, keyPEM)
			if cerr != nil {
				return tls.Certificate{}, "", false, fmt.Errorf("mesh: load self-signed keypair: %w", cerr)
			}
			if len(cert.Certificate) == 0 {
				return tls.Certificate{}, "", false, errors.New("mesh: loaded keypair has no certificate")
			}
			return cert, leafFingerprint(cert.Certificate[0]), false, nil
		}
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: cert present at %s but key missing at %s", certFile, keyFile)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: generate serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "linguine-mesh"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"linguine-mesh", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: create self-signed cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: marshal key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: write cert: %w", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: write key: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", false, fmt.Errorf("mesh: load generated keypair: %w", err)
	}
	return cert, leafFingerprint(der), true, nil
}

// leafFingerprint returns the sha256/base64 fingerprint of a leaf cert's DER
// bytes, matching the format workers pin via router.tls_fingerprint.
func leafFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256/" + base64.StdEncoding.EncodeToString(sum[:])
}

func validateFingerprint(fp string) error {
	if !strings.HasPrefix(fp, "sha256/") {
		return fmt.Errorf("mesh: fingerprint must be sha256/base64, got %q", fp)
	}
	return nil
}
