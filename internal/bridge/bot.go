package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/eleza-me/telegram-bridge-bot/internal/config"
	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/store"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

// Telegram is what the bot needs of the Telegram client.
type Telegram interface {
	Downloader
	GetUpdates(ctx context.Context, offset int64, timeout int) ([]telegram.Update, error)
	SendMessage(ctx context.Context, chat int64, text string, o telegram.SendOpts) (telegram.Message, error)
	EditMessage(ctx context.Context, chat, msg int64, text string, markup *telegram.InlineKeyboard) error
	DeleteMessage(ctx context.Context, chat, msg int64) error
	AnswerCallback(ctx context.Context, id, text string) error
	SetCommands(ctx context.Context, cmds []telegram.BotCommand) error
	DeleteWebhook(ctx context.Context) error
}

// Account is what the bot needs of the Eleza client beyond publishing.
type Account interface {
	RequestCode(ctx context.Context, phone string) (eleza.Challenge, error)
	VerifyCode(ctx context.Context, challengeID, code string) (eleza.Login, error)
	SubmitPassword(ctx context.Context, ticket, password string) (eleza.Login, error)
	Logout(ctx context.Context) error
}

// Bot is the Telegram side of the bridge. All state lives in Run's
// goroutine; publishing runs in a worker, one item at a time, in order.
type Bot struct {
	Cfg   config.Config
	TG    Telegram
	Acct  Account
	Pub   *Publisher
	Store *store.Store
	Log   *slog.Logger
	Now   func() time.Time

	events  chan func()
	work    chan task
	login   loginState
	pending map[string]*pendingItem
	order   []string // pending ids, oldest first
	groups  map[string]*albumBuf
}

type pendingItem struct {
	c      Content
	prompt int64 // the bot's message with the buttons
}

type albumBuf struct {
	msgs  []*telegram.Message
	timer *time.Timer
}

type task struct {
	id     string
	c      Content
	target string
	status int64
}

const maxPending = 200

// Run polls Telegram until ctx ends.
func (b *Bot) Run(ctx context.Context) error {
	if b.Now == nil {
		b.Now = time.Now
	}
	b.events = make(chan func(), 64)
	b.work = make(chan task, 256)
	b.pending = map[string]*pendingItem{}
	b.groups = map[string]*albumBuf{}

	if err := b.TG.DeleteWebhook(ctx); err != nil {
		b.Log.Warn("telegram: deleteWebhook failed", "err", err)
	}
	if err := b.TG.SetCommands(ctx, commands); err != nil {
		b.Log.Warn("telegram: setMyCommands failed", "err", err)
	}
	go b.worker(ctx)

	updates := make(chan []telegram.Update)
	go b.poll(ctx, updates)
	expiry := time.NewTicker(6 * time.Hour)
	defer expiry.Stop()
	b.checkExpiry(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ups := <-updates:
			for _, u := range ups {
				b.handle(ctx, u)
				id := u.UpdateID
				if err := b.Store.Update(func(s *store.State) { s.TGOffset = id + 1 }); err != nil {
					b.Log.Error("store: saving offset", "err", err)
				}
			}
		case fn := <-b.events:
			fn()
		case <-expiry.C:
			b.checkExpiry(ctx)
		}
	}
}

func (b *Bot) poll(ctx context.Context, out chan<- []telegram.Update) {
	offset := b.Store.Get().TGOffset
	failures := 0
	for ctx.Err() == nil {
		ups, err := b.TG.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			wait := time.Duration(min(failures, 6)) * 5 * time.Second
			var te *telegram.Error
			if errors.As(err, &te) && te.RetryAfter > 0 {
				wait = time.Duration(te.RetryAfter) * time.Second
			}
			b.Log.Warn("telegram: getUpdates failed", "err", err, "retry_in", wait)
			sleep(ctx, wait)
			continue
		}
		failures = 0
		if len(ups) == 0 {
			continue
		}
		offset = ups[len(ups)-1].UpdateID + 1
		select {
		case out <- ups:
		case <-ctx.Done():
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// post runs fn in Run's goroutine.
func (b *Bot) post(fn func()) { b.events <- fn }

func (b *Bot) owner() int64 { return b.Cfg.OwnerID }

func (b *Bot) say(ctx context.Context, text string, o telegram.SendOpts) telegram.Message {
	m, err := b.TG.SendMessage(ctx, b.owner(), text, o)
	if err != nil {
		b.Log.Warn("telegram: sendMessage failed", "err", err)
	}
	return m
}

func (b *Bot) handle(ctx context.Context, u telegram.Update) {
	switch {
	case u.CallbackQuery != nil:
		q := u.CallbackQuery
		if q.From.ID != b.owner() {
			_ = b.TG.AnswerCallback(ctx, q.ID, "")
			return
		}
		b.callback(ctx, q)
	case u.Message != nil:
		m := u.Message
		if m.From == nil || m.From.ID != b.owner() || m.Chat.Type != "private" {
			b.Log.Info("ignoring a message from someone else", "from", senderID(m))
			return
		}
		b.message(ctx, m)
	}
}

func senderID(m *telegram.Message) int64 {
	if m.From == nil {
		return 0
	}
	return m.From.ID
}

func (b *Bot) message(ctx context.Context, m *telegram.Message) {
	if cmd, args, ok := command(m); ok {
		b.command(ctx, m, cmd, args)
		return
	}
	if b.login.step != stepNone {
		b.loginInput(ctx, m)
		return
	}
	if m.MediaGroupID != "" {
		b.albumPart(m)
		return
	}
	b.content(ctx, []*telegram.Message{m})
}

// command parses "/cmd@bot args" of a message the owner typed (not a forward).
func command(m *telegram.Message) (string, string, bool) {
	if m.ForwardOrigin != nil || !strings.HasPrefix(m.Text, "/") {
		return "", "", false
	}
	head, args, _ := strings.Cut(m.Text[1:], " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.ToLower(head), strings.TrimSpace(args), head != ""
}

func (b *Bot) albumPart(m *telegram.Message) {
	g := b.groups[m.MediaGroupID]
	if g == nil {
		g = &albumBuf{}
		b.groups[m.MediaGroupID] = g
	}
	g.msgs = append(g.msgs, m)
	id := m.MediaGroupID
	if g.timer != nil {
		g.timer.Stop()
	}
	// Telegram delivers an album as separate messages: wait for the rest.
	g.timer = time.AfterFunc(b.Cfg.AlbumWait, func() {
		b.post(func() {
			g := b.groups[id]
			if g == nil {
				return
			}
			delete(b.groups, id)
			b.content(context.Background(), g.msgs)
		})
	})
}

func (b *Bot) content(ctx context.Context, msgs []*telegram.Message) {
	c := BuildContent(msgs, b.Cfg.ForwardSource)
	reply := telegram.SendOpts{ReplyTo: c.FirstMsg}
	if c.Empty() {
		msg := "این پیام چیزی ندارد که بشود در الزا فرستاد."
		if len(c.Skipped) > 0 {
			msg = "پشتیبانی نمی‌شود: " + strings.Join(c.Skipped, "، ")
		}
		b.say(ctx, msg, reply)
		return
	}
	if !b.Store.Get().LoggedIn() {
		b.say(ctx, "اول با /login وارد حساب الزا شو.", reply)
		return
	}
	id := newID()
	switch target := b.target(); target {
	case TargetAsk:
		reply.Markup = targetKeyboard(id)
		p := b.say(ctx, "کجا بفرستم؟", reply)
		b.remember(id, &pendingItem{c: c, prompt: p.MessageID})
	default:
		p := b.say(ctx, "⏳ "+targetLabel(target)+"…", reply)
		b.enqueue(task{id: id, c: c, target: target, status: p.MessageID})
	}
}

func (b *Bot) target() string {
	if t := b.Store.Get().Target; t != "" {
		return t
	}
	return b.Cfg.DefaultTarget
}

func (b *Bot) remember(id string, it *pendingItem) {
	b.pending[id] = it
	b.order = append(b.order, id)
	for len(b.order) > maxPending {
		delete(b.pending, b.order[0])
		b.order = b.order[1:]
	}
}

func (b *Bot) forget(id string) {
	delete(b.pending, id)
	for i, o := range b.order {
		if o == id {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
}

func (b *Bot) enqueue(t task) {
	select {
	case b.work <- t:
	default:
		b.say(context.Background(), "صف ارسال پر است؛ کمی بعد دوباره بفرست.", telegram.SendOpts{ReplyTo: t.c.FirstMsg})
	}
}

func newID() string {
	var r [6]byte
	_, _ = rand.Read(r[:])
	return hex.EncodeToString(r[:])
}

func targetKeyboard(id string) *telegram.InlineKeyboard {
	return &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{
		{{Text: "📝 الی", CallbackData: "t:" + id + ":" + TargetEli}, {Text: "💾 ذخیره در الزا", CallbackData: "t:" + id + ":" + TargetSaved}},
		{{Text: "📝+💾 هر دو", CallbackData: "t:" + id + ":" + TargetBoth}, {Text: "✖️ بی‌خیال", CallbackData: "t:" + id + ":x"}},
	}}
}

func targetLabel(t string) string {
	switch t {
	case TargetEli:
		return "انتشار در الی"
	case TargetSaved:
		return "ذخیره در پیام‌های ذخیره‌شده"
	case TargetBoth:
		return "انتشار در الی و ذخیره"
	}
	return "هر بار بپرس"
}

func (b *Bot) callback(ctx context.Context, q *telegram.CallbackQuery) {
	parts := strings.Split(q.Data, ":")
	switch {
	case len(parts) == 3 && parts[0] == "t":
		id, target := parts[1], parts[2]
		it := b.pending[id]
		if it == nil {
			_ = b.TG.AnswerCallback(ctx, q.ID, "این مورد دیگر در دسترس نیست؛ دوباره بفرستش.")
			return
		}
		_ = b.TG.AnswerCallback(ctx, q.ID, "")
		b.forget(id)
		if target == "x" {
			_ = b.TG.EditMessage(ctx, b.owner(), it.prompt, "لغو شد.", nil)
			return
		}
		_ = b.TG.EditMessage(ctx, b.owner(), it.prompt, "⏳ "+targetLabel(target)+"…", nil)
		b.enqueue(task{id: id, c: it.c, target: target, status: it.prompt})
	case len(parts) == 2 && parts[0] == "m":
		t := parts[1]
		if t != TargetAsk && t != TargetEli && t != TargetSaved && t != TargetBoth {
			_ = b.TG.AnswerCallback(ctx, q.ID, "")
			return
		}
		if err := b.Store.Update(func(s *store.State) { s.Target = t }); err != nil {
			_ = b.TG.AnswerCallback(ctx, q.ID, "ذخیره نشد")
			return
		}
		_ = b.TG.AnswerCallback(ctx, q.ID, "ذخیره شد")
		if q.Message != nil {
			_ = b.TG.EditMessage(ctx, b.owner(), q.Message.MessageID, "حالت ارسال: "+targetLabel(t), nil)
		}
	default:
		_ = b.TG.AnswerCallback(ctx, q.ID, "")
	}
}

func (b *Bot) worker(ctx context.Context) {
	for {
		var t task
		select {
		case <-ctx.Done():
			return
		case t = <-b.work:
		}
		jctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
		res, err := b.Pub.Publish(jctx, t.c, t.target)
		cancel()
		text := resultText(res, err)
		var markup *telegram.InlineKeyboard
		if err != nil {
			b.Log.Warn("publish failed", "target", t.target, "err", err)
			if !errors.Is(err, eleza.ErrNotLoggedIn) {
				// The same keys are used again, so a retry never publishes twice.
				markup = &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{{
					{Text: "🔁 دوباره", CallbackData: "t:" + t.id + ":" + t.target},
					{Text: "✖️ بی‌خیال", CallbackData: "t:" + t.id + ":x"},
				}}}
				done := t
				b.post(func() { b.remember(done.id, &pendingItem{c: done.c, prompt: done.status}) })
			}
		}
		if t.status != 0 {
			if err := b.TG.EditMessage(ctx, b.owner(), t.status, text, markup); err != nil {
				b.say(ctx, text, telegram.SendOpts{ReplyTo: t.c.FirstMsg, Markup: markup})
			}
		} else {
			b.say(ctx, text, telegram.SendOpts{ReplyTo: t.c.FirstMsg, Markup: markup})
		}
	}
}

func resultText(r Result, err error) string {
	var lines []string
	if len(r.PostIDs) > 0 {
		lines = append(lines, "✅ در الی منتشر شد:")
		for _, id := range r.PostIDs {
			lines = append(lines, eleza.PostURL(id))
		}
	}
	if r.Saved > 0 {
		lines = append(lines, "✅ در پیام‌های ذخیره‌شدهٔ الزا ذخیره شد.")
	}
	if len(r.Skipped) > 0 {
		lines = append(lines, "⚠️ فرستاده نشد: "+strings.Join(r.Skipped, "، "))
	}
	if err != nil {
		lines = append(lines, "❌ "+humanError(err))
	}
	if len(lines) == 0 {
		return "✅ انجام شد."
	}
	return strings.Join(lines, "\n")
}

// humanError keeps the API's own message (it is in Persian) and its code.
func humanError(err error) string {
	switch {
	case errors.Is(err, eleza.ErrNotLoggedIn):
		return "نشست الزا تمام شده؛ دوباره /login کن."
	case errors.Is(err, telegram.ErrTooBig):
		return "این فایل بزرگ‌تر از ۲۰ مگابایت است و API عمومی تلگرام آن را به ربات نمی‌دهد (برای فایل‌های بزرگ، سرور Bot API محلی لازم است؛ README را ببین)."
	case errors.Is(err, ErrNothing):
		return "چیزی برای ارسال در این مقصد نیست."
	case errors.Is(err, context.DeadlineExceeded):
		return "زمان ارسال تمام شد."
	}
	return err.Error()
}

func (b *Bot) checkExpiry(ctx context.Context) {
	st := b.Store.Get()
	if !st.LoggedIn() || st.ExpiryWarned || st.LoggedInAt.IsZero() {
		return
	}
	// Refresh tokens live auth.refresh_token_ttl (60 days by default) from login.
	if b.Now().Sub(st.LoggedInAt) < 57*24*time.Hour {
		return
	}
	b.say(ctx, "⚠️ ورود ربات به الزا تا چند روز دیگر منقضی می‌شود (حدود ۶۰ روز بعد از ورود). برای ادامه، دوباره /login کن.", telegram.SendOpts{})
	_ = b.Store.Update(func(s *store.State) { s.ExpiryWarned = true })
}

// SessionLost tells the owner that Eleza ended the session (any goroutine).
func (b *Bot) SessionLost(code string) {
	text := "⚠️ الزا این دستگاه را از حساب خارج کرد"
	switch code {
	case "auth.session_revoked":
		text += " (از بخش دستگاه‌ها یا خروج)"
	case "auth.step_up_required":
		text += " (برای امنیت، ورود دوباره با کد لازم است)"
	}
	b.say(context.Background(), text+". برای ادامه /login کن.", telegram.SendOpts{})
}

var commands = []telegram.BotCommand{
	{Command: "login", Description: "ورود به حساب الزا"},
	{Command: "mode", Description: "مقصد پیش‌فرض: الی، ذخیره، هر دو یا بپرس"},
	{Command: "status", Description: "وضعیت اتصال"},
	{Command: "logout", Description: "خروج از حساب الزا"},
	{Command: "cancel", Description: "لغو ورود در حال انجام"},
	{Command: "help", Description: "راهنما"},
}

const helpText = `هر چیزی برام بفرستی یا فوروارد کنی، در الزا با حساب خودت منتشر می‌کنم:
📝 الی: به‌عنوان پست
💾 ذخیره: در «پیام‌های ذخیره‌شده»

متن، عکس، ویدیو، گیف، آلبوم، ویدیو مسیج، ویس، آهنگ، فایل، نظرسنجی و موقعیت مکانی.
متن بلند در الی به رشته (thread) تبدیل می‌شود. پیام‌های فورواردی یک خط «↪️ از …» می‌گیرند.

دستورها:
/login ورود (با کد پیامکی الزا)
/mode مقصد پیش‌فرض
/status وضعیت
/logout خروج`

func (b *Bot) command(ctx context.Context, m *telegram.Message, cmd, args string) {
	switch cmd {
	case "start", "help":
		b.say(ctx, helpText, telegram.SendOpts{})
		if !b.Store.Get().LoggedIn() {
			b.say(ctx, "برای شروع /login را بزن.", telegram.SendOpts{})
		}
	case "login":
		b.startLogin(ctx)
	case "cancel":
		if b.login.step != stepNone {
			b.login = loginState{}
			b.say(ctx, "ورود لغو شد.", telegram.SendOpts{Markup: telegram.RemoveKeyboard{RemoveKeyboard: true}})
			return
		}
		b.say(ctx, "کاری در جریان نیست.", telegram.SendOpts{})
	case "logout":
		b.login = loginState{}
		if err := b.Acct.Logout(ctx); err != nil {
			b.say(ctx, "از ربات خارج شدی، ولی الزا خروج را تأیید نکرد: "+err.Error()+"\nدر الزا از بخش دستگاه‌ها هم می‌توانی این دستگاه را حذف کنی.", telegram.SendOpts{})
			return
		}
		b.say(ctx, "✅ از حساب الزا خارج شدی.", telegram.SendOpts{})
	case "status":
		b.say(ctx, b.status(), telegram.SendOpts{})
	case "mode":
		kb := &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{
			{{Text: "❓ هر بار بپرس", CallbackData: "m:" + TargetAsk}},
			{{Text: "📝 همیشه الی", CallbackData: "m:" + TargetEli}, {Text: "💾 همیشه ذخیره", CallbackData: "m:" + TargetSaved}},
			{{Text: "📝+💾 همیشه هر دو", CallbackData: "m:" + TargetBoth}},
		}}
		b.say(ctx, "حالت فعلی: "+targetLabel(b.target())+"\nحالت جدید را انتخاب کن:", telegram.SendOpts{Markup: kb})
	default:
		// An unknown "/…" is ordinary text to publish.
		if b.login.step != stepNone {
			b.loginInput(ctx, m)
			return
		}
		b.content(ctx, []*telegram.Message{m})
	}
}

func (b *Bot) status() string {
	st := b.Store.Get()
	if !st.LoggedIn() {
		return "به الزا وصل نیست. /login"
	}
	who := st.Handle
	if who != "" {
		who = "@" + who
	} else {
		who = st.UserID
	}
	s := "✅ وصل به الزا: " + who + "\nحالت ارسال: " + targetLabel(b.target())
	if !st.LoggedInAt.IsZero() {
		days := int(st.LoggedInAt.Add(60*24*time.Hour).Sub(b.Now()).Hours() / 24)
		s += fmt.Sprintf("\nورود دوباره لازم است تا حدود %d روز دیگر", max(days, 0))
	}
	return s
}
