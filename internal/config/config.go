// Package config reads the bridge's settings from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is everything the bridge needs to run.
type Config struct {
	TelegramToken string
	TelegramAPI   string // https://api.telegram.org, or a self-hosted Bot API server
	TelegramLocal bool   // the Bot API server runs with --local (no 20 MB download cap)
	OwnerID       int64  // the only Telegram user the bot answers
	ElezaAPI      string
	ClientHeader  string // X-Eleza-Client
	DeviceName    string // shown in Eleza's "devices" list
	DataDir       string
	SecretKey     []byte // 32 bytes: encrypts the state file
	DefaultTarget string // ask | eli | saved | both (the first value of /mode)
	ForwardSource bool   // add "↪️ from …" to forwarded content
	PollDuration  string // Eli polls: one of the server's allowed durations
	AlbumWait     time.Duration
	Limits        Limits
}

// Limits mirror the server's defaults (client-config). The server stays the
// judge: a smaller value only makes the bridge split earlier.
type Limits struct {
	EliText        int // post.max_text_chars
	EliThreadParts int // post.max_thread_parts
	EliAlbum       int // post.max_album_items
	EliPollOptions int // post.poll_max_options
	ChatText       int // messaging.max_message_length
	ChatCaption    int // messaging.max_caption_length
	ChatAlbum      int // messaging.max_album_items
}

// Load reads the environment.
func Load() (Config, error) {
	c := Config{
		TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramAPI:   strings.TrimRight(env("TELEGRAM_API_URL", "https://api.telegram.org"), "/"),
		ElezaAPI:      strings.TrimRight(env("ELEZA_API_URL", "https://api.eleza.me"), "/"),
		ClientHeader:  env("ELEZA_CLIENT_HEADER", "desktop/0.1.0 (tgbridge)"),
		DeviceName:    env("BRIDGE_DEVICE_NAME", "Telegram bridge"),
		DataDir:       env("BRIDGE_DATA_DIR", "/data"),
		DefaultTarget: env("BRIDGE_DEFAULT_TARGET", "ask"),
		PollDuration:  env("BRIDGE_POLL_DURATION", "24h"),
	}
	var errs []error
	if c.TelegramToken == "" {
		errs = append(errs, errors.New("TELEGRAM_BOT_TOKEN is required"))
	}
	owner, err := strconv.ParseInt(os.Getenv("TELEGRAM_OWNER_ID"), 10, 64)
	if err != nil || owner <= 0 {
		errs = append(errs, errors.New("TELEGRAM_OWNER_ID must be your numeric Telegram user id"))
	}
	c.OwnerID = owner
	key, err := base64.StdEncoding.DecodeString(os.Getenv("BRIDGE_SECRET_KEY"))
	if err != nil || len(key) != 32 {
		errs = append(errs, errors.New("BRIDGE_SECRET_KEY must be 32 random bytes in base64 (openssl rand -base64 32)"))
	}
	c.SecretKey = key
	switch c.DefaultTarget {
	case "ask", "eli", "saved", "both":
	default:
		errs = append(errs, fmt.Errorf("BRIDGE_DEFAULT_TARGET %q: use ask, eli, saved or both", c.DefaultTarget))
	}
	if _, err := time.ParseDuration(c.PollDuration); err != nil {
		errs = append(errs, fmt.Errorf("BRIDGE_POLL_DURATION: %v", err))
	}
	if c.TelegramLocal, err = boolEnv("TELEGRAM_LOCAL_MODE", false); err != nil {
		errs = append(errs, err)
	}
	if c.ForwardSource, err = boolEnv("BRIDGE_FORWARD_SOURCE", true); err != nil {
		errs = append(errs, err)
	}
	if c.AlbumWait, err = time.ParseDuration(env("BRIDGE_ALBUM_WAIT", "1500ms")); err != nil {
		errs = append(errs, fmt.Errorf("BRIDGE_ALBUM_WAIT: %v", err))
	}
	c.Limits = Limits{
		EliText:        intEnv("ELI_MAX_TEXT_CHARS", 500, &errs),
		EliThreadParts: intEnv("ELI_MAX_THREAD_PARTS", 25, &errs),
		EliAlbum:       intEnv("ELI_MAX_ALBUM_ITEMS", 8, &errs),
		EliPollOptions: intEnv("ELI_POLL_MAX_OPTIONS", 4, &errs),
		ChatText:       intEnv("CHAT_MAX_TEXT_CHARS", 4096, &errs),
		ChatCaption:    intEnv("CHAT_MAX_CAPTION_CHARS", 1024, &errs),
		ChatAlbum:      intEnv("CHAT_MAX_ALBUM_ITEMS", 10, &errs),
	}
	return c, errors.Join(errs...)
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func boolEnv(k string, def bool) (bool, error) {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q is not true/false", k, v)
	}
	return b, nil
}

func intEnv(k string, def int, errs *[]error) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a positive number", k, v))
		return def
	}
	return n
}
