package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A dev box made true from its spec file on a real Proxmox VE, by the
// binary's command line against the binary's brain: a key pair, a container
// with a floor that stays and a top-up that goes, two volumes plugged in at
// their paths — then found in sync, grown and backed up by the steps its
// plugins name, a field set at birth refused before anything moves, a volume
// that left the file unplugged and deleted once asked (the other keeping its
// file), and last the whole set let go of by an empty file. Without the
// bench's variables it skips:
//
//	sh tools/bench/bench.sh up && eval "$(sh tools/bench/bench.sh env)" && go test ./cmd/hangar -run BenchTheDevBox -v
func TestBenchTheDevBoxApplied(t *testing.T) {
	url, volTok := os.Getenv("HANGAR_BENCH_URL"), os.Getenv("HANGAR_BENCH_VOLUMES_TOKEN_FILE")
	if url == "" || volTok == "" {
		t.Skip("no bench (or one set up before the volumes token): sh tools/bench/bench.sh up, then eval its env")
	}
	benchSSH := func(cmd string) (string, error) {
		out, err := exec.Command("ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			"root@127.0.0.1", cmd).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(cmd string) string {
		t.Helper()
		out, err := benchSSH(cmd)
		if err != nil {
			t.Fatalf("on the bench: %s: %v %s", cmd, err, out)
		}
		return out
	}
	archive := must(`ls /var/lib/vz/template/cache/ | grep "^debian-13-standard_.*_$(dpkg --print-architecture)\." | sort -V | tail -1`)

	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "hangar")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	subject := fmt.Sprintf("bench-%d", time.Now().UnixNano()) // its own shelf, removed at the end
	cfg := filepath.Join(dir, "hangar.yaml")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `
data_dir: %[1]s/data
tiers:
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 1
      machines.vcpu_hours: 1000
      machines.vcpu: 2
      machines.memory_gb: 3
      machines.disk_gb: 4
      machines.key_pairs: 1
      machines.kind: [container]
      machines.class: [spot, guaranteed+spot]
      volumes.count: 2
      volumes.size_gb: 4
      volumes.backup_gb: 3
zones:
  - name: bench
    driver: proxmox
    endpoint: %[2]s
    options:
      node: pve-bench
      pool: hangar
      images_pool: hangar-images
      storage: local-zfs
      seed_storage: hangar-seeds
      bridge: hbnet
      vmids: 11100-11119
      ca_file: %[3]s
      shelf_archive: local:vztmpl/%[6]s
    room:                     # the bench's priority guest, VM 100: the machines' token reads its power
      memory_gb: 6
      reservations: [{name: priority, memory_gb: 3, while_running: "100"}]
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %[4]s}
    settings:
      images:
        debian-13: {vm: debian-13, container: "local:vztmpl/%[6]s"}
  - name: volumes
    builtin: volumes
    zones: [bench]
    credentials:
      bench: {file: %[5]s}
reconcile: {every: 10s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), os.Getenv("HANGAR_BENCH_TOKEN_FILE"), volTok, archive), 0o600); err != nil {
		t.Fatal(err)
	}
	host := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		cmd := exec.Command(bin, append(args, "--config", cfg)...)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("hangar %s: %v %s", args[0], err, errb.String())
		}
		return out.String()
	}
	host("check")
	secret := strings.TrimSpace(host("token", "create", "--subject", subject, "--groups", "users", "--name", "bench", "--ttl", "2h"))

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var logs bytes.Buffer
	serve := func() *exec.Cmd {
		srv := exec.Command(bin, "serve", "--config", cfg)
		srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
		srv.Stdout, srv.Stderr = &logs, &logs
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(20 * time.Second); ; {
			if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
				resp.Body.Close()
				if resp.StatusCode == 200 {
					return srv
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("never healthy\n%s", logs.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	srv := serve()
	exited := make(chan error, 1)
	go func() { exited <- srv.Wait() }()
	t.Cleanup(func() { _ = srv.Process.Kill() })

	// the command line: the same binary, signed in by an API token
	cli := func(args ...string) (string, string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "HANGAR_URL=http://"+addr, "HANGAR_TOKEN="+secret, "HANGAR_HOME="+filepath.Join(dir, "cli"))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		return out.String(), errb.String(), err
	}
	ok := func(args ...string) string {
		t.Helper()
		start := time.Now()
		out, errs, err := cli(args...)
		if err != nil {
			t.Fatalf("hangar %s: %v\n%s%s\n--- the brain:\n%s", strings.Join(args, " "), err, out, errs, logs.String())
		}
		t.Logf("hangar %s (%s):\n%s%s", strings.Join(args, " "), time.Since(start).Round(time.Second), out, errs)
		return errs
	}
	key, _ := os.ReadFile(os.Getenv("HANGAR_BENCH_SSH_KEY") + ".pub")
	file := filepath.Join(dir, "dev.yaml")
	write := func(memory int, cacheBackup, cache bool, kind string) {
		s := "set: dev\nzone: bench\nresources:\n"
		if cache {
			s += fmt.Sprintf("  cache:\n    type: volume\n    spec: {size_gb: 2, mount: /srv/cache, backup: %v, machine: box}\n", cacheBackup)
		}
		s += fmt.Sprintf(`  home:
    type: volume
    spec: {size_gb: 1, mount: /home/dev, backup: true, machine: box}
  box:
    type: machine
    spec: {name: dev, kind: %s, class: guaranteed+spot, cores: 2, memory_gb: %d, floor_gb: 1, cores_beside: 1, image: debian-13, disk_gb: 2, key_pairs: [me]}
  me:
    type: keypair
    spec: {public_key: %q}
`, kind, memory, strings.TrimSpace(string(key)))
		if err := os.WriteFile(file, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// what the brain holds of the set, by name
	set := func() map[string]map[string]any {
		t.Helper()
		out, _, err := cli("volume", "list", "-o", "json")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]map[string]any{}
		for _, list := range []string{out, func() string { o, _, _ := cli("machine", "list", "-o", "json"); return o }(), func() string { o, _, _ := cli("keypair", "list", "-o", "json"); return o }()} {
			var rs []map[string]any
			_ = json.Unmarshal([]byte(list), &rs)
			for _, r := range rs {
				tags, _ := r["tags"].(map[string]any)
				if tags["apply:set"] == "dev" {
					m[fmt.Sprint(tags["apply:name"])] = r
				}
			}
		}
		return m
	}
	vmidOf := func(r map[string]any) string {
		ref := fmt.Sprint(r["observed"].(map[string]any)["engine_ref"])
		return ref[strings.LastIndex(ref, "/")+1:]
	}
	t.Cleanup(func() {
		// whatever is left, let go of: the box stopped, an empty set applied
		if m := set(); m["box"] != nil {
			_, _, _ = cli("machine", "stop", fmt.Sprint(m["box"]["id"]))
		}
		_ = os.WriteFile(file, []byte("set: dev\nzone: bench\nresources: {}\n"), 0o600)
		_, _, _ = cli("apply", file, "--yes")
		sum := sha256.Sum256([]byte(subject)) // the shelf's name: its owner's, hashed
		_, _ = benchSSH("for id in $(pct list | awk 'NR>1 {print $1}'); do pct config $id | grep -q '^description: made by hangar%3A shelf " +
			hex.EncodeToString(sum[:6]) + "' && pct destroy $id --purge >/dev/null; done; true")
	})

	// the plan, then the box made true
	write(2, false, true, "container")
	if errs := ok("apply", file, "--plan"); !strings.Contains(errs, "4 to create") {
		t.Fatalf("the plan: %s", errs)
	}
	start := time.Now()
	if errs := ok("apply", file); !strings.Contains(errs, "4 created") {
		t.Fatalf("apply: %s", errs)
	}
	t.Logf("the dev box made in %s", time.Since(start).Round(time.Second))
	m := set()
	box, home, cache := m["box"], m["home"], m["cache"]
	vmid := vmidOf(box)
	conf := must("pct config " + vmid)
	t.Logf("pct config %s:\n%s", vmid, conf)
	for _, want := range []string{"memory: 2048", "/home/dev", "/srv/cache", "class.guaranteed+spot"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("the box on the engine: no %q", want)
		}
	}
	if got := must("pct exec " + vmid + " -- sh -c 'echo le-hangar > /home/dev/proof && cat /home/dev/proof && mountpoint -q /srv/cache && echo mounted'"); got != "le-hangar\nmounted" {
		t.Fatalf("in the box: %q", got)
	}
	if !strings.Contains(must("pct exec "+vmid+" -- cat /root/.ssh/authorized_keys"), strings.Fields(string(key))[1]) {
		t.Fatal("the key pair is not in the box")
	}

	// the second time: nothing to do
	if errs := ok("apply", file); !strings.Contains(errs, "Nothing to do") {
		t.Fatalf("again: %s", errs)
	}

	// grown live: one step, the box running
	write(3, false, true, "container")
	if errs := ok("apply", file); !strings.Contains(errs, "resize {cores: 2, memory_gb: 3}") || !strings.Contains(errs, "1 changed") {
		t.Fatalf("the change: %s", errs)
	}
	conf = must("pct config " + vmid)
	if !strings.Contains(conf, "memory: 3072") {
		t.Fatalf("grown: %s", conf)
	}
	if got := must("pct exec " + vmid + " -- cat /home/dev/proof"); got != "le-hangar" {
		t.Fatalf("the box kept its file across the resize: %q", got)
	}
	// the cache backed up: on this engine a container's volume takes its
	// backup flag only while it is stopped — refused running, in the
	// engine's words; done once it is stopped
	write(3, true, true, "container")
	if _, errs, err := cli("apply", file); err == nil || !strings.Contains(errs, "a volume's backup flag changes only while it is stopped") {
		t.Fatalf("a backup flag on a running container: %v\n%s", err, errs)
	} else {
		t.Logf("refused:\n%s", errs)
	}
	ok("machine", "stop", fmt.Sprint(box["id"]))
	if errs := ok("apply", file); !strings.Contains(errs, "set-backup {backup: true}") {
		t.Fatalf("the backup flag: %s", errs)
	}
	ok("machine", "start", fmt.Sprint(box["id"]))
	if conf = must("pct config " + vmid); !strings.Contains(conf, "/srv/cache,backup=1") {
		t.Fatalf("the cache's backup flag on the engine:\n%s", conf)
	}

	// a field set at birth: refused, nothing moved
	write(3, true, true, "vm")
	if _, errs, err := cli("apply", file); err == nil || !strings.Contains(errs, "/kind: it is a container, and a machine's kind is set at its birth") {
		t.Fatalf("a kind changed: %v\n%s", err, errs)
	} else {
		t.Logf("refused:\n%s", errs)
	}

	// the cache leaves the file: unplugged — once the box is stopped — then
	// deleted; the home volume keeps its file
	write(3, true, false, "container")
	if _, errs, err := cli("apply", file); err == nil || !strings.Contains(errs, "run again with --yes") {
		t.Fatalf("a delete unasked: %v\n%s", err, errs)
	}
	if _, errs, err := cli("apply", file, "--yes"); err == nil || !strings.Contains(errs, "running container") {
		t.Fatalf("a volume on a running container: %v\n%s", err, errs)
	} else {
		t.Logf("refused:\n%s", errs)
	}
	ok("machine", "stop", fmt.Sprint(box["id"]))
	if errs := ok("apply", file, "--yes"); !strings.Contains(errs, "- cache: "+fmt.Sprint(cache["id"])+" deleted") {
		t.Fatalf("the cache: %s", errs)
	}
	ok("machine", "start", fmt.Sprint(box["id"]))
	if got := must("pct exec " + vmid + " -- sh -c 'cat /home/dev/proof; mountpoint -q /srv/cache || echo gone'"); got != "le-hangar\ngone" {
		t.Fatalf("after the cache left: %q", got)
	}
	if m = set(); len(m) != 3 || m["home"]["id"] != home["id"] {
		t.Fatalf("the set after: %v", m)
	}

	// last, an empty file: the set let go of, the box stopped first
	ok("machine", "stop", fmt.Sprint(box["id"]))
	if err := os.WriteFile(file, []byte("set: dev\nzone: bench\nresources: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if errs := ok("apply", file, "--yes"); !strings.Contains(errs, "3 deleted") {
		t.Fatalf("the set let go of: %s", errs)
	}
	if out := must("pct list"); strings.Contains(out, vmid+" ") {
		t.Fatalf("the box is still on the engine:\n%s", out)
	}

	_ = srv.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(20 * time.Second):
		t.Fatal("still running 20 s after SIGTERM")
	}
	for _, f := range []string{os.Getenv("HANGAR_BENCH_TOKEN_FILE"), volTok} {
		if strings.Contains(logs.String(), strings.TrimSpace(readFile(f))) {
			t.Fatalf("an engine token is in the brain's logs (%s)", f)
		}
	}
	// the brain again, for the clean-up's calls
	srv = serve()
	go func() { _ = srv.Wait() }()
}
