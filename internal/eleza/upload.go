package eleza

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// UploadIn describes a file to upload.
type UploadIn struct {
	Path        string // local file
	Purpose     string // post | chat
	Kind        string // image | video | gif | video_note | voice | audio | audio_clip | file
	Mime        string
	Filename    string
	ClipStartMs int64 // audio_clip only
	ClipEndMs   int64
}

type uploadOut struct {
	UploadID string `json:"upload_id"`
	MediaID  string `json:"media_id"`
	PartSize int64  `json:"part_size"`
	Parts    []struct {
		N      int    `json:"n"`
		Number int    `json:"number"`
		URL    string `json:"url"`
	} `json:"parts"`
	Uploaded []int  `json:"uploaded"`
	State    string `json:"state"`
}

// Upload sends a file and waits until the server has processed it. Returns
// the media id to attach.
func (c *Client) Upload(ctx context.Context, in UploadIn) (string, error) {
	f, err := os.Open(in.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if fi.Size() == 0 {
		return "", errors.New("eleza: empty file")
	}
	body := map[string]any{"purpose": in.Purpose, "kind": in.Kind, "size": fi.Size(), "mime": in.Mime}
	if in.Filename != "" {
		body["filename"] = in.Filename
	}
	if in.Kind == "audio_clip" {
		body["clip_start_ms"], body["clip_end_ms"] = in.ClipStartMs, in.ClipEndMs
	}
	var up uploadOut
	// Not retried as a whole: a lost answer would open a second session (it
	// expires on its own, and the server closes abandoned ones).
	if err := c.call(ctx, http.MethodPost, "/v1/uploads", body, &up, reqOpts{auth: true}); err != nil {
		return "", err
	}
	if up.State == "uploading" || up.State == "" {
		done := map[int]bool{}
		for _, n := range up.Uploaded {
			done[n] = true
		}
		partSize := up.PartSize
		if partSize <= 0 {
			partSize = 8 << 20
		}
		for _, p := range up.Parts {
			n := p.N
			if n == 0 {
				n = p.Number
			}
			if done[n] || n < 1 {
				continue
			}
			off := int64(n-1) * partSize
			length := min(partSize, fi.Size()-off)
			if length <= 0 {
				continue
			}
			if err := c.putPart(ctx, f, off, length, p.URL, in.Mime); err != nil {
				_ = c.call(context.WithoutCancel(ctx), http.MethodDelete, "/v1/uploads/"+url.PathEscape(up.UploadID), nil, nil, reqOpts{auth: true})
				return "", fmt.Errorf("eleza: uploading part %d: %w", n, err)
			}
		}
		err := c.retry(ctx, "upload-complete", func() error {
			return c.call(ctx, http.MethodPost, "/v1/uploads/"+url.PathEscape(up.UploadID)+"/complete", nil, nil, reqOpts{auth: true, timeout: 2 * time.Minute})
		})
		if err != nil {
			return "", err
		}
	}
	return up.MediaID, c.waitReady(ctx, up.MediaID, in.Kind)
}

func (c *Client) putPart(ctx context.Context, f *os.File, off, length int64, u, mime string) error {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			if err := c.Sleep(ctx, time.Duration(attempt*2)*time.Second); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, io.NewSectionReader(f, off, length))
		if err != nil {
			return err
		}
		req.ContentLength = length
		req.Header.Set("Content-Type", mime)
		res, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			last = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		if res.StatusCode < 300 {
			return nil
		}
		last = fmt.Errorf("storage answered %d", res.StatusCode)
		if res.StatusCode < 500 && res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusRequestTimeout {
			return last
		}
	}
	return last
}

// waitReady polls the media until it is processed. Videos take longer.
func (c *Client) waitReady(ctx context.Context, mediaID, kind string) error {
	limit := 2 * time.Minute
	if kind == "video" || kind == "video_note" || kind == "gif" || kind == "audio" || kind == "audio_clip" {
		limit = 20 * time.Minute
	}
	deadline := c.Now().Add(limit)
	delay := 500 * time.Millisecond
	for {
		var m struct {
			State string `json:"state"`
		}
		err := c.retry(ctx, "media", func() error {
			return c.call(ctx, http.MethodGet, "/v1/media/"+url.PathEscape(mediaID), nil, &m, reqOpts{auth: true})
		})
		if err != nil {
			return err
		}
		switch m.State {
		case "ready", "":
			return nil
		case "failed", "rejected", "deleted", "expired", "aborted":
			return &APIError{Status: 422, Code: "media." + m.State, Title: "the server could not process this file"}
		}
		if c.Now().After(deadline) {
			return &APIError{Status: 504, Code: "media.not_ready", Title: "processing took too long"}
		}
		if err := c.Sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, 5*time.Second)
	}
}
