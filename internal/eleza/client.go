// Package eleza is a small client of the Eleza public API, acting as one
// logged-in device of the owner's account (platform "desktop").
//
// Auth (backend ADR-0017): a short-lived access token (Bearer) and a refresh
// token that rotates on every use. A refresh is signed by the device key
// registered at login: Ed25519 over "eleza/device-proof/v1\0refresh\0<nonce>",
// the nonce coming from the previous auth response.
package eleza

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/eleza-me/telegram-bridge-bot/internal/store"
)

// Client talks to the Eleza API. Safe for concurrent use.
type Client struct {
	Base         string
	ClientHeader string
	DeviceName   string
	AppVersion   string
	HTTP         *http.Client
	Store        *store.Store
	Log          *slog.Logger
	// OnLoggedOut is called (once per loss) when the server ends the session.
	OnLoggedOut func(code string)
	// Sleep waits between retries (tests make it instant).
	Sleep func(context.Context, time.Duration) error
	Now   func() time.Time

	refreshMu  sync.Mutex
	pendingMu  sync.Mutex
	pendingKey []byte // device key of a login that has not finished yet
}

// New returns a client with sane defaults.
func New(base string, st *store.Store) *Client {
	return &Client{
		Base:         strings.TrimRight(base, "/"),
		ClientHeader: "desktop/0.1.0 (tgbridge)",
		DeviceName:   "Telegram bridge",
		AppVersion:   "tgbridge/0.1.0",
		HTTP:         &http.Client{Timeout: 3 * time.Minute},
		Store:        st,
		Log:          slog.Default(),
		Sleep:        sleepCtx,
		Now:          time.Now,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrNotLoggedIn: there is no Eleza session (never logged in, or it ended).
var ErrNotLoggedIn = errors.New("eleza: not logged in")

// APIError is an application/problem+json answer.
type APIError struct {
	Status     int    `json:"status"`
	Code       string `json:"code"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Retryable  bool   `json:"retryable"`
	RetryAfter int    `json:"retry_after"`
}

func (e *APIError) Error() string {
	msg := e.Title
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return fmt.Sprintf("eleza %d %s: %s", e.Status, e.Code, msg)
}

// AsAPIError returns the APIError in err's chain, if any.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	ok := errors.As(err, &e)
	return e, ok
}

// IsCode reports whether err is an API error with this code.
func IsCode(err error, code string) bool {
	e, ok := AsAPIError(err)
	return ok && e.Code == code
}

// transportError marks a failure before any answer arrived (retry is safe
// for idempotent calls).
type transportError struct{ err error }

func (e *transportError) Error() string { return "eleza: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

type reqOpts struct {
	auth    bool
	idem    string // Idempotency-Key
	timeout time.Duration
}

// raw does one HTTP call and decodes the JSON answer into out.
func (c *Client) raw(ctx context.Context, method, path string, body, out any, token string, o reqOpts) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "fa")
	req.Header.Set("X-Eleza-Client", c.ClientHeader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if o.idem != "" {
		req.Header.Set("Idempotency-Key", o.idem)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &transportError{err}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return &transportError{err}
	}
	if res.StatusCode >= 300 {
		e := &APIError{Status: res.StatusCode}
		_ = json.Unmarshal(data, e)
		e.Status = res.StatusCode
		if e.Code == "" {
			e.Code = fmt.Sprintf("http_%d", res.StatusCode)
			e.Title = strings.TrimSpace(string(data))
			if len(e.Title) > 200 {
				e.Title = e.Title[:200]
			}
		}
		if res.StatusCode >= 500 {
			e.Retryable = true
		}
		return e
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("eleza: decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// call is raw with the access token and one refresh-and-retry on a 401.
func (c *Client) call(ctx context.Context, method, path string, body, out any, o reqOpts) error {
	if !o.auth {
		return c.raw(ctx, method, path, body, out, "", o)
	}
	tok, err := c.accessToken(ctx, false)
	if err != nil {
		return err
	}
	err = c.raw(ctx, method, path, body, out, tok, o)
	if e, ok := AsAPIError(err); ok && e.Status == http.StatusUnauthorized {
		// The access token was refused before it expired (key rotation, clock
		// skew): one forced refresh.
		if tok, err = c.accessToken(ctx, true); err != nil {
			return err
		}
		err = c.raw(ctx, method, path, body, out, tok, o)
	}
	return err
}

// retry runs fn until it succeeds or fails for good: rate limits wait for
// retry_after, media still being processed is waited for, outages back off.
// Only for calls that are idempotent (keys, or reads).
func (c *Client) retry(ctx context.Context, what string, fn func() error) error {
	transient, limited, notReady := 0, 0, 0
	for {
		err := fn()
		if err == nil {
			return nil
		}
		var wait time.Duration
		var te *transportError
		e, isAPI := AsAPIError(err)
		switch {
		case isAPI && e.Code == "media.not_ready":
			notReady++
			if notReady > 90 {
				return err
			}
			wait = 2 * time.Second
		case isAPI && e.Status == http.StatusTooManyRequests:
			limited++
			if limited > 3 || e.RetryAfter > 600 {
				return err
			}
			wait = time.Duration(max(e.RetryAfter, 2)) * time.Second
		case errors.As(err, &te) || (isAPI && e.Retryable):
			transient++
			if transient > 4 {
				return err
			}
			wait = time.Duration(1<<transient) * time.Second
		default:
			return err
		}
		c.Log.Info("eleza: retrying", "call", what, "wait", wait, "err", err.Error())
		if err := c.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}
