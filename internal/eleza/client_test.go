package eleza

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleza-me/telegram-bridge-bot/internal/store"
)

// fakeServer implements the slice of the Eleza API the client uses, with the
// same device-proof rules as the backend (pkg/auth, identity Refresh).
type fakeServer struct {
	t  *testing.T
	mu sync.Mutex

	twoStep    bool
	pub        ed25519.PublicKey
	refresh    string
	nonce      []byte
	nonceValid bool
	access     string
	accessExp  time.Time
	gen        int
	revoked    bool

	posts       []map[string]any
	postKeys    map[string]string
	messages    []map[string]any
	msgIDs      map[string]bool
	parts       map[string][]byte
	mediaPolls  int
	notReadyFor int // POST /v1/posts answers media.not_ready this many times
	refreshes   int
}

func newFake(t *testing.T) (*fakeServer, *httptest.Server) {
	f := &fakeServer{t: t, postKeys: map[string]string{}, msgIDs: map[string]bool{}, parts: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func problem(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "title": code, "status": status, "retryable": code == "media.not_ready"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeServer) issue() map[string]any {
	f.gen++
	f.refresh = fmt.Sprintf("v1.3.r%d", f.gen)
	f.access = fmt.Sprintf("a%d", f.gen)
	f.accessExp = time.Now().Add(10 * time.Minute)
	f.nonce = []byte(fmt.Sprintf("nonce-%d", f.gen))
	f.nonceValid = true
	return map[string]any{"access_token": f.access, "access_expires_at": f.accessExp, "refresh_token": f.refresh,
		"device_nonce": base64.StdEncoding.EncodeToString(f.nonce), "user_id": "uOwner"}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/part/") { // object storage, not the API
		b, _ := io.ReadAll(r.Body)
		f.parts[r.URL.Path] = b
		w.WriteHeader(200)
		return
	}
	if r.Header.Get("X-Eleza-Client") == "" {
		f.t.Errorf("%s %s without X-Eleza-Client", r.Method, r.URL.Path)
	}
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	authed := func() bool {
		if f.revoked || r.Header.Get("Authorization") != "Bearer "+f.access || time.Now().After(f.accessExp) {
			problem(w, 401, "auth.token_invalid")
			return false
		}
		return true
	}
	switch r.Method + " " + r.URL.Path {
	case "POST /v1/auth/otp/request":
		if body["platform"] != "desktop" || len(body["device_id"].(string)) < 8 {
			problem(w, 400, "request.invalid")
			return
		}
		writeJSON(w, 200, map[string]any{"challenge_id": "ch1", "code_length": 5})
	case "POST /v1/auth/otp/verify":
		if body["code"] != "12345" {
			problem(w, 400, "auth.code_invalid")
			return
		}
		dk := body["device_key"].(map[string]any)
		raw, _ := base64.StdEncoding.DecodeString(dk["key"].(string))
		if dk["type"] != "ed25519" || len(raw) != ed25519.PublicKeySize {
			problem(w, 400, "auth.device_key_invalid")
			return
		}
		f.pub = raw
		if f.twoStep {
			writeJSON(w, 200, map[string]any{"two_step_required": true, "ticket": "tk", "hint": "cat"})
			return
		}
		writeJSON(w, 200, f.issue())
	case "POST /v1/auth/two-step":
		if body["ticket"] != "tk" || body["password"] != "pw" {
			problem(w, 400, "auth.two_step_password_invalid")
			return
		}
		writeJSON(w, 200, f.issue())
	case "POST /v1/auth/token/refresh":
		f.refreshes++
		if f.revoked {
			problem(w, 401, "auth.session_revoked")
			return
		}
		if body["refresh_token"] != f.refresh {
			problem(w, 401, "auth.refresh_invalid")
			return
		}
		n, _ := base64.StdEncoding.DecodeString(body["device_nonce"].(string))
		sig, _ := base64.StdEncoding.DecodeString(body["signature"].(string))
		if !f.nonceValid || !bytes.Equal(n, f.nonce) || !ed25519.Verify(f.pub, ProofMessage("refresh", n), sig) {
			problem(w, 401, "auth.device_proof_invalid")
			return
		}
		out := f.issue()
		delete(out, "user_id")
		writeJSON(w, 200, out)
	case "POST /v1/auth/nonce":
		if body["refresh_token"] != f.refresh {
			problem(w, 401, "auth.refresh_invalid")
			return
		}
		f.nonce = []byte(fmt.Sprintf("re-%d", time.Now().UnixNano()))
		f.nonceValid = true
		writeJSON(w, 200, map[string]any{"device_nonce": base64.StdEncoding.EncodeToString(f.nonce)})
	case "POST /v1/auth/logout":
		if authed() {
			f.revoked = true
			w.WriteHeader(204)
		}
	case "GET /v1/me":
		if authed() {
			writeJSON(w, 200, map[string]any{"id": "uOwner", "handle": "hadi"})
		}
	case "POST /v1/posts":
		if !authed() {
			return
		}
		if f.notReadyFor > 0 {
			f.notReadyFor--
			problem(w, 409, "media.not_ready")
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if id, ok := f.postKeys[key]; ok && key != "" {
			writeJSON(w, 200, map[string]any{"id": id})
			return
		}
		id := fmt.Sprintf("p%d", len(f.posts)+1)
		f.posts = append(f.posts, body)
		f.postKeys[key] = id
		writeJSON(w, 201, map[string]any{"id": id})
	case "POST /v1/threads":
		if authed() {
			if body["user_id"] != "uOwner" {
				problem(w, 404, "user.unavailable")
				return
			}
			writeJSON(w, 200, map[string]any{"chat": "cSaved", "kind": "direct"})
		}
	case "POST /v1/chats/cSaved/messages":
		if !authed() {
			return
		}
		cm, _ := body["client_message_id"].(string)
		if len(cm) != 36 {
			problem(w, 400, "message.invalid")
			return
		}
		if f.msgIDs[cm] {
			writeJSON(w, 200, map[string]any{"seq": 1, "duplicate": true})
			return
		}
		f.msgIDs[cm] = true
		f.messages = append(f.messages, body)
		writeJSON(w, 200, map[string]any{"seq": len(f.messages)})
	case "POST /v1/uploads":
		if !authed() {
			return
		}
		size := int64(body["size"].(float64))
		parts := []map[string]any{}
		for n := 1; int64(n-1)*4 < size; n++ {
			parts = append(parts, map[string]any{"n": n, "url": "http://" + r.Host + fmt.Sprintf("/part/u1/%d", n)})
		}
		writeJSON(w, 200, map[string]any{"upload_id": "u1", "media_id": "m1", "part_size": 4, "parts": parts, "state": "uploading"})
	case "POST /v1/uploads/u1/complete":
		if authed() {
			writeJSON(w, 200, map[string]any{"state": "processing"})
		}
	case "GET /v1/media/m1":
		if authed() {
			f.mediaPolls++
			state := "processing"
			if f.mediaPolls >= 2 {
				state = "ready"
			}
			writeJSON(w, 200, map[string]any{"id": "m1", "state": state})
		}
	default:
		problem(w, 404, "not_found")
	}
}

func newClient(t *testing.T, srv *httptest.Server) (*Client, *store.Store) {
	st, err := store.Open(filepath.Join(t.TempDir(), "s"), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	c := New(srv.URL, st)
	c.Sleep = func(context.Context, time.Duration) error { return nil }
	return c, st
}

func login(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	ch, err := c.RequestCode(ctx, "09121234567")
	if err != nil || ch.ID != "ch1" || ch.CodeLength != 5 {
		t.Fatalf("request: %+v %v", ch, err)
	}
	l, err := c.VerifyCode(ctx, ch.ID, "12345")
	if err != nil || l.TwoStep != nil || l.Handle != "hadi" {
		t.Fatalf("verify: %+v %v", l, err)
	}
}

func TestLoginRefreshAndNonceReissue(t *testing.T) {
	f, srv := newFake(t)
	c, st := newClient(t, srv)
	ctx := context.Background()
	if _, err := c.VerifyCode(ctx, "ch1", "00000"); !IsCode(err, "auth.code_invalid") {
		t.Fatalf("wrong code: %v", err)
	}
	login(t, c)
	if s := st.Get(); !s.LoggedIn() || s.UserID != "uOwner" || s.Handle != "hadi" || len(s.DeviceKey) != ed25519.PrivateKeySize {
		t.Fatalf("state %+v", s)
	}

	// The access token expires: the next call refreshes with a signed nonce.
	f.mu.Lock()
	f.accessExp = time.Now().Add(-time.Second)
	f.mu.Unlock()
	_ = st.Update(func(s *store.State) { s.AccessExp = time.Now().Add(-time.Second) })
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 || st.Get().RefreshToken != f.refresh {
		t.Fatalf("refreshes %d, stored %q server %q", f.refreshes, st.Get().RefreshToken, f.refresh)
	}

	// The nonce expired while the bridge was idle: a new one is fetched.
	f.mu.Lock()
	f.nonceValid = false
	f.accessExp = time.Now().Add(-time.Second)
	f.mu.Unlock()
	_ = st.Update(func(s *store.State) { s.AccessExp = time.Now().Add(-time.Second) })
	if _, err := c.Me(ctx); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 3 {
		t.Fatalf("refreshes %d", f.refreshes)
	}
}

func TestTwoStepLogin(t *testing.T) {
	f, srv := newFake(t)
	f.twoStep = true
	c, st := newClient(t, srv)
	ctx := context.Background()
	l, err := c.VerifyCode(ctx, "ch1", "12345")
	if err != nil || l.TwoStep == nil || l.TwoStep.Hint != "cat" {
		t.Fatalf("%+v %v", l, err)
	}
	if st.Get().LoggedIn() {
		t.Fatal("logged in before the password")
	}
	if _, err := c.SubmitPassword(ctx, "tk", "bad"); err == nil {
		t.Fatal("bad password accepted")
	}
	l, err = c.SubmitPassword(ctx, "tk", "pw")
	if err != nil || l.Handle != "hadi" || !st.Get().LoggedIn() {
		t.Fatalf("%+v %v", l, err)
	}
	// The key registered at verify is the one that signs refreshes.
	_ = st.Update(func(s *store.State) { s.AccessExp = time.Time{} })
	if _, err := c.accessToken(ctx, false); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRevokedLogsOut(t *testing.T) {
	f, srv := newFake(t)
	c, st := newClient(t, srv)
	login(t, c)
	var lost string
	c.OnLoggedOut = func(code string) { lost = code }
	f.mu.Lock()
	f.revoked = true
	f.mu.Unlock()
	_ = st.Update(func(s *store.State) { s.AccessExp = time.Time{} })
	_, err := c.Me(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err %v", err)
	}
	if lost != "auth.session_revoked" || st.Get().LoggedIn() || st.Get().DeviceID == "" {
		t.Fatalf("lost %q state %+v", lost, st.Get())
	}
}

func TestPostIdempotentAndWaitsForMedia(t *testing.T) {
	f, srv := newFake(t)
	c, _ := newClient(t, srv)
	login(t, c)
	ctx := context.Background()
	f.notReadyFor = 2
	a, err := c.CreatePost(ctx, "1:2:eli:0", PostIn{Type: "text", Text: "سلام"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.CreatePost(ctx, "1:2:eli:0", PostIn{Type: "text", Text: "سلام"})
	if err != nil || a.ID != b.ID || len(f.posts) != 1 {
		t.Fatalf("a %v b %v posts %d err %v", a, b, len(f.posts), err)
	}
	if k := idemKey("x"); len(k) < 16 || len(k) > 128 || strings.ContainsAny(k, " /+=") {
		t.Fatalf("key %q", k)
	}
}

func TestSavedMessagesAndUpload(t *testing.T) {
	f, srv := newFake(t)
	c, st := newClient(t, srv)
	login(t, c)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	mid, err := c.Upload(ctx, UploadIn{Path: path, Purpose: "chat", Kind: "file", Mime: "text/plain", Filename: "a.txt"})
	if err != nil || mid != "m1" {
		t.Fatalf("%q %v", mid, err)
	}
	got := string(f.parts["/part/u1/1"]) + string(f.parts["/part/u1/2"]) + string(f.parts["/part/u1/3"])
	if got != "0123456789" {
		t.Fatalf("parts %q", got)
	}
	chat, err := c.SavedChat(ctx)
	if err != nil || chat != "cSaved" || st.Get().SavedChatID != "cSaved" {
		t.Fatalf("%q %v", chat, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := c.SendMessage(ctx, chat, "k1", MessageIn{Kind: "file", Attachment: mid, Text: "cap"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.messages) != 1 {
		t.Fatalf("messages %d", len(f.messages))
	}
}

func TestUUIDFromIsStableAndValid(t *testing.T) {
	a, b := UUIDFrom("x"), UUIDFrom("x")
	if a != b || a == UUIDFrom("y") || len(a) != 36 || a[14] != '8' || !strings.ContainsAny(a[19:20], "89ab") {
		t.Fatalf("%s", a)
	}
}

func TestProofMessageMatchesBackend(t *testing.T) {
	// eleza-backend pkg/auth.ProofMessage: domain ‖ purpose ‖ 0 ‖ nonce.
	want := append([]byte("eleza/device-proof/v1\x00refresh\x00"), 1, 2)
	if !bytes.Equal(ProofMessage("refresh", []byte{1, 2}), want) {
		t.Fatal("proof message differs")
	}
}
