package convert

import (
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

func spanText(r Rich, e eleza.Entity) string {
	u := utf16.Encode([]rune(r.Text))
	return string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
}

func TestFromTelegramKeepsKnownSpans(t *testing.T) {
	text := "😀 سلام bold link"
	// 😀 is two UTF-16 units: "سلام" starts at 3.
	ents := []telegram.Entity{
		{Type: "bold", Offset: 8, Length: 4},
		{Type: "text_link", Offset: 13, Length: 4, URL: "https://eleza.me"},
		{Type: "text_link", Offset: 13, Length: 4, URL: "tg://user?id=1"}, // not http(s)
		{Type: "hashtag", Offset: 0, Length: 2},
		{Type: "italic", Offset: 15, Length: 10}, // out of range
	}
	r := FromTelegram(text, ents)
	if len(r.Entities) != 2 {
		t.Fatalf("entities %+v", r.Entities)
	}
	if got := spanText(r, r.Entities[0]); got != "bold" {
		t.Fatalf("bold span %q", got)
	}
	if got := spanText(r, r.Entities[1]); got != "link" || r.Entities[1].URL != "https://eleza.me" {
		t.Fatalf("link span %q", got)
	}
}

func TestPlainShowsHiddenLinks(t *testing.T) {
	r := Rich{Text: "😀 read this and https://a.example", Entities: []eleza.Entity{
		{Type: "text_link", Offset: 3, Length: 4, URL: "https://b.example/x"},
		{Type: "text_link", Offset: 17, Length: 17, URL: "https://a.example"}, // shown already
		{Type: "bold", Offset: 8, Length: 4},
	}}
	want := "😀 read (https://b.example/x) this and https://a.example"
	if got := r.Plain(); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestSplitRespectsLimitAndBreaks(t *testing.T) {
	para := strings.Repeat("کلمه ", 30) // 150 runes
	text := strings.TrimSpace(para) + "\n\n" + strings.TrimSpace(para) + "\n\n" + strings.TrimSpace(para)
	parts := SplitPlain(text, 200)
	if len(parts) != 3 {
		t.Fatalf("%d parts: %q", len(parts), parts)
	}
	for _, p := range parts {
		if utf8.RuneCountInString(p) > 200 || strings.TrimSpace(p) != p || p == "" {
			t.Fatalf("bad part %q", p)
		}
	}
}

func TestSplitWithoutBreaksCutsHard(t *testing.T) {
	parts := SplitPlain(strings.Repeat("a", 25), 10)
	if len(parts) != 3 || parts[0] != strings.Repeat("a", 10) || parts[2] != "aaaaa" {
		t.Fatalf("%q", parts)
	}
}

func TestSplitClipsSpans(t *testing.T) {
	// "aaaa bbbb cccc" split at 10: "aaaa bbbb" and "cccc"; a bold span over
	// "bbbb cc" must become "bbbb" + "cc".
	r := Rich{Text: "aaaa bbbb cccc", Entities: []eleza.Entity{{Type: "bold", Offset: 5, Length: 7}}}
	parts := Split(r, 10)
	if len(parts) != 2 || parts[0].Text != "aaaa bbbb" || parts[1].Text != "cccc" {
		t.Fatalf("%+v", parts)
	}
	if len(parts[0].Entities) != 1 || spanText(parts[0], parts[0].Entities[0]) != "bbbb" {
		t.Fatalf("first %+v", parts[0].Entities)
	}
	if len(parts[1].Entities) != 1 || spanText(parts[1], parts[1].Entities[0]) != "cc" {
		t.Fatalf("second %+v", parts[1].Entities)
	}
}

func TestSplitWithSurrogates(t *testing.T) {
	text := strings.Repeat("😀", 7) + " " + strings.Repeat("😀", 7)
	r := Rich{Text: text, Entities: []eleza.Entity{{Type: "italic", Offset: 0, Length: utf16Len(text)}}}
	parts := Split(r, 10)
	if len(parts) != 2 {
		t.Fatalf("%+v", parts)
	}
	for _, p := range parts {
		if spanText(p, p.Entities[0]) != p.Text {
			t.Fatalf("span does not cover piece %+v", p)
		}
	}
}

func TestAppendAndTruncate(t *testing.T) {
	if got := (Rich{Text: "hi \n"}).Append("↪️ src").Text; got != "hi\n\n↪️ src" {
		t.Fatalf("%q", got)
	}
	if got := (Rich{}).Append("x").Text; got != "x" {
		t.Fatalf("%q", got)
	}
	if got := Truncate("abcdef", 4); got != "abc…" {
		t.Fatalf("%q", got)
	}
}
