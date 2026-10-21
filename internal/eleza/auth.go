package eleza

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/eleza-me/telegram-bridge-bot/internal/store"
)

// Challenge is the answer of a code request.
type Challenge struct {
	ID          string `json:"challenge_id"`
	CodeLength  int    `json:"code_length"`
	ExpiresIn   int    `json:"expires_in"`
	ResendAfter int    `json:"resend_after"`
}

// TwoStep is the second login step the account asks for.
type TwoStep struct {
	Ticket        string
	Hint          string
	RecoveryEmail string
}

// Login is the outcome of a code check: done (TwoStep nil) or a password owed.
type Login struct {
	TwoStep *TwoStep
	Handle  string
	IsNew   bool
}

type tokensOut struct {
	AccessToken     string     `json:"access_token"`
	AccessExp       *time.Time `json:"access_expires_at"`
	RefreshToken    string     `json:"refresh_token"`
	Nonce           string     `json:"device_nonce"`
	UserID          string     `json:"user_id"`
	IsNew           bool       `json:"is_new_user"`
	TwoStepRequired bool       `json:"two_step_required"`
	Ticket          string     `json:"ticket"`
	Hint            string     `json:"hint"`
	RecoveryEmail   string     `json:"recovery_email"`
}

func (c *Client) deviceID() (string, error) {
	if id := c.Store.Get().DeviceID; id != "" {
		return id, nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := "tgb-" + hex.EncodeToString(b)
	return id, c.Store.Update(func(s *store.State) { s.DeviceID = id })
}

// RequestCode sends the login SMS to phone (any form the server accepts:
// +98…, 0912…).
func (c *Client) RequestCode(ctx context.Context, phone string) (Challenge, error) {
	dev, err := c.deviceID()
	if err != nil {
		return Challenge{}, err
	}
	var ch Challenge
	err = c.raw(ctx, http.MethodPost, "/v1/auth/otp/request", map[string]string{
		"phone": phone, "device_id": dev, "platform": "desktop",
	}, &ch, "", reqOpts{})
	return ch, err
}

// VerifyCode checks the SMS code. A new device key is made for this login.
func (c *Client) VerifyCode(ctx context.Context, challengeID, code string) (Login, error) {
	dev, err := c.deviceID()
	if err != nil {
		return Login{}, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Login{}, err
	}
	var out tokensOut
	err = c.raw(ctx, http.MethodPost, "/v1/auth/otp/verify", map[string]any{
		"challenge_id": challengeID,
		"code":         code,
		"device_id":    dev,
		"device_name":  c.DeviceName,
		"platform":     "desktop",
		"app_version":  c.AppVersion,
		"device_key":   map[string]string{"type": "ed25519", "key": base64.StdEncoding.EncodeToString(pub)},
	}, &out, "", reqOpts{})
	if err != nil {
		return Login{}, err
	}
	if out.TwoStepRequired {
		c.pendingMu.Lock()
		c.pendingKey = priv
		c.pendingMu.Unlock()
		return Login{TwoStep: &TwoStep{Ticket: out.Ticket, Hint: out.Hint, RecoveryEmail: out.RecoveryEmail}}, nil
	}
	return c.finishLogin(ctx, out, priv)
}

// SubmitPassword completes a login that asked for the two-step password.
func (c *Client) SubmitPassword(ctx context.Context, ticket, password string) (Login, error) {
	c.pendingMu.Lock()
	priv := c.pendingKey
	c.pendingMu.Unlock()
	if priv == nil {
		return Login{}, errors.New("eleza: no login in progress")
	}
	dev, err := c.deviceID()
	if err != nil {
		return Login{}, err
	}
	var out tokensOut
	if err := c.raw(ctx, http.MethodPost, "/v1/auth/two-step", map[string]string{
		"ticket": ticket, "password": password, "device_id": dev,
	}, &out, "", reqOpts{}); err != nil {
		return Login{}, err
	}
	return c.finishLogin(ctx, out, priv)
}

func (c *Client) finishLogin(ctx context.Context, out tokensOut, priv ed25519.PrivateKey) (Login, error) {
	if out.RefreshToken == "" || out.AccessToken == "" {
		return Login{}, errors.New("eleza: login answer without tokens")
	}
	now := c.Now()
	if err := c.Store.Update(func(s *store.State) {
		s.ClearSession()
		s.DeviceKey = priv
		s.UserID = out.UserID
		s.LoggedInAt = now
		c.applyTokens(s, out)
	}); err != nil {
		return Login{}, err
	}
	c.pendingMu.Lock()
	c.pendingKey = nil
	c.pendingMu.Unlock()
	l := Login{IsNew: out.IsNew}
	if me, err := c.Me(ctx); err == nil {
		l.Handle = me.Handle
		_ = c.Store.Update(func(s *store.State) {
			s.Handle = me.Handle
			if s.UserID == "" {
				s.UserID = me.ID
			}
		})
	}
	return l, nil
}

func (c *Client) applyTokens(s *store.State, out tokensOut) {
	s.AccessToken = out.AccessToken
	if out.AccessExp != nil {
		s.AccessExp = *out.AccessExp
	} else {
		s.AccessExp = c.Now().Add(9 * time.Minute)
	}
	if out.RefreshToken != "" {
		s.RefreshToken = out.RefreshToken
	}
	if out.Nonce != "" {
		s.Nonce = out.Nonce
	}
}

// Logout ends this device's session on the server and forgets it here.
func (c *Client) Logout(ctx context.Context) error {
	var serverErr error
	if c.Store.Get().LoggedIn() {
		serverErr = c.call(ctx, http.MethodPost, "/v1/auth/logout", nil, nil, reqOpts{auth: true})
		if errors.Is(serverErr, ErrNotLoggedIn) {
			serverErr = nil
		}
	}
	if err := c.Store.Update(func(s *store.State) { s.ClearSession() }); err != nil {
		return err
	}
	return serverErr
}

// sessionEnded: the codes after which only a new login helps.
func sessionEnded(err error) bool {
	e, ok := AsAPIError(err)
	if !ok || e.Status != http.StatusUnauthorized {
		return false
	}
	return strings.HasPrefix(e.Code, "auth.")
}

// accessToken returns a valid access token, refreshing when it is about to
// expire (or always, with force).
func (c *Client) accessToken(ctx context.Context, force bool) (string, error) {
	st := c.Store.Get()
	if !st.LoggedIn() {
		return "", ErrNotLoggedIn
	}
	if !force && st.AccessToken != "" && c.Now().Add(time.Minute).Before(st.AccessExp) {
		return st.AccessToken, nil
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	st2 := c.Store.Get()
	if !st2.LoggedIn() {
		return "", ErrNotLoggedIn
	}
	if st2.AccessToken != st.AccessToken && st2.AccessToken != "" {
		return st2.AccessToken, nil // another caller refreshed meanwhile
	}
	out, err := c.refresh(ctx, st2)
	if err != nil {
		if sessionEnded(err) {
			code := "auth.session_ended"
			if e, ok := AsAPIError(err); ok {
				code = e.Code
			}
			c.Log.Warn("eleza: session ended", "code", code)
			_ = c.Store.Update(func(s *store.State) { s.ClearSession() })
			if c.OnLoggedOut != nil {
				c.OnLoggedOut(code)
			}
			return "", errors.Join(ErrNotLoggedIn, err)
		}
		return "", err
	}
	if err := c.Store.Update(func(s *store.State) { c.applyTokens(s, out) }); err != nil {
		return "", err
	}
	return out.AccessToken, nil
}

func (c *Client) refresh(ctx context.Context, st store.State) (tokensOut, error) {
	if len(st.DeviceKey) != ed25519.PrivateKeySize {
		return tokensOut{}, &APIError{Status: http.StatusUnauthorized, Code: "auth.device_key_missing", Title: "no device key stored"}
	}
	nonce := st.Nonce
	if nonce == "" {
		n, err := c.reissueNonce(ctx, st.RefreshToken)
		if err != nil {
			return tokensOut{}, err
		}
		nonce = n
	}
	out, err := c.refreshOnce(ctx, st, nonce)
	if IsCode(err, "auth.device_proof_invalid") {
		// The nonce expired (the bridge was idle or down): ask for a new one.
		n, nerr := c.reissueNonce(ctx, st.RefreshToken)
		if nerr != nil {
			return tokensOut{}, nerr
		}
		out, err = c.refreshOnce(ctx, st, n)
	}
	return out, err
}

func (c *Client) refreshOnce(ctx context.Context, st store.State, nonce string) (tokensOut, error) {
	raw, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return tokensOut{}, err
	}
	sig := ed25519.Sign(ed25519.PrivateKey(st.DeviceKey), ProofMessage("refresh", raw))
	body := map[string]string{
		"refresh_token": st.RefreshToken,
		"device_nonce":  nonce,
		"signature":     base64.StdEncoding.EncodeToString(sig),
	}
	var out tokensOut
	err = c.raw(ctx, http.MethodPost, "/v1/auth/token/refresh", body, &out, "", reqOpts{timeout: 30 * time.Second})
	var te *transportError
	if errors.As(err, &te) {
		// The server may have rotated already: the same request once more is
		// answered with the same child within the replay window.
		err = c.raw(ctx, http.MethodPost, "/v1/auth/token/refresh", body, &out, "", reqOpts{timeout: 30 * time.Second})
	}
	return out, err
}

func (c *Client) reissueNonce(ctx context.Context, refresh string) (string, error) {
	var out struct {
		Nonce string `json:"device_nonce"`
	}
	if err := c.raw(ctx, http.MethodPost, "/v1/auth/nonce", map[string]string{"refresh_token": refresh}, &out, "", reqOpts{timeout: 30 * time.Second}); err != nil {
		return "", err
	}
	if err := c.Store.Update(func(s *store.State) { s.Nonce = out.Nonce }); err != nil {
		return "", err
	}
	return out.Nonce, nil
}

// ProofMessage is the byte string a device signs (backend pkg/auth).
func ProofMessage(purpose string, nonce []byte) []byte {
	m := append([]byte("eleza/device-proof/v1\x00"), purpose...)
	m = append(m, 0)
	return append(m, nonce...)
}
