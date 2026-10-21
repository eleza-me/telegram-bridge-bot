// Package convert turns Telegram text into what Eleza takes: chat messages
// keep styled spans (both sides count offsets in UTF-16 units, so spans map
// one to one), Eli posts are plain text with a character limit.
package convert

import (
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

// Rich is text with styled spans.
type Rich struct {
	Text     string
	Entities []eleza.Entity
}

// Empty reports whether there is no visible text.
func (r Rich) Empty() bool { return strings.TrimSpace(r.Text) == "" }

// span types both sides know. Telegram's url, mention, hashtag, bot_command,
// email, phone_number and cashtag are found in the text by Eleza's apps, and
// blockquote, custom_emoji and text_mention have no Eleza counterpart.
var spanTypes = map[string]string{
	"bold": "bold", "italic": "italic", "underline": "underline", "strikethrough": "strikethrough",
	"spoiler": "spoiler", "code": "code", "pre": "pre", "text_link": "text_link",
}

// FromTelegram maps a Telegram text or caption.
func FromTelegram(text string, ents []telegram.Entity) Rich {
	r := Rich{Text: text}
	n := utf16Len(text)
	for _, e := range ents {
		t, ok := spanTypes[e.Type]
		if !ok || e.Length <= 0 || e.Offset < 0 || e.Offset+e.Length > n {
			continue
		}
		out := eleza.Entity{Type: t, Offset: e.Offset, Length: e.Length}
		switch t {
		case "text_link":
			if !httpURL(e.URL) {
				continue
			}
			out.URL = e.URL
		case "pre":
			if len(e.Language) <= 32 {
				out.Language = e.Language
			}
		}
		r.Entities = append(r.Entities, out)
	}
	return r
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && len(s) <= 2048
}

// Append adds plain text after r, separated by a blank line.
func (r Rich) Append(s string) Rich {
	s = strings.TrimSpace(s)
	if s == "" {
		return r
	}
	if r.Empty() {
		return Rich{Text: s}
	}
	return Rich{Text: strings.TrimRightFunc(r.Text, unicode.IsSpace) + "\n\n" + s, Entities: r.Entities}
}

// Plain renders r for an Eli post: a link hidden behind text keeps its
// address in parentheses after the text (unless the text shows it already).
func (r Rich) Plain() string {
	type ins struct {
		at  int // UTF-16 offset
		txt string
	}
	var adds []ins
	for _, e := range r.Entities {
		if e.Type != "text_link" || strings.Contains(r.Text, e.URL) {
			continue
		}
		adds = append(adds, ins{e.Offset + e.Length, " (" + e.URL + ")"})
	}
	if len(adds) == 0 {
		return r.Text
	}
	sort.SliceStable(adds, func(i, j int) bool { return adds[i].at < adds[j].at })
	u := utf16.Encode([]rune(r.Text))
	var b strings.Builder
	prev := 0
	for _, a := range adds {
		b.WriteString(string(utf16.Decode(u[prev:a.at])))
		b.WriteString(a.txt)
		prev = a.at
	}
	b.WriteString(string(utf16.Decode(u[prev:])))
	return b.String()
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// Split cuts r into pieces of at most max characters (runes), at a paragraph,
// line, sentence or word break when there is one in the second half of the
// window. Spans are clipped to their piece. Whitespace at the cuts is dropped.
func Split(r Rich, max int) []Rich {
	runes := []rune(r.Text)
	// off[i] = UTF-16 offset of rune i; off[len] = total.
	off := make([]int, len(runes)+1)
	for i, c := range runes {
		off[i+1] = off[i] + utf16.RuneLen(c)
	}
	var out []Rich
	start := 0
	for start < len(runes) {
		for start < len(runes) && unicode.IsSpace(runes[start]) {
			start++
		}
		if start >= len(runes) {
			break
		}
		end := len(runes)
		if end-start > max {
			end = cutPoint(runes, start, start+max)
		}
		trim := end
		for trim > start && unicode.IsSpace(runes[trim-1]) {
			trim--
		}
		piece := Rich{Text: string(runes[start:trim])}
		lo, hi := off[start], off[trim]
		for _, e := range r.Entities {
			s, t := max2(e.Offset, lo), min2(e.Offset+e.Length, hi)
			if t <= s {
				continue
			}
			e.Offset, e.Length = s-lo, t-s
			piece.Entities = append(piece.Entities, e)
		}
		out = append(out, piece)
		start = end
	}
	return out
}

// cutPoint picks where a piece that may run to limit (exclusive) ends.
func cutPoint(runes []rune, start, limit int) int {
	half := start + (limit-start)/2
	try := func(match func(i int) bool) int {
		for i := limit; i > half; i-- {
			if match(i) {
				return i
			}
		}
		return -1
	}
	if i := try(func(i int) bool { return i >= 2 && runes[i-1] == '\n' && runes[i-2] == '\n' }); i > 0 {
		return i
	}
	if i := try(func(i int) bool { return runes[i-1] == '\n' }); i > 0 {
		return i
	}
	if i := try(func(i int) bool {
		return unicode.IsSpace(runes[i-1]) && i >= 2 && strings.ContainsRune(".!?؟…", runes[i-2])
	}); i > 0 {
		return i
	}
	if i := try(func(i int) bool { return unicode.IsSpace(runes[i-1]) }); i > 0 {
		return i
	}
	return limit
}

// SplitPlain is Split for text without spans.
func SplitPlain(s string, max int) []string {
	var out []string
	for _, p := range Split(Rich{Text: s}, max) {
		out = append(out, p.Text)
	}
	return out
}

// Truncate shortens s to max runes, ending with "…" when cut.
func Truncate(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	if max <= 1 {
		return string(r[:max])
	}
	return strings.TrimRightFunc(string(r[:max-1]), unicode.IsSpace) + "…"
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
