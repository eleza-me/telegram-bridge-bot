package eleza

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"

	"github.com/eleza-me/telegram-bridge-bot/internal/store"
)

// Profile is the part of GET /v1/me the bridge uses.
type Profile struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
}

// Me returns the logged-in profile.
func (c *Client) Me(ctx context.Context) (Profile, error) {
	var p Profile
	err := c.retry(ctx, "me", func() error {
		return c.call(ctx, http.MethodGet, "/v1/me", nil, &p, reqOpts{auth: true})
	})
	return p, err
}

// MediaRef is one attachment of a post.
type MediaRef struct {
	ID  string `json:"id"`
	Alt string `json:"alt,omitempty"`
}

// Poll of a post.
type Poll struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Duration string   `json:"duration"`
}

// Clip is an audio post's track.
type Clip struct {
	Source  string `json:"source"` // device | record
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Media   string `json:"media"`
	StartMs int64  `json:"start_ms"`
	EndMs   int64  `json:"end_ms"`
}

// PostIn is the body of POST /v1/posts.
type PostIn struct {
	Type        string     `json:"type"` // text | media | video_note | audio | poll | thread
	Text        string     `json:"text,omitempty"`
	ReplyTo     string     `json:"reply_to,omitempty"`
	Media       []MediaRef `json:"media,omitempty"`
	Poll        *Poll      `json:"poll,omitempty"`
	Clip        *Clip      `json:"clip,omitempty"`
	ThreadParts []string   `json:"thread_parts,omitempty"`
}

// PostOut is the created post.
type PostOut struct {
	ID string `json:"id"`
}

// PostURL is the public link of a post.
func PostURL(id string) string { return "https://eleza.me/p/" + url.PathEscape(id) }

// CreatePost publishes an Eli post as the owner. key makes it idempotent: a
// retry with the same key returns the first post instead of publishing again.
func (c *Client) CreatePost(ctx context.Context, key string, p PostIn) (PostOut, error) {
	var out PostOut
	err := c.retry(ctx, "post", func() error {
		return c.call(ctx, http.MethodPost, "/v1/posts", p, &out, reqOpts{auth: true, idem: idemKey(key)})
	})
	return out, err
}

// idemKey fits a key to the server's Idempotency-Key alphabet and length.
func idemKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("tgb.%x", sum[:20])
}

// SavedChat returns the id of the owner's Saved Messages chat.
func (c *Client) SavedChat(ctx context.Context) (string, error) {
	st := c.Store.Get()
	if st.SavedChatID != "" {
		return st.SavedChatID, nil
	}
	uid := st.UserID
	if uid == "" {
		me, err := c.Me(ctx)
		if err != nil {
			return "", err
		}
		uid = me.ID
	}
	var view struct {
		Chat string `json:"chat"`
	}
	err := c.retry(ctx, "saved-chat", func() error {
		// A thread with oneself is the Saved Messages chat.
		return c.call(ctx, http.MethodPost, "/v1/threads", map[string]string{"user_id": uid}, &view, reqOpts{auth: true})
	})
	if err != nil {
		return "", err
	}
	if view.Chat == "" {
		return "", fmt.Errorf("eleza: /v1/threads returned no chat id")
	}
	_ = c.Store.Update(func(s *store.State) { s.SavedChatID = view.Chat })
	return view.Chat, nil
}

// Entity is a styled span of a chat message (offsets in UTF-16 units).
type Entity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
}

// Location of a message.
type Location struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Label   string  `json:"label,omitempty"`
	Address string  `json:"address,omitempty"`
}

// AudioMeta of a music message.
type AudioMeta struct {
	Title     string `json:"title,omitempty"`
	Performer string `json:"performer,omitempty"`
}

// MessageIn is the body of POST /v1/chats/{id}/messages.
type MessageIn struct {
	ClientMessageID string     `json:"client_message_id"` // set by SendMessage
	Kind            string     `json:"kind"`              // text | photo | video | gif | video_note | voice | audio | file | location
	Text            string     `json:"text,omitempty"`
	Entities        []Entity   `json:"entities,omitempty"`
	Attachment      string     `json:"attachment,omitempty"`
	Album           string     `json:"album,omitempty"`
	Location        *Location  `json:"location,omitempty"`
	Audio           *AudioMeta `json:"audio,omitempty"`
	Silent          bool       `json:"silent,omitempty"`
}

// MessageOut is the sent message.
type MessageOut struct {
	MessageID string `json:"message_id"`
	Seq       int64  `json:"seq"`
	Duplicate bool   `json:"duplicate"`
}

// SendMessage sends m to chat. key makes it idempotent (client_message_id).
func (c *Client) SendMessage(ctx context.Context, chat, key string, m MessageIn) (MessageOut, error) {
	m.ClientMessageID = UUIDFrom(key)
	var out MessageOut
	err := c.retry(ctx, "message", func() error {
		return c.call(ctx, http.MethodPost, "/v1/chats/"+url.PathEscape(chat)+"/messages", m, &out, reqOpts{auth: true})
	})
	return out, err
}

// UUIDFrom derives a stable UUID (version 8 layout) from key.
func UUIDFrom(key string) string {
	h := sha256.Sum256([]byte("tgbridge/" + key))
	h[6] = h[6]&0x0f | 0x80
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
