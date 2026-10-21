package bridge

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eleza-me/telegram-bridge-bot/internal/config"
	"github.com/eleza-me/telegram-bridge-bot/internal/convert"
	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
)

// ErrNothing: the content has nothing this target can show.
var ErrNothing = errors.New("nothing to publish")

// EliStep is one post to create. Media holds indexes into Content.Media and
// is filled with media ids just before the call.
type EliStep struct {
	Post       eleza.PostIn
	Media      []int
	UploadKind string // image/video per item is decided by eliUploadKind; audio_clip / video_note here
	ReplyFirst bool   // a reply under the first post of the plan
}

// eliUploadKind is the upload kind an item takes on Eli ("" = not on Eli).
func eliUploadKind(it MediaItem) string {
	switch it.Kind {
	case KPhoto:
		return "image"
	case KVideo:
		return "video"
	case KGIF:
		if it.Mime == "image/gif" {
			return "image"
		}
		return "video" // Eli has no top-level GIF post: the MP4 plays as a video
	case KVideoNote:
		return "video_note"
	case KVoice, KAudio:
		if it.Duration > 0 {
			return "audio_clip"
		}
	case KFile:
		switch {
		case strings.HasPrefix(it.Mime, "image/"):
			return "image"
		case strings.HasPrefix(it.Mime, "video/"):
			return "video"
		}
	}
	return ""
}

// PlanEli decides the posts for c: one post (text, thread, album, video
// note, audio or poll), more posts when there is more media than one post
// carries, and replies under the first post for text that did not fit.
func PlanEli(c Content, lim config.Limits, pollDuration string) ([]EliStep, []string, error) {
	skipped := append([]string(nil), c.Skipped...)
	parts := convert.SplitPlain(c.Text.Append(c.Source).Plain(), lim.EliText)

	if c.Poll != nil {
		if len(c.Poll.Options) > lim.EliPollOptions {
			return nil, nil, fmt.Errorf("نظرسنجی الی حداکثر %d گزینه دارد و این یکی %d گزینه دارد", lim.EliPollOptions, len(c.Poll.Options))
		}
		p := &eleza.Poll{Question: convert.Truncate(c.Poll.Question, lim.EliText), Duration: pollDuration}
		for _, o := range c.Poll.Options {
			p.Options = append(p.Options, convert.Truncate(o.Text, lim.EliText))
		}
		steps := []EliStep{{Post: eleza.PostIn{Type: "poll", Poll: p}}}
		for _, t := range parts {
			steps = append(steps, EliStep{Post: eleza.PostIn{Type: "text", Text: t}, ReplyFirst: true})
		}
		return steps, skipped, nil
	}

	var main []EliStep
	var visual []int
	flush := func() {
		for len(visual) > 0 {
			n := min(len(visual), lim.EliAlbum)
			main = append(main, EliStep{Post: eleza.PostIn{Type: "media"}, Media: visual[:n:n]})
			visual = visual[n:]
		}
	}
	for i, it := range c.Media {
		switch k := eliUploadKind(it); k {
		case "image", "video":
			visual = append(visual, i)
		case "video_note":
			flush()
			main = append(main, EliStep{Post: eleza.PostIn{Type: "video_note"}, Media: []int{i}, UploadKind: k})
		case "audio_clip":
			flush()
			main = append(main, EliStep{Post: eleza.PostIn{Type: "audio"}, Media: []int{i}, UploadKind: k})
		default:
			skipped = append(skipped, describe(it))
		}
	}
	flush()

	if len(main) == 0 {
		if c.Location != nil {
			parts = append(parts, fmt.Sprintf("📍 https://www.openstreetmap.org/?mlat=%.6f&mlon=%.6f", c.Location.Lat, c.Location.Lng))
		}
		switch {
		case len(parts) == 0:
			return nil, skipped, ErrNothing
		case len(parts) == 1:
			return []EliStep{{Post: eleza.PostIn{Type: "text", Text: parts[0]}}}, skipped, nil
		}
		n := min(len(parts), lim.EliThreadParts)
		steps := []EliStep{{Post: eleza.PostIn{Type: "thread", Text: parts[0], ThreadParts: parts[1:n]}}}
		for _, t := range parts[n:] {
			steps = append(steps, EliStep{Post: eleza.PostIn{Type: "text", Text: t}, ReplyFirst: true})
		}
		return steps, skipped, nil
	}

	// Media posts: the first carries the caption; a reply may carry only one
	// photo, so further media are posts of their own.
	if len(parts) > 0 {
		main[0].Post.Text = parts[0]
		parts = parts[1:]
	}
	for _, t := range parts {
		main = append(main, EliStep{Post: eleza.PostIn{Type: "text", Text: t}, ReplyFirst: true})
	}
	return main, skipped, nil
}

// ChatStep is one message to send to Saved Messages.
type ChatStep struct {
	Msg   eleza.MessageIn
	Media int // index into Content.Media, -1 for none
}

// chatUploadKind is the upload kind of a chat message kind.
func chatUploadKind(kind string) string {
	if kind == KPhoto {
		return "image"
	}
	return kind
}

// albumClass groups the kinds that may share one chat album.
func albumClass(kind string) string {
	switch kind {
	case KPhoto, KVideo:
		return "visual"
	case KFile, KAudio:
		return kind
	}
	return ""
}

// PlanChat decides the messages for c. Media go first (as albums where they
// can), with the text as the caption of the first when it fits; otherwise the
// text follows as messages of its own.
func PlanChat(c Content, lim config.Limits) ([]ChatStep, []string, error) {
	skipped := append([]string(nil), c.Skipped...)
	text := c.Text
	if c.Poll != nil {
		var b strings.Builder
		b.WriteString("📊 " + c.Poll.Question)
		for _, o := range c.Poll.Options {
			b.WriteString("\n• " + o.Text)
		}
		text = text.Append(b.String())
	}
	text = text.Append(c.Source)

	var steps []ChatStep
	if c.Location != nil {
		steps = append(steps, ChatStep{Msg: eleza.MessageIn{Kind: "location", Location: c.Location}, Media: -1})
	}
	captioned := false
	for i := 0; i < len(c.Media); {
		it := c.Media[i]
		cls := albumClass(it.Kind)
		j := i + 1
		for cls != "" && j < len(c.Media) && j-i < lim.ChatAlbum && albumClass(c.Media[j].Kind) == cls {
			j++
		}
		album := ""
		if j-i > 1 {
			album = eleza.UUIDFrom(fmt.Sprintf("%s:album:%d", c.Key, i))
		}
		for k := i; k < j; k++ {
			m := eleza.MessageIn{Kind: c.Media[k].Kind, Album: album}
			if c.Media[k].Kind == KAudio && (c.Media[k].Title != "" || c.Media[k].Performer != "") {
				m.Audio = &eleza.AudioMeta{Title: c.Media[k].Title, Performer: c.Media[k].Performer}
			}
			if !captioned && k == i && i == 0 && !text.Empty() && len([]rune(text.Text)) <= lim.ChatCaption && captionKind(m.Kind) {
				m.Text, m.Entities = text.Text, capSpans(text.Entities)
				captioned = true
			}
			steps = append(steps, ChatStep{Msg: m, Media: k})
		}
		i = j
	}
	if !captioned && !text.Empty() {
		for _, p := range convert.Split(text, lim.ChatText) {
			steps = append(steps, ChatStep{Msg: eleza.MessageIn{Kind: "text", Text: p.Text, Entities: capSpans(p.Entities)}, Media: -1})
		}
	}
	if len(steps) == 0 {
		return nil, skipped, ErrNothing
	}
	return steps, skipped, nil
}

// maxSpans is messaging.max_entities: a message with more is refused, so the
// rest of the styling is dropped (the text stays whole).
const maxSpans = 100

func capSpans(e []eleza.Entity) []eleza.Entity {
	if len(e) > maxSpans {
		return e[:maxSpans]
	}
	return e
}

// captionKind: kinds whose messages show a caption.
func captionKind(kind string) bool {
	switch kind {
	case KPhoto, KVideo, KGIF, KFile, KAudio, KVoice:
		return true
	}
	return false
}

func describe(it MediaItem) string {
	switch it.Kind {
	case KFile:
		if it.FileName != "" {
			return "فایل " + filepath.Base(it.FileName)
		}
		return "فایل"
	case KVoice:
		return "ویس"
	case KAudio:
		return "آهنگ"
	}
	return it.Kind
}
