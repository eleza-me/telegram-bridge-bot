// Package bridge is the bot: it takes what the owner sends or forwards on
// Telegram and publishes it on Eleza, as an Eli post or into Saved Messages.
package bridge

import (
	"fmt"
	"sort"
	"strings"

	"github.com/eleza-me/telegram-bridge-bot/internal/convert"
	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

// Media kinds of a Content, named as Eleza chat messages name them.
const (
	KPhoto     = "photo"
	KVideo     = "video"
	KGIF       = "gif"
	KVideoNote = "video_note"
	KVoice     = "voice"
	KAudio     = "audio"
	KFile      = "file"
)

// MediaItem is one Telegram file to bridge.
type MediaItem struct {
	Kind      string
	FileID    string
	Size      int64
	Mime      string
	FileName  string
	Duration  int // seconds
	Title     string
	Performer string
}

// Content is one thing the owner sent: a message, or an album of them.
type Content struct {
	Key      string // stable id: chat and first message id
	Text     convert.Rich
	Media    []MediaItem
	Poll     *telegram.Poll
	Location *eleza.Location
	Source   string   // "↪️ …" line of a forward ("" when off or not forwarded)
	Skipped  []string // parts that cannot be bridged
	FirstMsg int64
}

// Empty reports whether there is nothing to publish.
func (c Content) Empty() bool {
	return c.Text.Empty() && len(c.Media) == 0 && c.Poll == nil && c.Location == nil
}

// BuildContent turns messages (one, or the items of an album) into Content.
func BuildContent(msgs []*telegram.Message, withSource bool) Content {
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].MessageID < msgs[j].MessageID })
	c := Content{}
	if len(msgs) == 0 {
		return c
	}
	first := msgs[0]
	c.Key = fmt.Sprintf("%d:%d", first.Chat.ID, first.MessageID)
	c.FirstMsg = first.MessageID
	for _, m := range msgs {
		if c.Text.Empty() {
			switch {
			case m.Text != "":
				c.Text = convert.FromTelegram(m.Text, m.Entities)
			case m.Caption != "":
				c.Text = convert.FromTelegram(m.Caption, m.CaptionEntities)
			}
		}
		if withSource && c.Source == "" && m.ForwardOrigin != nil {
			c.Source = SourceLine(m.ForwardOrigin)
		}
		addMedia(&c, m)
	}
	return c
}

func addMedia(c *Content, m *telegram.Message) {
	switch {
	case len(m.Photo) > 0:
		best := m.Photo[0]
		for _, p := range m.Photo[1:] {
			if p.Width*p.Height > best.Width*best.Height {
				best = p
			}
		}
		c.Media = append(c.Media, MediaItem{Kind: KPhoto, FileID: best.FileID, Size: best.FileSize, Mime: "image/jpeg"})
	case m.Animation != nil:
		// A Telegram GIF is (almost always) a silent MP4.
		c.Media = append(c.Media, fileItem(KGIF, m.Animation, "video/mp4"))
	case m.Video != nil:
		c.Media = append(c.Media, fileItem(KVideo, m.Video, "video/mp4"))
	case m.VideoNote != nil:
		c.Media = append(c.Media, fileItem(KVideoNote, m.VideoNote, "video/mp4"))
	case m.Voice != nil:
		c.Media = append(c.Media, fileItem(KVoice, m.Voice, "audio/ogg"))
	case m.Audio != nil:
		it := fileItem(KAudio, &m.Audio.FileBase, "audio/mpeg")
		it.Title, it.Performer = m.Audio.Title, m.Audio.Performer
		c.Media = append(c.Media, it)
	case m.Document != nil:
		c.Media = append(c.Media, fileItem(KFile, m.Document, "application/octet-stream"))
	case m.Poll != nil:
		c.Poll = m.Poll
	case m.Venue != nil:
		c.Location = &eleza.Location{Lat: m.Venue.Location.Latitude, Lng: m.Venue.Location.Longitude, Label: m.Venue.Title, Address: m.Venue.Address}
	case m.Location != nil:
		c.Location = &eleza.Location{Lat: m.Location.Latitude, Lng: m.Location.Longitude}
	case m.Sticker != nil:
		c.Skipped = append(c.Skipped, "استیکر")
	case m.Contact != nil:
		name := strings.TrimSpace(m.Contact.FirstName + " " + m.Contact.LastName)
		c.Text = c.Text.Append("👤 " + name + "\n" + m.Contact.PhoneNumber)
	}
}

func fileItem(kind string, f *telegram.FileBase, defMime string) MediaItem {
	mime := strings.ToLower(strings.TrimSpace(strings.Split(f.MimeType, ";")[0]))
	if mime == "" {
		mime = defMime
	}
	return MediaItem{Kind: kind, FileID: f.FileID, Size: f.FileSize, Mime: mime, FileName: f.FileName, Duration: f.Duration}
}

// SourceLine names where a forwarded message comes from, with a link to the
// original when the channel or group is public.
func SourceLine(o *telegram.MessageOrigin) string {
	switch o.Type {
	case "channel", "chat":
		ch := o.Chat
		if ch == nil {
			ch = o.SenderChat
		}
		if ch == nil {
			return ""
		}
		line := "↪️ از «" + ch.Name() + "»"
		if ch.Username != "" {
			if o.MessageID != 0 {
				line += "\nhttps://t.me/" + ch.Username + "/" + fmt.Sprint(o.MessageID)
			} else {
				line += "\nhttps://t.me/" + ch.Username
			}
		}
		return line
	case "user":
		if o.SenderUser == nil {
			return ""
		}
		line := "↪️ از " + o.SenderUser.Name()
		if o.SenderUser.Username != "" {
			line += " (t.me/" + o.SenderUser.Username + ")"
		}
		return line
	case "hidden_user":
		if o.SenderUserName == "" {
			return ""
		}
		return "↪️ از " + o.SenderUserName
	}
	return ""
}
