package config

import (
	"os"
	"strings"
	"testing"
	"time"
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
		"a time zone that is none":        base + "time_zone: Mars/Olympus\n",
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

const scheduled = base + `schedules:
  - name: weekly
    cron: "0 3 * * sun"
    time_zone: Europe/Paris
    as: {subject: recipes, groups: [g]}
    create: {type: box, zone: z, spec: {kind: small}}
`

func TestSchedules(t *testing.T) {
	c, err := Parse([]byte(scheduled))
	if err != nil {
		t.Fatal(err)
	}
	sc, ok := c.Schedule("weekly")
	if !ok || sc.Keep != 2 || sc.Location.String() != "Europe/Paris" || sc.Create.Spec["kind"] != "small" {
		t.Fatalf("%+v", sc)
	}
	if n := sc.Line.Next(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), sc.Location); !n.Equal(time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("Sunday 03:00 in Paris is 01:00 UTC: %s", n)
	}
	utc, _ := Parse([]byte(strings.Replace(scheduled, "    time_zone: Europe/Paris\n", "", 1)))
	if s, _ := utc.Schedule("weekly"); s == nil || s.TimeZone != "UTC" {
		t.Fatal("UTC by default")
	}
	for name, doc := range map[string]string{
		"a cron that never comes":        strings.Replace(scheduled, "0 3 * * sun", "0 0 30 2 *", 1),
		"a cron of four fields":          strings.Replace(scheduled, "0 3 * * sun", "0 3 * *", 1),
		"a time zone nobody has":         strings.Replace(scheduled, "Europe/Paris", "Europe/Nowhere", 1),
		"no subject":                     strings.Replace(scheduled, "subject: recipes, ", "", 1),
		"groups in no tier":              strings.Replace(scheduled, "groups: [g]}", "groups: [nobody]}", 1),
		"a zone its tier is not open to": strings.Replace(strings.Replace(scheduled, "zone: z, spec", "zone: y, spec", 1), "  - name: z\n", "  - name: z\n    driver: fake\n  - name: y\n", 1),
		"an undeclared zone":             strings.Replace(scheduled, "zone: z, spec", "zone: nowhere, spec", 1),
		"no type":                        strings.Replace(scheduled, "type: box, ", "", 1),
		"a tag of the core's":            strings.Replace(scheduled, "spec: {kind: small}}", "spec: {kind: small}, tags: {hangar:schedule: x}}", 1),
		"keep below 1":                   scheduled + "    keep: -1\n",
		"two of one name":                scheduled + "  - name: weekly\n    cron: \"* * * * *\"\n    as: {subject: r, groups: [g]}\n    create: {type: box, zone: z}\n",
	} {
		if _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), "schedule") {
			t.Errorf("%s: want the schedule refused, got %v", name, err)
		}
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
	// the meters' months are counted in UTC unless the file names a zone
	if c.TimeZone != "UTC" || c.Location != time.UTC {
		t.Fatalf("the months' calendar: %q %v", c.TimeZone, c.Location)
	}
	if c, err = Parse([]byte(base + "time_zone: Europe/Paris\n")); err != nil || c.Location.String() != "Europe/Paris" {
		t.Fatalf("%v %v", c, err)
	}
	if _, ok := c.Tier("users"); !ok {
		t.Fatal("a tier is found by its name")
	}
	if _, ok := c.Tier("nobody"); ok {
		t.Fatal("a tier nobody declared was found")
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
