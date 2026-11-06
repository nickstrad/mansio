// Package edgetest holds test helpers for the edge and its TLS: a private certificate authority that issues
// server certificates, as an operator's private CA would for the edge and the agents.
package edgetest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a throwaway private certificate authority.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	PEM  []byte // the CA certificate, PEM: what clients trust
}

// NewCA makes a CA valid for a day.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ts-db test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{cert: cert, key: key, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// Pool is a cert pool holding only this CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// Issue makes a server certificate for hosts (DNS names or IPs), PEM encoded.
func (ca *CA) Issue(t testing.TB, hosts ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: hosts[0]},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd})
}

// TLS is a server certificate for hosts, ready for tls.Config.Certificates.
func (ca *CA) TLS(t testing.TB, hosts ...string) tls.Certificate {
	t.Helper()
	c, k := ca.Issue(t, hosts...)
	cert, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// WriteFiles writes the CA (dir/ca.pem) and a server certificate for hosts (dir/<name>.crt, dir/<name>.key,
// mode 0600) and returns the three paths.
func (ca *CA) WriteFiles(t testing.TB, dir, name string, hosts ...string) (caFile, certFile, keyFile string) {
	t.Helper()
	c, k := ca.Issue(t, hosts...)
	caFile, certFile, keyFile = filepath.Join(dir, "ca.pem"), filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	for path, b := range map[string][]byte{caFile: ca.PEM, certFile: c, keyFile: k} {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return caFile, certFile, keyFile
}
