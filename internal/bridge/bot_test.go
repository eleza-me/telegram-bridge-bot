package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eleza-me/telegram-bridge-bot/internal/config"
	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/store"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

const owner = 42

type sent struct {
	id     int64
	text   string
	markup any
}

// fakeTG records what the bot says and feeds it updates.
type fakeTG struct {
	mu      sync.Mutex
	next    int64
	sent    []sent
	edits   map[int64]string
	editKB  map[int64]*telegram.InlineKeyboard
	deleted []int64
	answers []string
	updates chan telegram.Update
}

func newFakeTG() *fakeTG {
	return &fakeTG{next: 1000, edits: map[int64]string{}, editKB: map[int64]*telegram.InlineKeyboard{}, updates: make(chan telegram.Update, 16)}
}

func (f *fakeTG) GetUpdates(ctx context.Context, offset int64, timeout int) ([]telegram.Update, error) {
	select {
	case u := <-f.updates:
		return []telegram.Update{u}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (f *fakeTG) SendMessage(ctx context.Context, chat int64, text string, o telegram.SendOpts) (telegram.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if chat != owner {
		panic("message to someone else")
	}
	f.next++
	f.sent = append(f.sent, sent{f.next, text, o.Markup})
	return telegram.Message{MessageID: f.next}, nil
}
func (f *fakeTG) EditMessage(ctx context.Context, chat, msg int64, text string, markup *telegram.InlineKeyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits[msg] = text
	f.editKB[msg] = markup
	return nil
}
func (f *fakeTG) DeleteMessage(ctx context.Context, chat, msg int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, msg)
	return nil
}
func (f *fakeTG) AnswerCallback(ctx context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, id)
	return nil
}
func (f *fakeTG) SetCommands(context.Context, []telegram.BotCommand) error { return nil }
func (f *fakeTG) DeleteWebhook(context.Context) error                      { return nil }
func (f *fakeTG) Download(ctx context.Context, fileID string, size int64, dst string) error {
	return os.WriteFile(dst, []byte("bytes of "+fileID), 0o600)
}

func (f *fakeTG) last() sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sent{}
	}
	return f.sent[len(f.sent)-1]
}

// fakeEleza is the account and the publishing API.
type fakeEleza struct {
	mu       sync.Mutex
	st       *store.Store
	codes    []string
	phone    string
	posts    []eleza.PostIn
	postKeys map[string]string
	msgs     []eleza.MessageIn
	uploads  []eleza.UploadIn
	failPost error
}

func (e *fakeEleza) RequestCode(ctx context.Context, phone string) (eleza.Challenge, error) {
	e.phone = phone
	return eleza.Challenge{ID: "ch", CodeLength: 5}, nil
}
func (e *fakeEleza) VerifyCode(ctx context.Context, ch, code string) (eleza.Login, error) {
	e.codes = append(e.codes, code)
	if code != "12345" {
		return eleza.Login{}, &eleza.APIError{Status: 400, Code: "auth.code_invalid", Title: "کد نادرست است"}
	}
	return eleza.Login{TwoStep: &eleza.TwoStep{Ticket: "tk", Hint: "گربه"}}, nil
}
func (e *fakeEleza) SubmitPassword(ctx context.Context, ticket, pw string) (eleza.Login, error) {
	if ticket != "tk" || pw != "pw" {
		return eleza.Login{}, &eleza.APIError{Status: 400, Code: "auth.two_step_password_invalid", Title: "رمز نادرست است"}
	}
	_ = e.st.Update(func(s *store.State) { s.RefreshToken, s.Handle = "r", "hadi" })
	return eleza.Login{Handle: "hadi"}, nil
}
func (e *fakeEleza) Logout(ctx context.Context) error {
	return e.st.Update(func(s *store.State) { s.ClearSession() })
}
func (e *fakeEleza) Upload(ctx context.Context, in eleza.UploadIn) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := os.ReadFile(in.Path)
	if !bytes.HasPrefix(b, []byte("bytes of ")) {
		return "", fmt.Errorf("upload of a file that was not downloaded")
	}
	e.uploads = append(e.uploads, in)
	return fmt.Sprintf("m%d", len(e.uploads)), nil
}
func (e *fakeEleza) CreatePost(ctx context.Context, key string, p eleza.PostIn) (eleza.PostOut, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failPost != nil {
		err := e.failPost
		e.failPost = nil
		return eleza.PostOut{}, err
	}
	if id, ok := e.postKeys[key]; ok {
		return eleza.PostOut{ID: id}, nil
	}
	e.posts = append(e.posts, p)
	id := fmt.Sprintf("p%d", len(e.posts))
	e.postKeys[key] = id
	return eleza.PostOut{ID: id}, nil
}
func (e *fakeEleza) SavedChat(ctx context.Context) (string, error) { return "cSaved", nil }
func (e *fakeEleza) SendMessage(ctx context.Context, chat, key string, m eleza.MessageIn) (eleza.MessageOut, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.msgs = append(e.msgs, m)
	return eleza.MessageOut{Seq: int64(len(e.msgs))}, nil
}

type harness struct {
	t      *testing.T
	tg     *fakeTG
	el     *fakeEleza
	st     *store.Store
	nextID int64
	upd    int64
}

func start(t *testing.T) *harness {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "s"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tg := newFakeTG()
	el := &fakeEleza{st: st, postKeys: map[string]string{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{OwnerID: owner, DefaultTarget: TargetAsk, ForwardSource: true, PollDuration: "24h", AlbumWait: 50 * time.Millisecond,
		Limits: lim, DeviceName: "Telegram bridge"}
	b := &Bot{Cfg: cfg, TG: tg, Acct: el, Store: st, Log: log,
		Pub: &Publisher{TG: tg, EL: el, Limits: lim, PollDuration: "24h", TmpDir: filepath.Join(dir, "tmp"), Log: log}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &harness{t: t, tg: tg, el: el, st: st, nextID: 1}
}

func (h *harness) send(m *telegram.Message) {
	h.upd++
	if m.MessageID == 0 {
		h.nextID++
		m.MessageID = h.nextID
	}
	if m.From == nil {
		m.From = &telegram.User{ID: owner}
	}
	if m.Chat.ID == 0 {
		m.Chat = telegram.Chat{ID: m.From.ID, Type: "private"}
	}
	h.tg.updates <- telegram.Update{UpdateID: h.upd, Message: m}
}

func (h *harness) press(data string) {
	h.upd++
	h.tg.updates <- telegram.Update{UpdateID: h.upd, CallbackQuery: &telegram.CallbackQuery{ID: fmt.Sprint("q", h.upd), From: telegram.User{ID: owner}, Data: data}}
}

func (h *harness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.tg.mu.Lock()
		h.el.mu.Lock()
		good := ok()
		h.el.mu.Unlock()
		h.tg.mu.Unlock()
		if good {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s; bot said %+v, edits %+v", what, h.tg.sent, h.tg.edits)
}

func lastText(h *harness) string {
	if len(h.tg.sent) == 0 {
		return ""
	}
	return h.tg.sent[len(h.tg.sent)-1].text
}

func (h *harness) login() {
	h.send(&telegram.Message{Text: "/login"})
	h.waitFor("phone prompt", func() bool { return strings.Contains(lastText(h), "شمارهٔ موبایل") })
	h.send(&telegram.Message{Contact: &telegram.Contact{PhoneNumber: "989121234567", UserID: owner}})
	h.waitFor("code prompt", func() bool { return strings.Contains(lastText(h), "کد 5 رقمی") })
	h.send(&telegram.Message{Text: "۱۲۳۴۴"})
	h.waitFor("wrong code", func() bool { return strings.Contains(lastText(h), "کد نادرست") })
	h.send(&telegram.Message{Text: "12 345"})
	h.waitFor("password prompt", func() bool { return strings.Contains(lastText(h), "یادآور: گربه") })
	h.send(&telegram.Message{Text: "pw"})
	h.waitFor("logged in", func() bool { return strings.Contains(lastText(h), "وارد شدی: @hadi") })
}

func TestLoginFlowDeletesSecrets(t *testing.T) {
	h := start(t)
	h.login()
	if h.el.phone != "+989121234567" || strings.Join(h.el.codes, ",") != "12344,12345" {
		t.Fatalf("phone %q codes %q", h.el.phone, h.el.codes)
	}
	// The two codes and the password were deleted from the chat.
	h.waitFor("deletes", func() bool { return len(h.tg.deleted) == 3 })
}

func TestStrangersAreIgnored(t *testing.T) {
	h := start(t)
	h.send(&telegram.Message{Text: "/login", From: &telegram.User{ID: 7}})
	h.send(&telegram.Message{Text: "/help"})
	h.waitFor("help", func() bool { return len(h.tg.sent) > 0 })
	h.tg.mu.Lock()
	defer h.tg.mu.Unlock()
	for _, s := range h.tg.sent {
		if strings.Contains(s.text, "شمارهٔ موبایل") {
			t.Fatal("answered a stranger's /login")
		}
	}
}

func TestNeedsLoginFirst(t *testing.T) {
	h := start(t)
	h.send(&telegram.Message{Text: "hello"})
	h.waitFor("login hint", func() bool { return strings.Contains(lastText(h), "/login") })
}

func TestAskThenPublishToEli(t *testing.T) {
	h := start(t)
	h.login()
	h.send(&telegram.Message{Text: "سلام الزا", ForwardOrigin: &telegram.MessageOrigin{Type: "user", SenderUser: &telegram.User{FirstName: "Sara", Username: "sara"}}})
	h.waitFor("prompt", func() bool { return lastText(h) == "کجا بفرستم؟" })
	kb := h.tg.last().markup.(*telegram.InlineKeyboard)
	data := kb.InlineKeyboard[0][0].CallbackData
	if !strings.HasSuffix(data, ":eli") {
		t.Fatalf("first button %q", data)
	}
	prompt := h.tg.last().id
	h.press(data)
	h.waitFor("published", func() bool { return strings.Contains(h.tg.edits[prompt], "https://eleza.me/p/p1") })
	if h.el.posts[0].Text != "سلام الزا\n\n↪️ از Sara (t.me/sara)" {
		t.Fatalf("post %q", h.el.posts[0].Text)
	}
	// The item is used up: a second press does nothing.
	h.press(data)
	h.waitFor("answer", func() bool { return len(h.tg.answers) == 2 })
	if len(h.el.posts) != 1 {
		t.Fatal("published twice")
	}
}

func TestModeSavedAndAlbum(t *testing.T) {
	h := start(t)
	h.login()
	h.send(&telegram.Message{Text: "/mode"})
	h.waitFor("mode menu", func() bool { return strings.Contains(lastText(h), "حالت جدید") })
	h.press("m:saved")
	h.waitFor("mode saved", func() bool { return h.st.Get().Target == TargetSaved })
	for i, id := range []string{"a", "b", "c"} {
		m := &telegram.Message{MediaGroupID: "g1", Photo: []telegram.PhotoSize{{FileID: id, Width: 10, Height: 10}}}
		if i == 0 {
			m.Caption = "سه عکس"
		}
		h.send(m)
	}
	h.waitFor("saved", func() bool { return len(h.el.msgs) == 3 })
	h.waitFor("status", func() bool {
		for _, v := range h.tg.edits {
			if strings.Contains(v, "پیام‌های ذخیره‌شده") {
				return true
			}
		}
		return false
	})
	h.el.mu.Lock()
	defer h.el.mu.Unlock()
	if h.el.msgs[0].Text != "سه عکس" || h.el.msgs[0].Album == "" || h.el.msgs[2].Album != h.el.msgs[0].Album || !h.el.msgs[0].Silent {
		t.Fatalf("%+v", h.el.msgs)
	}
	for _, u := range h.el.uploads {
		if u.Purpose != "chat" || u.Kind != "image" {
			t.Fatalf("%+v", u)
		}
	}
}

func TestFailureOffersRetryWithoutDuplicates(t *testing.T) {
	h := start(t)
	h.login()
	_ = h.st.Update(func(s *store.State) { s.Target = TargetBoth })
	h.el.mu.Lock()
	h.el.failPost = &eleza.APIError{Status: 503, Code: "unavailable", Title: "سرویس در دسترس نیست"}
	h.el.mu.Unlock()
	h.send(&telegram.Message{Text: "متن"})
	var status int64
	h.waitFor("failure", func() bool {
		for id, v := range h.tg.edits {
			if strings.Contains(v, "❌") && strings.Contains(v, "سرویس در دسترس نیست") {
				status = id
				return true
			}
		}
		return false
	})
	// Saved Messages went through despite the Eli failure.
	if len(h.el.msgs) != 1 {
		t.Fatalf("msgs %d", len(h.el.msgs))
	}
	h.tg.mu.Lock()
	kb := h.tg.editKB[status]
	h.tg.mu.Unlock()
	if kb == nil || !strings.HasPrefix(kb.InlineKeyboard[0][0].Text, "🔁") {
		t.Fatalf("no retry button: %+v", kb)
	}
	h.press(kb.InlineKeyboard[0][0].CallbackData)
	h.waitFor("retried", func() bool { return strings.Contains(h.tg.edits[status], "https://eleza.me/p/p1") })
	// The retry reuses the item's keys (the server answers a repeated
	// Saved Messages send as a duplicate); Eli got exactly one post.
	if len(h.el.posts) != 1 {
		t.Fatalf("posts %d", len(h.el.posts))
	}
}
