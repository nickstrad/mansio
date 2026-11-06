package main

import (
	"crypto/tls"
	"net"

	"github.com/nickstrad/ts-db-go/internal/errs"
	"github.com/nickstrad/ts-db-go/internal/tlsx"
)

// TLSFiles is the query listener's server certificate and key (PEM). With it the listener speaks HTTPS, h2 and
// http/1.1 by ALPN (protocols then does not apply): the edge → agent hop (control-plane plan CP-7), where the
// edge checks the certificate against the operator's private CA (its agent_tls.ca_file).
type TLSFiles = tlsx.Files

// queryTLS loads the query listener's TLS config; nil without a tls section.
func (c *Config) queryTLS() (*tls.Config, error) {
	if c.TLS == nil {
		return nil, nil
	}
	t, err := tlsx.ServerConfig(c.TLS.CertFile, c.TLS.KeyFile)
	if err != nil {
		return nil, errs.Wrap(module, err, "cannot load the tls certificate").With("cert_file", c.TLS.CertFile)
	}
	return t, nil
}

// checkTLS refuses, without -dev, a query listener that clients off this host could reach in plain HTTP:
// outside a laptop, agents sit behind the edge and speak TLS to it (object-storage guide S2's rule, applied to
// the agent's own listener).
func (c *Config) checkTLS() error {
	if c.TLS == nil && c.Listen != "" && !loopback(c.Listen) && !c.Dev { // "" is refused by parseConfig
		return errs.Wrap(module, nil, "listen %s is not loopback and there is no tls section: plain HTTP off this host is for local "+
			"development only; add \"tls\": {\"cert_file\", \"key_file\"} (a private CA the edge trusts), or pass -dev", c.Listen)
	}
	return nil
}

// tlsListener wraps ln in TLS when cfg is set.
func tlsListener(ln net.Listener, cfg *tls.Config) net.Listener {
	if cfg == nil {
		return ln
	}
	return tls.NewListener(ln, cfg)
}
