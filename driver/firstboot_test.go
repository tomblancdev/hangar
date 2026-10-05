package driver

import (
	"bytes"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

type bootPart struct{ kind, name, body string }

// walk reads a first boot as cloud-init does: every leaf of a MIME message,
// whatever is nested in it, each with its type, its file name and its body
// decoded.
func walk(t *testing.T, header mail.Header, body io.Reader) []bootPart {
	t.Helper()
	mt, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("a part's type %q: %v", header.Get("Content-Type"), err)
	}
	if !strings.HasPrefix(mt, "multipart/") {
		raw, _ := io.ReadAll(body)
		if strings.EqualFold(header.Get("Content-Transfer-Encoding"), "base64") {
			if raw, err = base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), "")); err != nil {
				t.Fatal(err)
			}
		}
		_, disp, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
		return []bootPart{{mt, disp["filename"], string(raw)}}
	}
	var out []bootPart
	mr := multipart.NewReader(body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, walk(t, mail.Header(p.Header), p)...)
	}
}

func parts(t *testing.T, firstBoot []byte) []bootPart {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(firstBoot))
	if err != nil {
		t.Fatal(err)
	}
	return walk(t, msg.Header, msg.Body)
}

// A guest's first boot is handed its owner's user data, and a step of the
// cloud's own goes BESIDE it: the owner's part arrives as it was written,
// byte for byte, whatever it is. With no step nothing is added at all.
func TestAStepRidesBesideTheOwnersUserData(t *testing.T) {
	config := "#cloud-config\npackages: [htop]\nruncmd:\n  - echo 'été — ok'\n"
	script := "#!/bin/sh\necho mine >/var/tmp/mine\n"
	theirs := "Content-Type: multipart/mixed; boundary=\"mine\"\nMIME-Version: 1.0\n\n--mine\nContent-Type: text/cloud-config\n\n" + config +
		"\n--mine\nContent-Type: text/x-shellscript\nContent-Disposition: attachment; filename=\"second.sh\"\n\n" + script + "\n--mine--\n"

	for _, own := range []string{"", config, script, theirs} {
		if got := FirstBoot([]byte(own)); string(got) != own {
			t.Fatalf("with no step the user data travels as it is; it became %q", got)
		}
	}

	step := Step{Kind: "text/cloud-boothook", Name: "a-step", Body: "#!/bin/sh\necho the cloud's\n"}
	last := func(ps []bootPart) bootPart { return ps[len(ps)-1] }

	// nothing of the owner's: the step alone
	if ps := parts(t, FirstBoot(nil, step)); len(ps) != 1 || ps[0] != (bootPart{step.Kind, step.Name, step.Body}) {
		t.Fatalf("a step with no user data: %+v", ps)
	}
	// a cloud-config, a script: labelled as text — what it is is read from
	// its first line, as when it is alone — and not a byte of it moved
	for _, own := range []string{config, script} {
		ps := parts(t, FirstBoot([]byte(own), step))
		if len(ps) != 2 || ps[0].kind != "text/plain" || ps[0].body != own || last(ps) != (bootPart{step.Kind, step.Name, step.Body}) {
			t.Fatalf("user data and a step: %+v", ps)
		}
	}
	// user data that is a multipart already: its parts, under their own
	// types and names, then the step
	ps := parts(t, FirstBoot([]byte(theirs), step))
	if len(ps) != 3 || ps[0].kind != "text/cloud-config" || strings.TrimSpace(ps[0].body) != strings.TrimSpace(config) ||
		ps[1].kind != "text/x-shellscript" || ps[1].name != "second.sh" || strings.TrimSpace(ps[1].body) != strings.TrimSpace(script) ||
		last(ps).name != step.Name {
		t.Fatalf("a multipart of the owner's and a step: %+v", ps)
	}
	// …whatever header it begins with, and a message of one part too: read
	// as cloud-init reads it alone — a message, since it says MIME-Version
	other := "X-Mine: yes\n" + theirs
	if ps := parts(t, FirstBoot([]byte(other), step)); len(ps) != 3 || ps[0].kind != "text/cloud-config" || last(ps).name != step.Name {
		t.Fatalf("a multipart that begins with another header: %+v", ps)
	}
	one := "MIME-Version: 1.0\nContent-Type: text/x-shellscript; charset=\"utf-8\"\nContent-Transfer-Encoding: base64\n\n" +
		base64.StdEncoding.EncodeToString([]byte(script)) + "\n"
	if ps := parts(t, FirstBoot([]byte(one), step)); len(ps) != 2 || ps[0].kind != "text/x-shellscript" || ps[0].body != script || last(ps).name != step.Name {
		t.Fatalf("a message of one part, encoded: %+v", ps)
	}
	// a script that only mentions the word is still a script
	mentions := "#!/bin/sh\n# writes a mail: MIME-Version: 1.0\necho hi\n"
	if ps := parts(t, FirstBoot([]byte(mentions), step)); len(ps) != 2 || ps[0].kind != "text/plain" || ps[0].body != mentions {
		t.Fatalf("a script that mentions MIME-Version: %+v", ps)
	}
}

// The step that signs a console in: a boothook — read before the first
// login is offered —, written once, for every serial port of the guest, and
// naming no user: who the guest's user is is found when the getty starts —
// which is when a terminal is there to say its size, and not before.
func TestTheSignedInConsolesStep(t *testing.T) {
	s := SignedInConsole
	if s.Kind != "text/cloud-boothook" || !strings.HasPrefix(s.Body, "#!/bin/sh\n") {
		t.Fatalf("a boothook that is a script: %s, %q", s.Kind, s.Body[:20])
	}
	for _, want := range []string{
		"/etc/systemd/system/serial-getty@.service.d/hangar-terminal.conf",              // every serial port's getty
		`[ -e "$conf" ] && [ "$(cat "$mine" 2>/dev/null)" = "$INSTANCE_ID" ] && exit 0`, // once for this machine: a boothook runs at every boot
		"ExecStart=\nExecStart=-/usr/local/sbin/hangar-terminal",                        // the unit's own start, replaced
		"--autologin \"$u\"",
		"-o '-p -f -- \\u'", // one backslash: a script's, not a unit file's two
		"system_info.default_user.name", "getent passwd 1000", "u=root",
		"systemctl daemon-reload",
		// it waits for a terminal, and asks its size with a question of its
		// own — never the cursor's place, which a machine's init asks too
		`printf '\033[18t'`, `[8;`, "xterm-256color",
		// this machine's alone: its instance id beside it, and a getty that
		// asks a login anywhere else — on a machine born from an image of it
		`"$INSTANCE_ID" >"$mine"`, "/etc/hangar-terminal.instance", "/var/lib/cloud/data/instance-id", `-o '-p -- \u'`,
		// the port is the script's own input, whatever the image's unit does;
		// with none it waits and leaves, and never spins
		"StandardInput=tty\nStandardOutput=tty\nTTYPath=/dev/%I", "[ -t 0 ] || { sleep 5; exit 1; }",
		"[ -x /usr/local/sbin/hangar-terminal ] || exit 0",
	} {
		if !strings.Contains(s.Body, want) {
			t.Errorf("the step lacks %q", want)
		}
	}
	for _, never := range []string{"debian", "ubuntu", "passwd -d", "chpasswd", "[6n"} {
		if strings.Contains(s.Body, never) {
			t.Errorf("the step names %q: it knows no image's user, and sets no password", never)
		}
	}
}
