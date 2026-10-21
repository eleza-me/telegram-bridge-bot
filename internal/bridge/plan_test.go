package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eleza-me/telegram-bridge-bot/internal/config"
	"github.com/eleza-me/telegram-bridge-bot/internal/convert"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

var lim = config.Limits{EliText: 500, EliThreadParts: 25, EliAlbum: 8, EliPollOptions: 4, ChatText: 4096, ChatCaption: 1024, ChatAlbum: 10}

func msg(id int64) *telegram.Message {
	return &telegram.Message{MessageID: id, Chat: telegram.Chat{ID: 42, Type: "private"}, From: &telegram.User{ID: 42}}
}

func TestBuildContentAlbumWithForward(t *testing.T) {
	a, b := msg(11), msg(10)
	a.Photo = []telegram.PhotoSize{{FileID: "small", Width: 90, Height: 90}, {FileID: "big", Width: 1280, Height: 720}}
	b.Video = &telegram.FileBase{FileID: "vid", MimeType: "video/mp4", Duration: 3}
	b.Caption = "سلام *دنیا*"
	b.CaptionEntities = []telegram.Entity{{Type: "bold", Offset: 5, Length: 6}}
	b.ForwardOrigin = &telegram.MessageOrigin{Type: "channel", Chat: &telegram.Chat{Title: "خبر", Username: "news"}, MessageID: 77}
	c := BuildContent([]*telegram.Message{a, b}, true)
	if c.Key != "42:10" || c.FirstMsg != 10 || len(c.Media) != 2 || c.Media[0].Kind != KVideo || c.Media[1].FileID != "big" {
		t.Fatalf("%+v", c)
	}
	if c.Text.Text != "سلام *دنیا*" || len(c.Text.Entities) != 1 {
		t.Fatalf("text %+v", c.Text)
	}
	if c.Source != "↪️ از «خبر»\nhttps://t.me/news/77" {
		t.Fatalf("source %q", c.Source)
	}
	if BuildContent([]*telegram.Message{b}, false).Source != "" {
		t.Fatal("source added while off")
	}
}

func TestPlanEliShortText(t *testing.T) {
	steps, _, err := PlanEli(Content{Text: convert.Rich{Text: "سلام"}}, lim, "24h")
	if err != nil || len(steps) != 1 || steps[0].Post.Type != "text" || steps[0].Post.Text != "سلام" {
		t.Fatalf("%+v %v", steps, err)
	}
}

func TestPlanEliLongTextIsAThread(t *testing.T) {
	text := strings.Repeat("یک جملهٔ نسبتاً بلند برای آزمایش. ", 60) // ~2000 runes
	steps, _, err := PlanEli(Content{Text: convert.Rich{Text: text}}, lim, "24h")
	if err != nil || len(steps) != 1 || steps[0].Post.Type != "thread" || len(steps[0].Post.ThreadParts) < 3 {
		t.Fatalf("%+v %v", steps, err)
	}
	for _, p := range append([]string{steps[0].Post.Text}, steps[0].Post.ThreadParts...) {
		if utf8.RuneCountInString(p) > 500 {
			t.Fatalf("part of %d runes", utf8.RuneCountInString(p))
		}
	}
}

func TestPlanEliMoreThanAThread(t *testing.T) {
	small := lim
	small.EliText, small.EliThreadParts = 10, 3
	steps, _, err := PlanEli(Content{Text: convert.Rich{Text: strings.Repeat("abcdefghi ", 6)}}, small, "24h")
	if err != nil || len(steps) != 4 || steps[0].Post.Type != "thread" || len(steps[0].Post.ThreadParts) != 2 {
		t.Fatalf("%+v %v", steps, err)
	}
	for _, s := range steps[1:] {
		if !s.ReplyFirst || s.Post.Type != "text" {
			t.Fatalf("%+v", s)
		}
	}
}

func TestPlanEliMedia(t *testing.T) {
	var media []MediaItem
	for i := 0; i < 10; i++ {
		media = append(media, MediaItem{Kind: KPhoto, Mime: "image/jpeg"})
	}
	media = append(media,
		MediaItem{Kind: KFile, Mime: "application/pdf", FileName: "a.pdf"},
		MediaItem{Kind: KFile, Mime: "image/png"},
		MediaItem{Kind: KVideoNote, Mime: "video/mp4"},
		MediaItem{Kind: KVoice, Mime: "audio/ogg", Duration: 4},
		MediaItem{Kind: KVoice, Mime: "audio/ogg"}, // no duration: cannot be a clip
	)
	c := Content{Text: convert.Rich{Text: "کپشن"}, Media: media}
	steps, skipped, err := PlanEli(c, lim, "24h")
	if err != nil {
		t.Fatal(err)
	}
	// 8 photos | 2 photos + png | video note | voice
	if len(steps) != 4 || len(steps[0].Media) != 8 || len(steps[1].Media) != 3 || steps[2].Post.Type != "video_note" || steps[3].Post.Type != "audio" {
		t.Fatalf("%+v", steps)
	}
	if steps[0].Post.Text != "کپشن" || steps[1].Post.Text != "" || steps[1].ReplyFirst {
		t.Fatalf("caption %+v", steps)
	}
	if len(skipped) != 2 || skipped[0] != "فایل a.pdf" {
		t.Fatalf("skipped %q", skipped)
	}
}

func TestPlanEliHiddenLinkAndSource(t *testing.T) {
	c := Content{Text: convert.FromTelegram("ببین", []telegram.Entity{{Type: "text_link", Offset: 0, Length: 4, URL: "https://x.example"}}), Source: "↪️ از «ک»"}
	steps, _, _ := PlanEli(c, lim, "24h")
	if steps[0].Post.Text != "ببین (https://x.example)\n\n↪️ از «ک»" {
		t.Fatalf("%q", steps[0].Post.Text)
	}
}

func TestPlanEliPoll(t *testing.T) {
	p := &telegram.Poll{Question: "کدام؟", Options: []telegram.PollOption{{Text: "الف"}, {Text: "ب"}}}
	steps, _, err := PlanEli(Content{Poll: p}, lim, "24h")
	if err != nil || len(steps) != 1 || steps[0].Post.Poll == nil || steps[0].Post.Poll.Duration != "24h" || len(steps[0].Post.Poll.Options) != 2 {
		t.Fatalf("%+v %v", steps, err)
	}
	p.Options = append(p.Options, telegram.PollOption{Text: "1"}, telegram.PollOption{Text: "2"}, telegram.PollOption{Text: "3"})
	if _, _, err := PlanEli(Content{Poll: p}, lim, "24h"); err == nil {
		t.Fatal("5 options accepted")
	}
}

func TestPlanEliNothing(t *testing.T) {
	if _, _, err := PlanEli(Content{Media: []MediaItem{{Kind: KFile, Mime: "application/zip"}}}, lim, "24h"); err != ErrNothing {
		t.Fatalf("%v", err)
	}
}

func TestPlanChatAlbumsAndCaption(t *testing.T) {
	c := Content{Key: "1:2", Text: convert.Rich{Text: "کپشن"}, Media: []MediaItem{
		{Kind: KPhoto}, {Kind: KVideo}, {Kind: KVoice}, {Kind: KFile}, {Kind: KFile},
	}}
	steps, _, err := PlanChat(c, lim)
	if err != nil || len(steps) != 5 {
		t.Fatalf("%+v %v", steps, err)
	}
	if steps[0].Msg.Text != "کپشن" || steps[0].Msg.Album == "" || steps[0].Msg.Album != steps[1].Msg.Album {
		t.Fatalf("first album %+v", steps[:2])
	}
	if steps[2].Msg.Album != "" || steps[3].Msg.Album == "" || steps[3].Msg.Album != steps[4].Msg.Album || steps[3].Msg.Album == steps[0].Msg.Album {
		t.Fatalf("albums %+v", steps)
	}
}

func TestPlanChatLongTextFollowsMedia(t *testing.T) {
	c := Content{Key: "1:2", Text: convert.Rich{Text: strings.Repeat("ا ", 700)}, Media: []MediaItem{{Kind: KPhoto}}}
	steps, _, _ := PlanChat(c, lim)
	if len(steps) != 2 || steps[0].Msg.Text != "" || steps[1].Msg.Kind != "text" {
		t.Fatalf("%+v", steps)
	}
}

func TestPlanChatPollAndLocation(t *testing.T) {
	c := Content{Key: "1:2", Poll: &telegram.Poll{Question: "q", Options: []telegram.PollOption{{Text: "a"}}}}
	steps, _, _ := PlanChat(c, lim)
	if len(steps) != 1 || steps[0].Msg.Text != "📊 q\n• a" {
		t.Fatalf("%+v", steps)
	}
	c = BuildContent([]*telegram.Message{func() *telegram.Message {
		m := msg(5)
		m.Location = &telegram.Location{Latitude: 35.7, Longitude: 51.4}
		return m
	}()}, false)
	steps, _, _ = PlanChat(c, lim)
	if len(steps) != 1 || steps[0].Msg.Kind != "location" || steps[0].Msg.Location.Lat != 35.7 {
		t.Fatalf("%+v", steps)
	}
}

func TestNormalizePhoneAndDigits(t *testing.T) {
	cases := map[string]string{"09121234567": "09121234567", "+98 912 123 4567": "+989121234567", "۰۹۱۲۱۲۳۴۵۶۷": "09121234567", "00989121234567": "+989121234567", "abc": "", "12": ""}
	for in, want := range cases {
		if got := NormalizePhone(in, false); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if NormalizePhone("989121234567", true) != "+989121234567" {
		t.Error("contact number")
	}
	if Digits("۱۲ ۳۴۵") != "12345" || Digits("12a") != "" {
		t.Error("digits")
	}
}
