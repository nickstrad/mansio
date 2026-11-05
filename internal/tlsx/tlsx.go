// Package tlsx loads the TLS material the services share: a server certificate (agentd's query listener, the
// edge) and a private CA bundle (the edge → agent hop, frontbench). An adapter: it reads files; callers wrap
// its errors in their own module (errs.Wrap), so it has no error module of its own.
package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ServerConfig loads a PEM certificate and key: TLS 1.2 or later, h2 and http/1.1 by ALPN.
func ServerConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}, nil
}

// LoadPool reads a PEM CA bundle into a pool holding only those CAs.
func LoadPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", caFile)
	}
	return pool, nil
}
