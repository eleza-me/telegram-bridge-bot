package bridge

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

type loginStep int

const (
	stepNone loginStep = iota
	stepPhone
	stepCode
	stepPassword
)

type loginState struct {
	step      loginStep
	challenge string
	ticket    string
}

func (b *Bot) startLogin(ctx context.Context) {
	b.login = loginState{step: stepPhone}
	kb := telegram.ReplyKeyboard{Keyboard: [][]telegram.KeyboardButton{{{Text: "📱 فرستادن شمارهٔ همین تلگرام", RequestContact: true}}},
		ResizeKeyboard: true, OneTimeKeyboard: true}
	note := ""
	if b.Store.Get().LoggedIn() {
		note = "\n(الان وارد هستی؛ ورود تازه جای نشست فعلی را می‌گیرد.)"
	}
	b.say(ctx, "شمارهٔ موبایل حساب الزا را بفرست (مثلاً 09121234567)، یا دکمهٔ پایین را بزن."+note+"\nلغو: /cancel", telegram.SendOpts{Markup: kb})
}

func (b *Bot) loginInput(ctx context.Context, m *telegram.Message) {
	switch b.login.step {
	case stepPhone:
		phone := ""
		switch {
		case m.Contact != nil:
			if m.Contact.UserID != 0 && m.Contact.UserID != b.owner() {
				b.say(ctx, "این شمارهٔ خودت نیست. شماره را تایپ کن.", telegram.SendOpts{})
				return
			}
			phone = NormalizePhone(m.Contact.PhoneNumber, true)
		default:
			phone = NormalizePhone(m.Text, false)
		}
		if phone == "" {
			b.say(ctx, "شماره درست نیست. دوباره بفرست، یا /cancel.", telegram.SendOpts{})
			return
		}
		ch, err := b.Acct.RequestCode(ctx, phone)
		if err != nil {
			b.say(ctx, "❌ "+humanError(err), telegram.SendOpts{})
			return
		}
		b.login = loginState{step: stepCode, challenge: ch.ID}
		n := ch.CodeLength
		if n == 0 {
			n = 6
		}
		b.say(ctx, fmt.Sprintf("کد %d رقمی که الزا پیامک کرد را بفرست.\nپیامت را بعد از خواندن پاک می‌کنم.", n),
			telegram.SendOpts{Markup: telegram.RemoveKeyboard{RemoveKeyboard: true}})
	case stepCode:
		_ = b.TG.DeleteMessage(ctx, b.owner(), m.MessageID)
		code := Digits(m.Text)
		if code == "" {
			b.say(ctx, "فقط رقم‌های کد را بفرست.", telegram.SendOpts{})
			return
		}
		l, err := b.Acct.VerifyCode(ctx, b.login.challenge, code)
		if err != nil {
			if fatalLoginError(err) {
				b.login = loginState{}
				b.say(ctx, "❌ "+humanError(err)+"\nدوباره /login کن.", telegram.SendOpts{})
				return
			}
			b.say(ctx, "❌ "+humanError(err)+"\nدوباره بفرست، یا /cancel.", telegram.SendOpts{})
			return
		}
		if l.TwoStep != nil {
			b.login = loginState{step: stepPassword, ticket: l.TwoStep.Ticket}
			msg := "حسابت تأیید دومرحله‌ای دارد. رمز را بفرست (پیامت را پاک می‌کنم)."
			if l.TwoStep.Hint != "" {
				msg += "\nیادآور: " + l.TwoStep.Hint
			}
			b.say(ctx, msg, telegram.SendOpts{})
			return
		}
		b.loggedIn(ctx, l)
	case stepPassword:
		_ = b.TG.DeleteMessage(ctx, b.owner(), m.MessageID)
		if m.Text == "" {
			b.say(ctx, "رمز را به‌صورت متن بفرست.", telegram.SendOpts{})
			return
		}
		l, err := b.Acct.SubmitPassword(ctx, b.login.ticket, m.Text)
		if err != nil {
			if fatalLoginError(err) {
				b.login = loginState{}
				b.say(ctx, "❌ "+humanError(err)+"\nدوباره /login کن.", telegram.SendOpts{})
				return
			}
			b.say(ctx, "❌ "+humanError(err)+"\nدوباره بفرست، یا /cancel.", telegram.SendOpts{})
			return
		}
		b.loggedIn(ctx, l)
	}
}

func (b *Bot) loggedIn(ctx context.Context, l eleza.Login) {
	b.login = loginState{}
	who := "حساب الزا"
	if l.Handle != "" {
		who = "@" + l.Handle
	}
	msg := "✅ وارد شدی: " + who + "\nاز این به بعد هر چه بفرستی یا فوروارد کنی را می‌فرستم. حالت ارسال: " + targetLabel(b.target()) + " (/mode)" +
		"\nاین ربات در الزا، بخش دستگاه‌ها، با نام «" + b.Cfg.DeviceName + "» دیده می‌شود."
	if l.IsNew {
		msg += "\n⚠️ این شماره قبلاً حساب الزا نداشت و یک حساب تازه ساخته شد."
	}
	b.say(ctx, msg, telegram.SendOpts{})
}

// fatalLoginError: the login attempt cannot continue (a new code is needed).
func fatalLoginError(err error) bool {
	e, ok := eleza.AsAPIError(err)
	if !ok {
		return false
	}
	c := e.Code
	return strings.Contains(c, "expired") || strings.Contains(c, "too_many") || strings.Contains(c, "attempts") ||
		strings.Contains(c, "ticket") || c == "rate_limited" || e.Status == 429
}

// Digits keeps the digits of s, Persian and Arabic digits turned into ASCII.
func Digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '۰' && r <= '۹':
			b.WriteRune('0' + r - '۰')
		case r >= '٠' && r <= '٩':
			b.WriteRune('0' + r - '٠')
		case unicode.IsSpace(r) || r == '-':
		default:
			return ""
		}
	}
	return b.String()
}

// NormalizePhone turns what the owner typed (or Telegram's contact number,
// which comes without "+") into a form the server accepts. "" = not a number.
func NormalizePhone(s string, international bool) string {
	s = strings.TrimSpace(s)
	plus := strings.HasPrefix(s, "+")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= '۰' && r <= '۹':
			b.WriteRune('0' + r - '۰')
		case r >= '٠' && r <= '٩':
			b.WriteRune('0' + r - '٠')
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')':
		default:
			return ""
		}
	}
	d := b.String()
	if len(d) < 7 || len(d) > 15 {
		return ""
	}
	if plus || international {
		return "+" + d
	}
	if strings.HasPrefix(d, "00") {
		return "+" + d[2:]
	}
	return d // national form (0912…): the server reads it as an Iranian number
}
