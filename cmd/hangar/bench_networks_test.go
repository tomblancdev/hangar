package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Networks through the binary, against a real Proxmox VE, as an operator
// runs it: two plugins, each with its own token. A person's first machine
// names no network: their default is made — its gateway — and the machine is
// born on it, at an address of its range. Nobody jumps into it until the
// network names a key pair; then its owner's key does, through a gateway made
// again. A gateway stopped by hand under a running machine is started at the
// brain's next look; its last machine stopped, it rests; and the network is
// not deleted while a machine stands on it. Without the bench's variables it
// skips.
func TestBenchANetworkThroughTheAPI(t *testing.T) {
	url := os.Getenv("HANGAR_BENCH_URL")
	if url == "" {
		t.Skip("no bench: sh tools/bench/bench.sh up, then eval its env")
	}
	benchSSH := func(cmd string, stdin ...string) (string, error) {
		c := exec.Command("ssh", "-i", os.Getenv("HANGAR_BENCH_SSH_KEY"), "-p", os.Getenv("HANGAR_BENCH_SSH_PORT"),
			"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
			benchRoot(), cmd)
		if len(stdin) > 0 {
			c.Stdin = strings.NewReader(stdin[0])
		}
		out, err := c.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	archive, err := benchSSH(`ls /var/lib/vz/template/cache/ | grep "^debian-13-standard_.*_$(dpkg --print-architecture)\." | sort -V | tail -1`)
	if err != nil || archive == "" {
		t.Fatalf("no container template on the bench: %v %s", err, archive)
	}
	gateway, err := benchSSH(`ls /var/lib/vz/template/cache/ | grep '^hangar-gateway-.*\.tar\.zst$' | sort -V | tail -1`)
	if err != nil || gateway == "" || os.Getenv("HANGAR_BENCH_NETWORKS_TOKEN_FILE") == "" || os.Getenv("HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE") == "" {
		t.Skip("a bench set up before the networks: sh tools/bench/bench.sh up")
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
	cfg := filepath.Join(dir, "hangar.yaml")
	text := fmt.Sprintf(`
data_dir: %s/data
tiers:
  - name: users
    groups: [users]
    zones: [bench]
    limits:
      machines.count: 2
      machines.vcpu_hours: 1000
      machines.vcpu: 4
      machines.memory_gb: 4
      machines.disk_gb: 16
      machines.key_pairs: 1
      machines.kind: [container]
      machines.class: [spot]
      networks.count: 1
      networks.visibility: [private]
zones:
  - name: bench
    driver: proxmox
    endpoint: %s
    options:
      node: pve-bench
      pool: hangar
      images_pool: hangar-images
      storage: local-zfs
      seed_storage: hangar-seeds
      bridge: hbnet
      vmids: 11120-11139
      ca_file: %s
      subnet: 198.51.100.0/24
      first_address: 198.51.100.40
      gateway: 198.51.100.1
      firewall: "on"
      firewall_groups: hangar-floor
      firewall_log: info
      net_bridge: hgnets        # networks of the cloud's own, each a tag of this bridge
      net_tag: "110"
      net_block: 192.0.2.0/24
      net_size: "27"
      net_vmids: 11210-11213    # their gateways: a network's number is its gateway's
      net_pool: hangar-nets
      net_address: 198.51.100.210
      net_archive: "local:vztmpl/%s"
    room:
      memory_gb: 6
      reservations: [{name: priority, memory_gb: 3, while_running: "100"}]
plugins:
  - name: machines
    builtin: machines
    zones: [bench]
    credentials:
      bench: {file: %s}
    settings:
      images:
        debian-13: {container: "local:vztmpl/%s"}
  - name: networks
    builtin: networks
    zones: [bench]
    credentials:
      bench: {file: %s}
reconcile: {every: 5s}
`, dir, url, os.Getenv("HANGAR_BENCH_CA_FILE"), gateway, os.Getenv("HANGAR_BENCH_MACHINES_NETS_TOKEN_FILE"), archive, os.Getenv("HANGAR_BENCH_NETWORKS_TOKEN_FILE"))
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(file string, args ...string) (string, error) {
		var out, errb bytes.Buffer
		cmd := exec.Command(bin, append(args, "--config", file)...)
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return out.String(), fmt.Errorf("%v: %s", err, errb.String())
		}
		return out.String(), nil
	}
	if out, err := run(cfg, "check"); err != nil || !strings.Contains(out, "network") || !strings.Contains(out, "net.private") {
		t.Fatalf("check: %v\n%s", err, out)
	} else {
		t.Logf("hangar check:\n%s", out)
	}
	// a file that says what its zone cannot hold is refused by check, in the
	// driver's own words — a mistyped word, gateways among the guests
	for want, edit := range map[string][2]string{
		"no option net_adress":       {"net_address: 198.51.100.210", "net_adress: 198.51.100.210"},
		"run into the guests'":       {"net_address: 198.51.100.210", "net_address: 198.51.100.57"},
		"outside what":               {"first_address: 198.51.100.40", "first_address: 198.51.100.250"},
		"no security group's name":   {"firewall_groups: hangar-floor", "firewall_groups: \"hangar floor\""},
		"a gateway's id is no machi": {"net_vmids: 11210-11213", "net_vmids: 11130-11133"},
	} {
		broken := filepath.Join(dir, "broken.yaml")
		if !strings.Contains(text, edit[0]) {
			t.Fatalf("the file has no %q to break", edit[0])
		}
		if err := os.WriteFile(broken, []byte(strings.Replace(text, edit[0], edit[1], 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := run(broken, "check"); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(out, "sound") {
			t.Fatalf("a file with %q came back: %v\n%s", edit[1], err, out)
		}
	}
	secret, err := run(cfg, "token", "create", "--subject", "alice", "--groups", "users", "--name", "bench", "--ttl", "1h")
	if err != nil {
		t.Fatalf("token: %v %s", err, secret)
	}
	secret = strings.TrimSpace(secret)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	var logs bytes.Buffer
	srv := exec.Command(bin, "serve", "--config", cfg)
	srv.Env = append(os.Environ(), "HANGAR_LISTEN="+addr)
	srv.Stdout, srv.Stderr = &logs, &logs
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Process.Kill() })

	call := func(method, path string, body any) (int, map[string]any) {
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, "http://"+addr+path, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, map[string]any{"error": err.Error()}
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		if code, _ := call("GET", "/healthz", nil); code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy:\n%s", logs.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	await := func(what string, code int, acc map[string]any) map[string]any {
		t.Helper()
		if code != 202 {
			t.Fatalf("%s: %d %v\n%s", what, code, acc, logs.String())
		}
		op := acc["operation"].(map[string]any)["id"].(string)
		for {
			_, o := call("GET", "/v1/operations/"+op+"?wait=60", nil)
			switch o["state"] {
			case "succeeded":
				return o
			case "failed":
				t.Fatalf("%s failed: %v\n%s", what, o, logs.String())
			}
		}
	}
	get := func(id string) map[string]any { _, r := call("GET", "/v1/resources/"+id, nil); return r }
	at := func(r map[string]any, path ...string) string {
		var v any = r
		for _, p := range path {
			m, _ := v.(map[string]any)
			v = m[p]
		}
		return fmt.Sprint(v)
	}
	eventually := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(within); !ok(); {
			if time.Now().After(deadline) {
				t.Fatalf("%s: still not so after %s\n%s", what, within, logs.String())
			}
			time.Sleep(2 * time.Second)
		}
	}
	deleted := map[string]bool{}
	gone := func(what, id string) {
		t.Helper()
		code, acc := call("DELETE", "/v1/resources/"+id, nil)
		await("deleting "+what, code, acc)
		deleted[id] = true
	}

	// a key of her own, its private half on the bench — the tests come in
	// from the bench's node, as someone on the lane would
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "alice", "-f", filepath.Join(dir, "k")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, _ := os.ReadFile(filepath.Join(dir, "k.pub"))
	priv, _ := os.ReadFile(filepath.Join(dir, "k"))
	onBench := "/root/hangar-api-net-key"
	if out, err := benchSSH("umask 077 && cat > "+onBench, string(priv)); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	t.Cleanup(func() { _, _ = benchSSH("rm -f " + onBench) })
	code, acc := call("POST", "/v1/resources", map[string]any{"type": "keypair", "zone": "bench", "name": "laptop", "spec": map[string]any{"public_key": string(pub)}})
	await("the key pair", code, acc)
	kp := at(acc, "resource", "id")

	// her first machine names no network: her default is made, and it is born there
	began := time.Now()
	code, acc = call("POST", "/v1/resources", map[string]any{"type": "machine", "zone": "bench", "name": "first", "spec": map[string]any{
		"kind": "container", "image": "debian-13", "type": "t3.micro", "disk_gb": 2, "key_pairs": []string{"laptop"}}})
	if code != 202 {
		t.Fatalf("her first machine: %d %v\n%s", code, acc, logs.String())
	}
	t.Logf("a first machine was accepted %s after it was asked: its owner's default network was made first", time.Since(began).Round(time.Second))
	m := at(acc, "resource", "id")
	t.Cleanup(func() {
		for _, id := range []string{m, at(get(m), "spec", "network")} {
			if id != "" && id != "<nil>" && !deleted[id] {
				c, a := call("DELETE", "/v1/resources/"+id, nil)
				if c == 202 {
					op := at(a, "operation", "id")
					for range 30 {
						if _, o := call("GET", "/v1/operations/"+op+"?wait=10", nil); o["state"] != "running" {
							break
						}
					}
				}
			}
		}
	})
	await("her first machine", code, acc)
	t.Logf("…and ran %s after it was asked", time.Since(began).Round(time.Second))
	machine := get(m)
	netID := at(machine, "spec", "network")
	network := get(netID)
	if at(network, "name") != "default" || at(network, "tags", "hangar:default") != "machine.network" || !strings.HasPrefix(at(network, "observed", "range"), "192.0.2.") ||
		!strings.HasPrefix(at(machine, "observed", "address"), "192.0.2.") || at(machine, "observed", "wall") != "exact" || at(network, "observed", "wall") != "exact" {
		t.Fatalf("her machine %v\non her network %v", machine, network)
	}
	address, jumpAt := at(machine, "observed", "address"), at(network, "observed", "jump")
	gw := at(network, "observed", "engine_ref")
	gw = gw[strings.LastIndex(gw, "/")+1:]
	if out, _ := benchSSH("pct status " + gw); out != "status: running" {
		t.Fatalf("her machine runs, its network's gateway is %q", out)
	}
	if out, _ := benchSSH("pvesh get /pools/hangar-nets --output-format json"); !strings.Contains(out, `"vmid":`+gw) {
		t.Fatalf("the gateway is not in the gateways' pool: %s", out)
	}

	// nobody jumps into a network that names no key pair
	ssh := "ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=6 -i " + onBench + " "
	jump := func() (string, error) {
		return benchSSH(ssh + "-o ProxyCommand=\"" + ssh + "-W %h:%p " + jumpAt + "\" root@" + address + " hostname")
	}
	if out, err := jump(); err == nil || !strings.Contains(out, "Permission denied") {
		t.Fatalf("a jump into a network that names no key pair: %q %v", out, err)
	}
	// it names hers: its gateway is made again with it, and she jumps to her machine
	code, acc = call("POST", "/v1/resources/"+netID+"/actions/set_key_pairs", map[string]any{"params": map[string]any{"key_pairs": []string{"laptop"}}})
	await("its key pairs set", code, acc)
	if network = get(netID); at(network, "observed", "keys") != "1" || at(network, "spec", "key_pairs") != "["+kp+"]" || at(network, "observed", "running") != "true" {
		t.Fatalf("after its key pairs were set: %v", network)
	}
	eventually("her key jumps through her network's gateway to her machine", time.Minute, func() bool {
		out, err := jump()
		return err == nil && out == "first"
	})
	if out, err := benchSSH(ssh + jumpAt + " id"); err == nil {
		t.Fatalf("her key got a command on the gateway: %q", out)
	}

	// a hand stops the gateway under her running machine: the brain's next look starts it
	if out, err := benchSSH("pct stop " + gw); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	eventually("the brain started the gateway again", time.Minute, func() bool {
		out, _ := benchSSH("pct status " + gw)
		return out == "status: running" && strings.Contains(logs.String(), "its network's gateway (started)")
	})
	eventually("her network reads up", 30*time.Second, func() bool { return at(get(netID), "status") == "up" })

	// a network a machine stands on is not deleted
	if code, r := call("DELETE", "/v1/resources/"+netID, nil); code != 409 || at(r, "kind") != "members" || !strings.Contains(at(r, "detail"), "(default) still holds first") {
		t.Fatalf("her network deleted under her machine: %d %v", code, r)
	}
	// her machine stopped, its gateway rests — a node with nothing running can sleep
	code, acc = call("POST", "/v1/resources/"+m+"/actions/stop", nil)
	await("the stop", code, acc)
	if out, _ := benchSSH("pct status " + gw); out != "status: stopped" {
		t.Fatalf("her last machine stopped, the gateway is %q", out)
	}
	eventually("her network reads idle", 30*time.Second, func() bool { return at(get(netID), "status") == "idle" })
	if running, _ := benchSSH("pct list | awk '$2 == \"running\"' | wc -l"); running != "0" {
		t.Fatalf("%s guests still run on the bench", running)
	}
	// deleted in their order, and the gateway goes with its network
	gone("her machine", m)
	gone("her network", netID)
	if out, err := benchSSH("pct status " + gw); err == nil {
		t.Fatalf("the gateway stayed: %s", out)
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("the brain logged an error:\n%s", logs.String())
	}
}
