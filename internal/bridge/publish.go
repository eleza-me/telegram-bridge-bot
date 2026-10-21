package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/eleza-me/telegram-bridge-bot/internal/config"
	"github.com/eleza-me/telegram-bridge-bot/internal/convert"
	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
)

// Targets.
const (
	TargetAsk   = "ask"
	TargetEli   = "eli"
	TargetSaved = "saved"
	TargetBoth  = "both"
)

// Downloader fetches a Telegram file to a local path.
type Downloader interface {
	Download(ctx context.Context, fileID string, knownSize int64, dst string) error
}

// Eleza is what the publisher needs of the Eleza client.
type Eleza interface {
	Upload(ctx context.Context, in eleza.UploadIn) (string, error)
	CreatePost(ctx context.Context, key string, p eleza.PostIn) (eleza.PostOut, error)
	SavedChat(ctx context.Context) (string, error)
	SendMessage(ctx context.Context, chat, key string, m eleza.MessageIn) (eleza.MessageOut, error)
}

// Publisher carries out the plans.
type Publisher struct {
	TG           Downloader
	EL           Eleza
	Limits       config.Limits
	PollDuration string
	TmpDir       string
	Log          *slog.Logger
}

// Result says what was published.
type Result struct {
	PostIDs []string
	Saved   int
	Skipped []string
}

// job keeps downloads of one Content for both targets.
type job struct {
	c     Content
	dir   string
	files map[int]string
}

func (p *Publisher) newJob(c Content) (*job, error) {
	if err := os.MkdirAll(p.TmpDir, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(p.TmpDir, "job-")
	if err != nil {
		return nil, err
	}
	return &job{c: c, dir: dir, files: map[int]string{}}, nil
}

func (p *Publisher) file(ctx context.Context, j *job, i int) (string, error) {
	if f, ok := j.files[i]; ok {
		return f, nil
	}
	it := j.c.Media[i]
	path := filepath.Join(j.dir, fmt.Sprintf("m%d", i))
	if err := p.TG.Download(ctx, it.FileID, it.Size, path); err != nil {
		return "", fmt.Errorf("دریافت %s از تلگرام: %w", describe(it), err)
	}
	j.files[i] = path
	return path, nil
}

// Publish sends c to target (eli, saved or both).
func (p *Publisher) Publish(ctx context.Context, c Content, target string) (Result, error) {
	j, err := p.newJob(c)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(j.dir)
	var res Result
	var errs []error
	if target == TargetEli || target == TargetBoth {
		ids, skipped, err := p.toEli(ctx, j)
		res.PostIDs, res.Skipped = ids, skipped
		if err != nil {
			errs = append(errs, fmt.Errorf("الی: %w", err))
		}
	}
	if target == TargetSaved || target == TargetBoth {
		n, skipped, err := p.toSaved(ctx, j)
		res.Saved = n
		if target == TargetSaved {
			res.Skipped = skipped
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("پیام‌های ذخیره‌شده: %w", err))
		}
	}
	return res, errors.Join(errs...)
}

func (p *Publisher) toEli(ctx context.Context, j *job) ([]string, []string, error) {
	steps, skipped, err := PlanEli(j.c, p.Limits, p.PollDuration)
	if err != nil {
		return nil, skipped, err
	}
	var ids []string
	for si, st := range steps {
		post := st.Post
		for _, mi := range st.Media {
			it := j.c.Media[mi]
			path, err := p.file(ctx, j, mi)
			if err != nil {
				return ids, skipped, err
			}
			kind := st.UploadKind
			if kind == "" {
				kind = eliUploadKind(it)
			}
			up := eleza.UploadIn{Path: path, Purpose: "post", Kind: kind, Mime: it.Mime, Filename: it.FileName}
			if kind == "audio_clip" {
				up.ClipEndMs = int64(min(it.Duration, 600)) * 1000
				up.Mime = clipMime(up.Mime)
			}
			if kind == "image" && !strings.HasPrefix(up.Mime, "image/") {
				up.Mime = "image/jpeg"
			}
			id, err := p.EL.Upload(ctx, up)
			if err != nil {
				return ids, skipped, fmt.Errorf("آپلود %s: %w", describe(it), err)
			}
			if kind == "audio_clip" {
				post.Clip = &eleza.Clip{Source: "device", Title: audioTitle(it, p.Limits.EliText), Artist: convert.Truncate(it.Performer, p.Limits.EliText),
					Media: id, StartMs: 0, EndMs: up.ClipEndMs}
				if it.Kind == KVoice {
					post.Clip.Source = "record"
				}
			} else {
				post.Media = append(post.Media, eleza.MediaRef{ID: id})
			}
		}
		if st.ReplyFirst && len(ids) > 0 {
			post.ReplyTo = ids[0]
		}
		out, err := p.EL.CreatePost(ctx, fmt.Sprintf("%s:eli:%d", j.c.Key, si), post)
		if err != nil {
			return ids, skipped, err
		}
		ids = append(ids, out.ID)
	}
	return ids, skipped, nil
}

// clipMime maps the audio types Telegram reports to the ones Eli audio
// posts take (media.allowed_mime_types audio_clip).
func clipMime(m string) string {
	switch m {
	case "audio/x-m4a", "audio/m4a":
		return "audio/mp4"
	case "audio/mp3":
		return "audio/mpeg"
	case "audio/x-wav":
		return "audio/wav"
	case "audio/x-flac":
		return "audio/flac"
	case "audio/opus", "audio/x-opus+ogg":
		return "audio/ogg"
	}
	return m
}

func audioTitle(it MediaItem, max int) string {
	t := it.Title
	if t == "" && it.FileName != "" {
		t = strings.TrimSuffix(filepath.Base(it.FileName), filepath.Ext(it.FileName))
	}
	if t == "" {
		if it.Kind == KVoice {
			t = "ویس"
		} else {
			t = "صدا"
		}
	}
	return convert.Truncate(t, max)
}

func (p *Publisher) toSaved(ctx context.Context, j *job) (int, []string, error) {
	steps, skipped, err := PlanChat(j.c, p.Limits)
	if err != nil {
		return 0, skipped, err
	}
	chat, err := p.EL.SavedChat(ctx)
	if err != nil {
		return 0, skipped, err
	}
	sent := 0
	for si, st := range steps {
		m := st.Msg
		if st.Media >= 0 {
			it := j.c.Media[st.Media]
			path, err := p.file(ctx, j, st.Media)
			if err != nil {
				return sent, skipped, err
			}
			id, err := p.EL.Upload(ctx, eleza.UploadIn{Path: path, Purpose: "chat", Kind: chatUploadKind(it.Kind), Mime: it.Mime, Filename: it.FileName})
			if err != nil {
				return sent, skipped, fmt.Errorf("آپلود %s: %w", describe(it), err)
			}
			m.Attachment = id
		}
		m.Silent = true // it is the owner's own chat: no push to their phone
		if _, err := p.EL.SendMessage(ctx, chat, fmt.Sprintf("%s:chat:%d", j.c.Key, si), m); err != nil {
			return sent, skipped, err
		}
		sent++
	}
	return sent, skipped, nil
}
