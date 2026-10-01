package ui

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strings"
	"sync"
)

// The mark ships carrying the PRODUCT's name and nothing else. An operator's
// own house word — if they have one — arrives from their config and is
// substituted here, so the same binary shows "LE HANGAR" to a stranger and
// somebody's house to the people who run it. A product that ships somebody
// else's name on its front page has a bug, not a brand.

var (
	once sync.Once
	raw  []byte

	reWord = regexp.MustCompile(`(?s)<text class="lg-word"[^>]*>.*?</text>`)
	reAria = regexp.MustCompile(`aria-label="[^"]*"`)
)

func base() []byte {
	once.Do(func() {
		b, err := staticFS.ReadFile("static/logo-animated.svg")
		if err != nil {
			panic(err)
		}
		raw = b
	})
	return raw
}

// Lockup renders the mark. An empty house leaves the shipped one untouched.
//
// The wordmark is drawn with a fixed textLength, so a house word of any
// length fits the lockup instead of running off the edge of it.
func Lockup(house string) []byte {
	house = strings.TrimSpace(house)
	if house == "" {
		return base()
	}
	out := reWord.ReplaceAll(base(),
		[]byte(`<text class="lg-word" x="205" y="122" textLength="400" lengthAdjust="spacingAndGlyphs">`+
			tspans(strings.ToUpper(house))+`</text>`))
	out = reAria.ReplaceAll(out,
		[]byte(`aria-label="`+html.EscapeString(house)+` — le hangar"`))
	return out
}

// tspans splits a word into the per-character spans the mark animates.
func tspans(text string) string {
	var b bytes.Buffer
	for i, c := range text {
		fmt.Fprintf(&b, `<tspan style="--i:%d">%s</tspan>`, i, html.EscapeString(string(c)))
	}
	return b.String()
}

// Still renders the mark drawn whole and at rest — what a page shows as an
// image. (Lockup's draws itself, and plays whole as a document of its own;
// as an image, the browser the tests drive drew its ring and its bolt and
// never its words.) It is the same mark: the asset says how it looks without
// motion, for those who ask their browser for none, and Still asks for that
// always.
func Still(house string) []byte {
	return bytes.Replace(Lockup(house), []byte(stillFrom), []byte(stillTo), 1)
}

const (
	stillFrom = "@media (prefers-reduced-motion: reduce) {"
	stillTo   = "@media all {"
)
