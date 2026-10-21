package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client calls the Bot API. Safe for concurrent use.
type Client struct {
	Base  string // https://api.telegram.org
	Token string
	// Local: the Bot API server runs with --local, so getFile returns a path
	// on the shared disk and there is no 20 MB download cap.
	Local bool
	HTTP  *http.Client
}

// New returns a client.
func New(base, token string, local bool) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Token: token, Local: local, HTTP: &http.Client{Timeout: 5 * time.Minute}}
}

// MaxCloudDownload is what the public Bot API lets a bot download.
const MaxCloudDownload = 20 << 20

// Error is a Bot API error.
type Error struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *Error) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/bot"+c.Token+"/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return scrub(err, c.Token)
	}
	defer res.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: %w", method, res.StatusCode, err)
	}
	if !env.OK {
		return &Error{Code: env.ErrorCode, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// scrub keeps the bot token out of error messages (they contain the URL).
func scrub(err error, token string) error {
	if token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<token>"))
}

// GetUpdates long-polls for updates from offset.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var out []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeout, "allowed_updates": []string{"message", "callback_query"},
	}, &out)
	return out, err
}

// SendOpts are optional sendMessage fields.
type SendOpts struct {
	ReplyTo int64
	Markup  any
}

// SendMessage sends plain text.
func (c *Client) SendMessage(ctx context.Context, chat int64, text string, o SendOpts) (Message, error) {
	p := map[string]any{"chat_id": chat, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if o.ReplyTo != 0 {
		p["reply_parameters"] = map[string]any{"message_id": o.ReplyTo, "allow_sending_without_reply": true}
	}
	if o.Markup != nil {
		p["reply_markup"] = o.Markup
	}
	var m Message
	err := c.call(ctx, "sendMessage", p, &m)
	return m, err
}

// EditMessage replaces a message's text (and its buttons: nil removes them).
func (c *Client) EditMessage(ctx context.Context, chat, msg int64, text string, markup *InlineKeyboard) error {
	p := map[string]any{"chat_id": chat, "message_id": msg, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if markup != nil {
		p["reply_markup"] = markup
	} else {
		p["reply_markup"] = InlineKeyboard{InlineKeyboard: [][]InlineButton{}}
	}
	err := c.call(ctx, "editMessageText", p, nil)
	var te *Error
	if errors.As(err, &te) && strings.Contains(te.Description, "message is not modified") {
		return nil
	}
	return err
}

// DeleteMessage deletes a message (the owner's code or password).
func (c *Client) DeleteMessage(ctx context.Context, chat, msg int64) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chat, "message_id": msg}, nil)
}

// AnswerCallback stops a button's spinner.
func (c *Client) AnswerCallback(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

// SetCommands sets the command menu.
func (c *Client) SetCommands(ctx context.Context, cmds []BotCommand) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

// DeleteWebhook makes long polling possible.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	return c.call(ctx, "deleteWebhook", map[string]any{}, nil)
}

// ErrTooBig: the public Bot API does not hand out files over 20 MB.
var ErrTooBig = errors.New("telegram: file is larger than the Bot API's 20 MB download limit")

// Download saves a file to dst.
func (c *Client) Download(ctx context.Context, fileID string, knownSize int64, dst string) error {
	if !c.Local && knownSize > MaxCloudDownload {
		return ErrTooBig
	}
	var f File
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		var te *Error
		if errors.As(err, &te) && strings.Contains(strings.ToLower(te.Description), "too big") {
			return ErrTooBig
		}
		return err
	}
	if f.FilePath == "" {
		return errors.New("telegram: getFile returned no path")
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if c.Local && filepath.IsAbs(f.FilePath) {
		// --local: the file is on the Bot API server's disk, shared with us.
		in, err := os.Open(f.FilePath)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(out, in)
		return err
	}
	u := c.Base + "/file/bot" + c.Token + "/" + (&url.URL{Path: f.FilePath}).EscapedPath()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return scrub(err, c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return scrub(err, c.Token)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: file download answered %d", res.StatusCode)
	}
	if _, err := io.Copy(out, res.Body); err != nil {
		return scrub(err, c.Token)
	}
	return out.Sync()
}
