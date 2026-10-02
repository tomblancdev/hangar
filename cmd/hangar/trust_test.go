package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A brain on a system that trusts no certificate authority — a bare image —
// could verify nobody, and it showed only at the first sign-in. check names
// what the config reaches over https before that: refused with no bundle,
// sound with one, and sound with none when nothing is reached over https.
// Run through the binary, the bundle named the way an operator names one.
func TestCheckNamesWhatItCouldNeverVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "hangar")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	config := func(name, issuer, wake string) string {
		t.Helper()
		p := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(p, fmt.Appendf(nil, `
data_dir: %s/data
identity:
  oidc: {issuer: %s, audience: hangar}
tiers:
  - name: users
    groups: [users]
    zones: [z]
    limits: {toy.boxes: 1, toy.cores: 2, toy.memory_gb: 2, toy.kind: [container]}
zones:
  - name: z
    driver: fake
    endpoint: %s/zone.json
    wake: {url: %s}
plugins:
  - {name: toy, builtin: toy, zones: [z]}
`, dir, issuer, dir, wake), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// one certificate authority, in a bundle of its own
	bundle := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(bundle, authority(t), 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(cfg, certFile string) (string, error) {
		cmd := exec.Command(bin, "check", "--config", cfg)
		// the system's own bundle and directories are never read: only the one named
		cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+certFile, "SSL_CERT_DIR="+filepath.Join(dir, "no-such-dir"))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String() + errb.String(), err
	}
	none := filepath.Join(dir, "no-such-bundle.pem")
	overTLS := config("tls", "https://id.example.com/o/hangar/", "https://power.example.com/wake")

	out, err := check(overTLS, none)
	if err == nil || strings.Contains(out, "sound:") {
		t.Fatalf("no certificate authority, an https provider, and check is sound:\n%s", out)
	}
	for _, want := range []string{"trusts no certificate authority", "the identity provider (https://id.example.com/o/hangar/)", "zone z's wake (https://power.example.com/wake)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
	// the control: the same config, a bundle named
	if out, err := check(overTLS, bundle); err != nil || !strings.Contains(out, "sound:") {
		t.Fatalf("with a bundle: %v\n%s", err, out)
	}
	// nothing reached over https needs no authority
	if out, err := check(config("plain", "http://id.example.com/o/hangar/", "http://power.example.com/wake"), none); err != nil || !strings.Contains(out, "sound:") {
		t.Fatalf("nothing over https, no bundle: %v\n%s", err, out)
	}
}

// authority is a certificate authority's certificate, in PEM.
func authority(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "a test authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
