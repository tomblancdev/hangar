package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tomblancdev/hangar/internal/identity"
	"github.com/tomblancdev/hangar/internal/testoidc"
)

// A token of actions asks those actions and nothing else write allows; one
// bound to a resource, of that one alone — by the id its name stood for when
// the token was made.
func TestATokenOfActions(t *testing.T) {
	s := newStack(t)
	alice := s.token("alice", "users")
	person := s.iss.Token(t, testoidc.Claims{Subject: "alice", Audience: "hangar", Groups: []string{"users"}})
	dev := s.create(alice, map[string]any{"type": "box", "zone": "z", "name": "dev"}).str("resource", "id")
	ci := s.create(alice, map[string]any{"type": "box", "zone": "z", "name": "ci"}).str("resource", "id")
	mint := func(scopes ...string) reply {
		t.Helper()
		return s.do("POST", "/v1/tokens", person, map[string]any{"name": "acts", "expires_in": 3600, "scopes": scopes})
	}
	act := func(bearer, id, action string) reply {
		t.Helper()
		return s.do("POST", "/v1/resources/"+id+"/actions/"+action, bearer, nil)
	}
	succeeded := func(what string, r reply) {
		t.Helper()
		if r.code != 202 || s.done(alice, r.str("operation", "id")).str("state") != "succeeded" {
			t.Fatalf("%s: %d %v", what, r.code, r.body)
		}
	}
	refused := func(what string, r reply, detail string) {
		t.Helper()
		if r.code != 403 || r.str("kind") != "scope" || !strings.Contains(r.str("detail"), detail) {
			t.Fatalf("%s: %d %v", what, r.code, r.body)
		}
	}

	// what is refused at the making, and why
	for scopes, why := range map[string]string{
		"write,box.start":                 "write already asks every action",
		"room,box.start":                  "holds that scope alone",
		"box.strat":                       `type box has no action "strat"`,
		"crate.start":                     `no type "crate" here`,
		"box.start:nobody":                "alice calls no live box nobody",
		"box.start:box-0123456789abcdef0": "box-0123456789abcdef0 is no live box of alice's",
		"start":                           `no scope "start"`,
		"box.start:Dev":                   `no scope "box.start:Dev"`,
	} {
		if r := mint(strings.Split(scopes, ",")...); r.code != 400 || !strings.Contains(r.str("detail"), why) {
			t.Errorf("%s: %d %v", scopes, r.code, r.body)
		}
	}

	// read and box.start: it reads, it starts any box of hers, it does
	// nothing else — nor anything of bob's
	r := mint("read", "box.start", "read")
	if r.code != 201 || r.str("token", "scopes") != "[read box.start]" {
		t.Fatalf("mint: %d %v", r.code, r.body)
	}
	starts := r.str("secret")
	if w := s.do("GET", "/v1/whoami", starts, nil); w.code != 200 || w.str("scopes") != "[read box.start]" {
		t.Fatalf("whoami: %d %v", w.code, w.body)
	}
	succeeded("her stop", act(alice, dev, "stop"))
	succeeded("its start", act(starts, dev, "start"))
	refused("its stop", act(starts, dev, "stop"), "this token may only read, box.start: not stop of "+dev)
	refused("its resize", s.do("POST", "/v1/resources/"+dev+"/actions/resize", starts, map[string]any{"params": map[string]any{"cores": 1, "memory_gb": 1}}), "not resize of "+dev)
	refused("a create", s.do("POST", "/v1/resources", starts, map[string]any{"type": "box", "zone": "z"}), "not a create")
	refused("a delete", s.do("DELETE", "/v1/resources/"+ci, starts, nil), "not a delete")
	refused("a name", s.do("PATCH", "/v1/resources/"+ci, starts, map[string]any{"name": "mine"}), "not a name")
	if w := s.do("POST", "/v1/tokens", starts, map[string]any{"name": "x", "expires_in": 3600}); w.code != 403 {
		t.Fatalf("a token of actions made a token: %d", w.code)
	}
	bob := s.token("bob", "users")
	bobs := s.create(bob, map[string]any{"type": "box", "zone": "z"}).str("resource", "id")
	if w := act(starts, bobs, "start"); w.code != 404 {
		t.Fatalf("it starts bob's: %d %v", w.code, w.body)
	}

	// bound to ci by its name: ci's stop alone — not even a read
	r = mint("box.stop:ci")
	if r.code != 201 || r.str("token", "scopes") != "[box.stop:"+ci+"]" {
		t.Fatalf("bound: %d %v", r.code, r.body)
	}
	stops, stopsID := r.str("secret"), r.str("token", "id")
	r = act(stops, ci, "stop")
	succeeded("ci's stop", r)
	if _, told := r.body["resource"]; told {
		t.Fatalf("a token that cannot read is told the resource it stopped: %v", r.body)
	}
	refused("dev's stop", act(stops, dev, "stop"), "this token may only box.stop:"+ci+": not stop of "+dev)
	// an id nobody holds is told what any other is: nothing of what is there
	refused("no one's", act(stops, "box-0123456789abcdef0", "stop"), "not stop of box-0123456789abcdef0")
	refused("ci's start", act(stops, ci, "start"), "not start of "+ci)
	if w := s.do("GET", "/v1/resources/"+ci, stops, nil); w.code != 403 || w.str("kind") != "scope" {
		t.Fatalf("a token of one action reads: %d %v", w.code, w.body)
	}
	// a HEAD runs the GET's handler: it is a read too
	if w := s.do("HEAD", "/v1/resources/"+ci, stops, nil); w.code != 403 {
		t.Fatalf("a token of one action reads by HEAD: %d", w.code)
	}
	// an id of another type sharing the action's name is refused before it
	// is looked up, as one of its own type would be: 403, never a 404
	refused("another type's id", act(starts, "m-0123456789abcdef0", "start"), "this token may only read, box.start: not start of m-0123456789abcdef0")

	// ci made again under its name is not the token's: the id was
	succeeded("ci's delete", s.do("DELETE", "/v1/resources/"+ci, alice, nil))
	again := s.create(alice, map[string]any{"type": "box", "zone": "z", "name": "ci"}).str("resource", "id")
	if again == ci || again == "" {
		t.Fatalf("ci again: %q", again)
	}
	refused("the new ci's stop", act(stops, again, "stop"), "not stop of "+again)

	// a token that does not write revokes itself alone
	if w := s.do("DELETE", "/v1/tokens/"+stopsID, starts, nil); w.code != 403 || w.str("kind") != "scope" {
		t.Fatalf("a token of actions revoked another: %d %v", w.code, w.body)
	}
	if w := s.do("DELETE", "/v1/tokens/"+stopsID, stops, nil); w.code != 204 {
		t.Fatalf("a token revokes itself: %d %v", w.code, w.body)
	}
	if w := act(stops, again, "stop"); w.code != 401 {
		t.Fatalf("revoked, it still asks: %d %v", w.code, w.body)
	}

	// the command beside the registry binds names as the brain does; it
	// knows no plugin, so an action it cannot check is held as asked
	ctx := context.Background()
	if got, err := identity.Bind(ctx, s.store, "alice", false, []string{"read", "box.strat:ci"}, nil); err != nil || strings.Join(got, ",") != "read,box.strat:"+again {
		t.Fatalf("bound beside the registry: %v %v", got, err)
	}
	if _, err := identity.Bind(ctx, s.store, "alice", false, []string{"box.stop:" + ci}, nil); err == nil || !strings.Contains(err.Error(), "is no live box of alice's") {
		t.Fatalf("a deleted id: %v", err)
	}
	if _, err := identity.Bind(ctx, s.store, "alice", false, []string{"box.stop:" + bobs}, nil); err == nil {
		t.Fatal("bound to bob's box for alice")
	}
	if got, err := identity.Bind(ctx, s.store, "root", true, []string{"box.stop:" + bobs}, nil); err != nil || got[0] != "box.stop:"+bobs {
		t.Fatalf("an operator's binds anyone's: %v %v", got, err)
	}

	// the audit says each refusal, and by which token
	var seen bool
	for _, line := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(line, `"kind":"audit"`) && strings.Contains(line, `"reason":"scope"`) && strings.Contains(line, "not stop of "+dev) && strings.Contains(line, `"via":"token:`) {
			seen = true
		}
	}
	if !seen {
		t.Error("no audit line for the refused stop")
	}
}

// A token that cannot read learns of a resource only what acting on it
// tells: one shared with its owner's group is, to it, not there — where a
// token that reads is told whose it is.
func TestATokenThatCannotReadIsToldNothingOfWhatIsShared(t *testing.T) {
	s := netStack{newStackWith(t, networksConfig, "machines", "networks"), t}
	alice := s.token("alice", "users", "family")
	s.made(s.create(alice, map[string]any{"type": "keypair", "zone": "z", "name": "laptop", "spec": map[string]any{"public_key": netKeyA}}))
	net := s.made(s.create(alice, map[string]any{"type": "network", "zone": "z", "name": "lab", "spec": map[string]any{"key_pairs": []string{"laptop"}}}))
	if r := s.act(alice, net, "share", map[string]any{"shared_with": []string{"family"}}); r.code != 202 {
		t.Fatalf("%d %v", r.code, r.body)
	}
	mint := func(scopes ...string) string {
		secret, _, err := identity.Mint(context.Background(), s.store, "bob", "acts", []string{"users", "family"}, scopes, time.Hour, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	ask := func(bearer string) reply {
		return s.do("POST", "/v1/resources/"+net+"/actions/set_key_pairs", bearer, map[string]any{"params": map[string]any{"key_pairs": []string{}}})
	}
	if r := ask(mint("read", "network.set_key_pairs")); r.code != 403 || r.str("kind") != "shared" || !strings.Contains(r.str("detail"), "alice") {
		t.Fatalf("a token that reads: %d %v", r.code, r.body)
	}
	if r := ask(mint("network.set_key_pairs")); r.code != 404 || r.str("kind") != "not-found" || strings.Contains(r.str("detail"), "alice") {
		t.Fatalf("a token that cannot read: %d %v", r.code, r.body)
	}
}
