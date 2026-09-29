package config

import (
	"os"
	"strings"
	"testing"
)

func TestTheExampleIsSound(t *testing.T) {
	c, err := Load("../../example/hangar.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tiers) != 2 || !c.Tiers[1].Limits["toy.kind"].IsChoice || c.Tiers[1].Limits["toy.boxes"].Max != 2 {
		t.Fatalf("%+v", c.Tiers)
	}
	if !c.Tiers[0].Limits["*"].Unlimited {
		t.Fatal(`"*": unlimited`)
	}
}

const base = `
tiers:
  - name: users
    groups: [g]
    zones: [z]
zones:
  - name: z
    driver: fake
plugins:
  - name: toy
    builtin: toy
    zones: [z]
`

func TestRefusals(t *testing.T) {
	cases := map[string]string{
		"a typo in a key":                 base + "listn: :80\n",
		"both builtin and path":           strings.Replace(base, "builtin: toy", "builtin: toy\n    path: /bin/x", 1),
		"a credential where not enabled":  base + "    credentials:\n      other: {env: X}\n",
		"a tier in an undeclared zone":    strings.Replace(base, "zones: [z]\nzones", "zones: [elsewhere]\nzones", 1),
		"a limit spelled neither way":     strings.Replace(base, "zones: [z]\nzones", "zones: [z]\n    limits: {toy.boxes: lots}\nzones", 1),
		"a credential with file and env":  base + "    credentials:\n      z: {env: X, file: /y}\n",
		"sha256 on a builtin":             strings.Replace(base, "builtin: toy", "builtin: toy\n    sha256: ab", 1),
		"no tier":                         "zones: []\n",
		"an issuer without its audience":  base + "identity:\n  oidc:\n    issuer: https://id.example.com/\n",
		"a reconcile loop faster than 5s": base + "reconcile:\n  every: 1s\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Parse([]byte(base)); err != nil {
		t.Fatalf("the base is sound: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(base + "identity:\n  oidc:\n    issuer: https://id.example.com/\n    audience: hangar\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DataDir != "/data" || c.Identity.OIDC.GroupsClaim != "groups" || c.Reconcile.Every.Seconds() != 60 {
		t.Fatalf("%+v", c)
	}
}

func TestSecretsAreReadNotWritten(t *testing.T) {
	f := t.TempDir() + "/cred"
	if err := os.WriteFile(f, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := (Secret{File: f}).Read(); err != nil || string(v) != "s3cret" {
		t.Fatalf("%q %v", v, err)
	}
	t.Setenv("HANGAR_TEST_SECRET", "x")
	if v, _ := (Secret{Env: "HANGAR_TEST_SECRET"}).Read(); string(v) != "x" {
		t.Fatal("env")
	}
	if _, err := (Secret{Env: "HANGAR_TEST_UNSET"}).Read(); err == nil {
		t.Fatal("an unset variable is an error, not an empty secret")
	}
}
