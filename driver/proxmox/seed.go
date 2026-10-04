package proxmox

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// A VM's first boot reads its user data from a NoCloud seed: a disc labelled
// "cidata" holding meta-data, user-data and — where the zone gives the VM its
// address — network-config (cloud-init's NoCloud datasource). Proxmox builds
// such a disc itself only from a user data file on a node's snippets storage,
// which its API cannot write; it CAN take an uploaded ISO. So the driver
// writes the disc: this
// file is the smallest ISO 9660 writer that does it — one root directory,
// a handful of files, and a Joliet tree, because ISO 9660's own names cannot
// hold the dash in "user-data" and Linux reads the Joliet names when they are
// there.

const sector = 2048

// seedFile is one file on the disc.
type seedFile struct {
	name string // as the guest reads it: "user-data"
	data []byte
}

// seedNet is the network a VM's first boot is told: the address its zone
// gave it, on the card that carries this MAC.
type seedNet struct {
	mac       string
	address   netip.Prefix
	gateway   netip.Addr // not valid: no way out
	resolvers []netip.Addr
}

// config is cloud-init's network-config, version 2. The card is matched by
// its MAC: the one match every renderer of cloud-init understands (a name's
// glob is netplan's alone), and the driver knows it — Proxmox drew it before
// the disc is written. The way out is a route to everything, the form old and
// new netplan both take.
func (n seedNet) config() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "version: 2\nethernets:\n  net0:\n    match: {macaddress: %q}\n    addresses: [%q]\n",
		strings.ToLower(n.mac), n.address.String())
	if n.gateway.IsValid() {
		fmt.Fprintf(&b, "    routes: [{to: \"0.0.0.0/0\", via: %q}]\n", n.gateway.String())
	}
	if len(n.resolvers) > 0 {
		list := make([]string, len(n.resolvers))
		for i, r := range n.resolvers {
			list[i] = fmt.Sprintf("%q", r.String())
		}
		fmt.Fprintf(&b, "    nameservers: {addresses: [%s]}\n", strings.Join(list, ", "))
	}
	return []byte(b.String())
}

// nocloudSeed builds the disc for one guest. net: the network its first boot
// is told (nil: it asks a DHCP, cloud-init's own default).
func nocloudSeed(id, hostname string, keys []string, userData []byte, net *seedNet, now time.Time) []byte {
	var md strings.Builder
	fmt.Fprintf(&md, "instance-id: %s\nlocal-hostname: %s\n", id, hostname)
	if len(keys) > 0 {
		md.WriteString("public-keys:\n")
		for _, k := range keys {
			fmt.Fprintf(&md, "  - %q\n", strings.TrimSpace(k))
		}
	}
	files := []seedFile{
		{name: "meta-data", data: []byte(md.String())},
		{name: "user-data", data: userData},
	}
	if net != nil {
		files = append(files, seedFile{name: "network-config", data: net.config()})
	}
	return isoImage("cidata", files, now)
}

// isoImage writes an ISO 9660 image with a Joliet tree: sectors 0–15 empty,
// the primary and the Joliet descriptors, the terminator, four path tables,
// two root directories (one per tree, pointing at the same file data), then
// the files.
func isoImage(label string, files []seedFile, now time.Time) []byte {
	const (
		pvdAt = 16
		svdAt = 17
		endAt = 18
		pvdL  = 19
		pvdM  = 20
		jolL  = 21
		jolM  = 22
		pvdDr = 23
		jolDr = 24
		first = 25
	)
	files = append([]seedFile(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })

	at := make([]int, len(files))
	next := first
	for i, f := range files {
		at[i] = next
		next += (len(f.data) + sector - 1) / sector
		if len(f.data) == 0 {
			next++ // an empty file still points somewhere of its own
		}
	}
	total := next
	img := make([]byte, total*sector)
	put := func(s int, b []byte) { copy(img[s*sector:], b) }

	date := recDate(now)
	// the primary tree: level-1 names (8.3, d-characters) nobody will read
	var pvdEntries, jolEntries []dirEntry
	for i, f := range files {
		pvdEntries = append(pvdEntries, dirEntry{id: []byte(level1Name(f.name)), at: at[i], size: len(f.data)})
		jolEntries = append(jolEntries, dirEntry{id: ucs2(f.name), at: at[i], size: len(f.data)})
	}
	sort.Slice(pvdEntries, func(i, j int) bool { return bytes.Compare(pvdEntries[i].id, pvdEntries[j].id) < 0 })
	sort.Slice(jolEntries, func(i, j int) bool { return bytes.Compare(jolEntries[i].id, jolEntries[j].id) < 0 })
	put(pvdDr, directory(pvdDr, pvdEntries, date))
	put(jolDr, directory(jolDr, jolEntries, date))
	for i, f := range files {
		put(at[i], f.data)
	}

	pt := func(loc int, bigEndian bool) []byte {
		b := make([]byte, 10)
		b[0] = 1 // the root's identifier is one byte
		if bigEndian {
			binary.BigEndian.PutUint32(b[2:], uint32(loc))
			binary.BigEndian.PutUint16(b[6:], 1)
		} else {
			binary.LittleEndian.PutUint32(b[2:], uint32(loc))
			binary.LittleEndian.PutUint16(b[6:], 1)
		}
		return b
	}
	put(pvdL, pt(pvdDr, false))
	put(pvdM, pt(pvdDr, true))
	put(jolL, pt(jolDr, false))
	put(jolM, pt(jolDr, true))

	put(pvdAt, volumeDescriptor(1, total, pvdL, pvdM, record([]byte{0}, pvdDr, sector, true, date), label, false))
	put(svdAt, volumeDescriptor(2, total, jolL, jolM, record([]byte{0}, jolDr, sector, true, date), label, true))
	end := make([]byte, sector)
	end[0] = 255
	copy(end[1:], "CD001")
	end[6] = 1
	put(endAt, end)
	return img
}

type dirEntry struct {
	id   []byte
	at   int
	size int
}

// directory is one root directory's sector: ".", "..", then the files.
func directory(self int, entries []dirEntry, date [7]byte) []byte {
	var b bytes.Buffer
	b.Write(record([]byte{0}, self, sector, true, date))
	b.Write(record([]byte{1}, self, sector, true, date))
	for _, e := range entries {
		b.Write(record(e.id, e.at, e.size, false, date))
	}
	return b.Bytes()
}

// record is a directory record (ECMA-119 9.1).
func record(id []byte, at, size int, dir bool, date [7]byte) []byte {
	n := 33 + len(id)
	if len(id)%2 == 0 {
		n++ // padding keeps each record's length even
	}
	b := make([]byte, n)
	b[0] = byte(n)
	both32(b[2:], uint32(at))
	both32(b[10:], uint32(size))
	copy(b[18:25], date[:])
	if dir {
		b[25] = 2
	}
	both16(b[28:], 1)
	b[32] = byte(len(id))
	copy(b[33:], id)
	return b
}

// volumeDescriptor is the primary (1) or the Joliet supplementary (2) one.
func volumeDescriptor(kind byte, total, ptL, ptM int, root []byte, label string, joliet bool) []byte {
	b := make([]byte, sector)
	b[0] = kind
	copy(b[1:], "CD001")
	b[6] = 1
	text := func(off, size int, s string) {
		if joliet {
			fill := ucs2(s)
			for len(fill)+2 <= size {
				fill = append(fill, 0, ' ')
			}
			copy(b[off:off+size], fill)
			return
		}
		copy(b[off:off+size], fmt.Sprintf("%-*s", size, s))
	}
	text(8, 32, "")
	text(40, 32, label)
	both32(b[80:], uint32(total))
	if joliet {
		copy(b[88:], "%/E") // UCS-2 level 3
	}
	both16(b[120:], 1)
	both16(b[124:], 1)
	both16(b[128:], sector)
	both32(b[132:], 10)
	binary.LittleEndian.PutUint32(b[140:], uint32(ptL))
	binary.BigEndian.PutUint32(b[148:], uint32(ptM))
	copy(b[156:190], root)
	for _, f := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
		if joliet && f[1]%2 == 0 {
			text(f[0], f[1], "")
		} else {
			copy(b[f[0]:f[0]+f[1]], bytes.Repeat([]byte{' '}, f[1]))
		}
	}
	for _, off := range []int{813, 830, 847, 864} {
		copy(b[off:], "0000000000000000") // "not specified", and a 0 offset
	}
	b[881] = 1
	return b
}

// level1Name makes an ISO 9660 level-1 name: at most eight d-characters, a
// dot, no extension, the version ";1".
func level1Name(s string) string {
	var out []rune
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
		if len(out) == 8 {
			break
		}
	}
	return string(out) + ".;1"
}

func ucs2(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u>>8), byte(u))
	}
	return b
}

func recDate(t time.Time) [7]byte {
	t = t.UTC()
	return [7]byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}
