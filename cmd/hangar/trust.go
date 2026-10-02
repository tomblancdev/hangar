package main

import (
	"crypto/x509"
	"fmt"
	"strings"

	"github.com/tomblancdev/hangar/internal/config"
)

// untrusted lists what the config reaches over https that this system could
// never verify. With no certificate authority at all — a bare image, a
// container nobody gave a bundle — every certificate is "signed by unknown
// authority", and it shows only at the first sign-in, or the first wake. The
// product's image carries a bundle; SSL_CERT_FILE and SSL_CERT_DIR name
// another. (A zone's engine is its driver's own: a ca_file, a fingerprint.)
func untrusted(cfg *config.Config) []string {
	pool, err := x509.SystemCertPool()
	if err == nil && !pool.Equal(x509.NewCertPool()) {
		return nil
	}
	var out []string
	if o := cfg.Identity.OIDC; o != nil && strings.HasPrefix(o.Issuer, "https://") {
		out = append(out, "the identity provider ("+o.Issuer+")")
	}
	for _, z := range cfg.Zones {
		if z.Wake != nil && strings.HasPrefix(z.Wake.URL, "https://") {
			out = append(out, fmt.Sprintf("zone %s's wake (%s)", z.Name, z.Wake.URL))
		}
	}
	return out
}

// noTrust is check's refusal for what untrusted lists.
func noTrust(what []string) error {
	return fmt.Errorf("config: this system trusts no certificate authority (no CA bundle), and %s is reached over https: "+
		"its certificate could never be verified — give the brain a bundle (the product's image carries one; SSL_CERT_FILE names another)",
		strings.Join(what, ", "))
}
