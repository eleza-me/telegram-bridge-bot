// Command tgbridge runs the Telegram → Eleza bridge bot.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/eleza-me/telegram-bridge-bot/internal/bridge"
	"github.com/eleza-me/telegram-bridge-bot/internal/config"
	"github.com/eleza-me/telegram-bridge-bot/internal/eleza"
	"github.com/eleza-me/telegram-bridge-bot/internal/store"
	"github.com/eleza-me/telegram-bridge-bot/internal/telegram"
)

var version = "0.1.0"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("tgbridge", version)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(2)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "state.enc"), cfg.SecretKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if st.Get().Target == "" {
		_ = st.Update(func(s *store.State) { s.Target = cfg.DefaultTarget })
	}

	tg := telegram.New(cfg.TelegramAPI, cfg.TelegramToken, cfg.TelegramLocal)
	el := eleza.New(cfg.ElezaAPI, st)
	el.ClientHeader, el.DeviceName, el.AppVersion, el.Log = cfg.ClientHeader, cfg.DeviceName, "tgbridge/"+version, log

	bot := &bridge.Bot{
		Cfg:   cfg,
		TG:    tg,
		Acct:  el,
		Store: st,
		Log:   log,
		Pub: &bridge.Publisher{
			TG: tg, EL: el, Limits: cfg.Limits, PollDuration: cfg.PollDuration,
			TmpDir: filepath.Join(cfg.DataDir, "tmp"), Log: log,
		},
	}
	el.OnLoggedOut = bot.SessionLost
	// Downloads left behind by a crash.
	_ = os.RemoveAll(filepath.Join(cfg.DataDir, "tmp"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("tgbridge started", "version", version, "eleza", cfg.ElezaAPI, "logged_in", st.Get().LoggedIn())
	if err := bot.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}
