// Package tlsx loads the TLS material the services share: a server certificate (agentd's query listener, the
// edge, controld's API) and a private CA bundle (the edge → agent hop, clients of controld, frontbench). An adapter: it reads files; callers wrap
// its errors in their own module (errs.Wrap), so it has no error module of its own.
package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
)

// Files is a server certificate and its key (PEM): the "tls" section of agentd's, the edge's and controld's
// configs.
type Files struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// HTTPClient is an HTTP client trusting only caFile's CAs, or the system roots when caFile is "". Callers bound
// each request with its ctx.
func HTTPClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{}, nil
	}
	cfg, err := ClientConfig(caFile)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr}, nil
}

// ServerConfig loads a PEM certificate and key: TLS 1.2 or later, h2 and http/1.1 by ALPN.
func ServerConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}, nil
}

// ClientConfig trusts only caFile's CAs: TLS 1.2 or later (the edge → agent hop, clients of a private CA).
func ClientConfig(caFile string) (*tls.Config, error) {
	pool, err := LoadPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
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

// Loopback reports whether a listen address (host:port) binds a loopback interface only. A listener that does
// not needs TLS, and an admin listener must be loopback: binding is its only access control.
func Loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
