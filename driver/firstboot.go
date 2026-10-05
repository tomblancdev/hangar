package driver

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/mail"
)

// A guest's first boot is handed its owner's user data — and, where the
// cloud has a step of its own for it, that step BESIDE the user data, never
// in it: a MIME multipart cloud-init reads part by part, the owner's part as
// they wrote it. A guest with no step of the cloud's is handed its owner's
// user data as it is, not one byte around it.

// Step is a part of a first boot that is the cloud's own.
type Step struct {
	// Kind is the part's type as cloud-init reads it: "text/cloud-boothook"
	// (run early at every boot), "text/x-shellscript" (run once, last).
	Kind string
	// Name is the part's file name: what the guest's logs call it.
	Name string
	Body string
}

// SignedInConsole is the step that makes a guest's console ask nothing
// (GuestSpec.SignedIn): the getty of its serial ports signs the guest's own
// user in. Whoever reaches that port was let through by the cloud, as the
// guest's owner — the port is on no network, and the engine opens it to the
// cloud's credential alone.
//
// It is a boothook: written before the first login is offered — cloud-init
// reads it ahead of the getty's start, and the getty starts after the
// guest's users are made —, so the way in is there while the owner's own
// first boot still runs.
//
// What it puts in place of the getty waits for a terminal at the other end
// before it signs anyone in, and asks it its size: a serial port carries no
// window size, so a shell started at boot, with nobody there, would believe
// 80 by 24 whatever screen is opened on it later. A terminal answers the
// question by itself (« report your text area's size », in characters);
// one that cannot is let in on Enter, at the port's own size. So no
// shell sits open on a port nobody holds, and the one a person gets fits
// their window — as it was when they signed in: exit signs in again.
//
// The step is THIS guest's: it is written with the guest's own instance id,
// and the getty signs in only on the guest it was written for. An image
// saved from a guest born signed in carries its files; a guest born from
// that image without the step — one that asked a login — gets a login.
//
// The user is found then too, not when this is written: the one cloud-init
// made for the image (its default user), else the first ordinary account
// (an owner who names their own), else root. For guests whose init is
// systemd; another boots as its image says, and its console shows what the
// image shows.
var SignedInConsole = Step{Kind: "text/cloud-boothook", Name: "hangar-terminal", Body: `#!/bin/sh
# hangar: this machine's terminal opens signed in. Its serial port is the
# cloud's own way in - whoever reaches it was let through by the cloud, as
# this machine's owner - so the port's getty asks nothing and signs the
# machine's user in. Written once for this machine (its instance id beside
# it), before the first login is offered.
conf=/etc/systemd/system/serial-getty@.service.d/hangar-terminal.conf
mine=/etc/hangar-terminal.instance
[ -e "$conf" ] && [ "$(cat "$mine" 2>/dev/null)" = "$INSTANCE_ID" ] && exit 0
[ -d /run/systemd/system ] || exit 0
mkdir -p /usr/local/sbin "${conf%/*}"
cat >/usr/local/sbin/hangar-terminal <<'EOF'
#!/bin/sh
# The getty of this machine's serial ports - its terminal, as its cloud
# opens it. It waits for a terminal at the other end and asks it its size (a
# serial port carries none), then signs the machine's user in, nothing asked.
agetty=$(command -v agetty) || agetty=/sbin/agetty
keep=
"$agetty" --help 2>&1 | grep -q -- --noreset && keep=--noreset
# the port is this script's own input and output (the unit's drop-in says
# so, whatever the image's unit does); without it there is nothing to ask
[ -t 0 ] || { sleep 5; exit 1; }

# only on the machine this was written for: one born from an image of it
# that did not ask for this carries the file too, and asks a login
[ "$(cat /etc/hangar-terminal.instance 2>/dev/null)" = "$(cat /var/lib/cloud/data/instance-id 2>/dev/null)" ] ||
	exec "$agetty" -o '-p -- \u' $keep --noclear --keep-baud 115200,57600,38400,9600 - "${TERM:-vt220}"

# a terminal says its size when asked (in characters: « report the text
# area »). Asked every two seconds until one answers - or Enter is pressed,
# by someone whose terminal cannot say. Nothing else that arrives counts: an
# answer to a question the machine's init asked a moment ago is not ours.
cr=$(printf '\r')
saved=$(stty -g)
stty raw -echo
size=
while :; do
	printf '\033[18t'
	got=
	stty min 0 time 20
	while chunk=$(dd bs=64 count=1 2>/dev/null) && [ -n "$chunk" ]; do
		got=$got$chunk
		case $got in *'[8;'*t*) break ;; esac
		stty min 0 time 2
	done
	size=$(printf %s "$got" | sed -n 's/.*\[8;\([0-9]\{1,4\}\);\([0-9]\{1,4\}\)t.*/\1 \2/p')
	[ -n "$size" ] && break
	case $got in *"$cr"*) break ;; esac
done
# what comes late, or twice, is not for the shell to read
stty min 0 time 3
while [ -n "$(dd bs=64 count=1 2>/dev/null)" ]; do :; done
stty "$saved"
term=${TERM:-vt220}
set -- $size
if [ $# = 2 ]; then
	stty rows "$1" cols "$2"
	# what answers is a terminal of today's: let it show its colours
	for d in /usr/share/terminfo /lib/terminfo /etc/terminfo; do
		[ -e "$d/x/xterm-256color" ] && term=xterm-256color
	done
fi

# who the machine's user is: the one its first boot made (cloud-init's
# default user), else its first ordinary account, else root
u=$(cloud-init query merged_system_cfg.system_info.default_user.name 2>/dev/null) || u=
id "$u" >/dev/null 2>&1 || u=$(getent passwd 1000 | cut -d: -f1)
id "$u" >/dev/null 2>&1 || u=root
exec "$agetty" -o '-p -f -- \u' --autologin "$u" $keep --noclear --keep-baud 115200,57600,38400,9600 - "$term"
EOF
chmod 755 /usr/local/sbin/hangar-terminal
# nothing points the getty at a script that did not land
[ -x /usr/local/sbin/hangar-terminal ] || exit 0
printf '%s\n' "$INSTANCE_ID" >"$mine"
cat >"$conf" <<'EOF'
[Service]
ExecStart=
ExecStart=-/usr/local/sbin/hangar-terminal
# the port itself as the script's input and output: an image's own unit may
# name it to its getty instead, and leave them closed
StandardInput=tty
StandardOutput=tty
TTYPath=/dev/%I
EOF
systemctl daemon-reload
`}

// firstBootBoundary parts the cloud's steps from the owner's user data.
const firstBootBoundary = "==hangar-first-boot-5c1e9a=="

// FirstBoot is what a guest's first boot is handed: its owner's user data
// and the cloud's steps after it. No step: the user data as it is. The
// owner's part is not read here, and means what it meant alone: user data
// cloud-init itself would read as a MIME message — it says « MIME-Version »
// in its head, cloud-init's own test — goes in whole as a part under its own
// type (a multipart: cloud-init walks into it), and anything else is
// labelled as text, which cloud-init tells apart by its first line as it
// does when it is alone.
func FirstBoot(userData []byte, steps ...Step) []byte {
	if len(steps) == 0 {
		return userData
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\nMIME-Version: 1.0\r\n\r\n", firstBootBoundary)
	part := func(kind, name string, body []byte) {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=\"utf-8\"\r\nMIME-Version: 1.0\r\nContent-Transfer-Encoding: base64\r\n"+
			"Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", firstBootBoundary, kind, name)
		enc := base64.StdEncoding.EncodeToString(body)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	if head, body, ok := messageOf(userData); ok {
		fmt.Fprintf(&b, "--%s\r\n%s\r\n", firstBootBoundary, head)
		b.Write(body)
		b.WriteString("\r\n")
	} else if len(bytes.TrimSpace(userData)) > 0 {
		part("text/plain", "user-data", userData)
	}
	for _, s := range steps {
		part(s.Kind, s.Name, []byte(s.Body))
	}
	fmt.Fprintf(&b, "--%s--\r\n", firstBootBoundary)
	return b.Bytes()
}

// messageOf reads user data that cloud-init would read as a MIME message
// (its own test: « mime-version: » within the first 4096 characters): the
// headers that say what its body is — its type, boundary and all, how it is
// encoded, what it is called — and the body as written. Anything else, and
// a message whose head does not parse or whose boundary is this file's own,
// is not one: it travels as text.
func messageOf(userData []byte) (head string, body []byte, ok bool) {
	if !bytes.Contains(bytes.ToLower(userData[:min(len(userData), 4096)]), []byte("mime-version:")) {
		return "", nil, false
	}
	msg, err := mail.ReadMessage(bytes.NewReader(bytes.TrimLeft(userData, " \t\r\n")))
	if err != nil {
		return "", nil, false
	}
	kind := msg.Header.Get("Content-Type")
	if kind == "" {
		kind = "text/plain"
	}
	if _, params, err := mime.ParseMediaType(kind); err != nil || params["boundary"] == firstBootBoundary {
		return "", nil, false
	}
	head = "Content-Type: " + kind + "\r\n"
	for _, h := range []string{"Content-Transfer-Encoding", "Content-Disposition"} {
		if v := msg.Header.Get(h); v != "" {
			head += h + ": " + v + "\r\n"
		}
	}
	if body, err = io.ReadAll(msg.Body); err != nil {
		return "", nil, false
	}
	return head, body, true
}
