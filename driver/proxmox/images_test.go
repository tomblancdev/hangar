package proxmox

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

// A builder's user data, read back as cloud-init reads it: a multipart whose
// first part is the recipe untouched (a cloud-config stays one; a script is
// a script) and whose last is the driver's end of the bake.
func TestABuildersUserDataCarriesTheRecipeThenTheEnd(t *testing.T) {
	for _, tc := range []struct{ recipe, kind string }{
		{"#cloud-config\npackages: [qemu-guest-agent]\n# é, and a line --==hangar\n", "text/cloud-config"},
		{"#!/bin/sh\necho baked > /etc/baked\n", "text/x-shellscript"},
	} {
		msg, err := mail.ReadMessage(bytes.NewReader(bakeUserData([]byte(tc.recipe))))
		if err != nil {
			t.Fatal(err)
		}
		mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
		if err != nil || mt != "multipart/mixed" {
			t.Fatalf("%s %v", mt, err)
		}
		mr := multipart.NewReader(msg.Body, params["boundary"])
		var kinds, bodies []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(p)
			body, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(raw), "\r\n", ""))
			if err != nil {
				t.Fatal(err)
			}
			kind, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
			kinds, bodies = append(kinds, kind), append(bodies, string(body))
		}
		if len(kinds) != 2 || kinds[0] != tc.kind || bodies[0] != tc.recipe || kinds[1] != "text/x-shellscript" || bodies[1] != bakeEnd {
			t.Fatalf("parts %v", kinds)
		}
	}
	if !strings.Contains(bakeEnd, "hostname "+bakeDoneName) || !strings.Contains(bakeEnd, "hostname "+bakeFailedName) ||
		!strings.Contains(bakeEnd, "cloud-init clean") {
		t.Fatal("the end of a bake says done or failed, and cleans")
	}
}
