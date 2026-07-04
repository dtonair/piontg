package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"piontg/authz"
	"piontg/config"
	"piontg/folders"
	"piontg/session"
	"piontg/store"
	piontggram "piontg/telegram"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "piontg: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("piontg", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to YAML config file")
	dryRun := fs.Bool("dry-run", false, "validate config/state setup and exit")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	stateStore := store.New(cfg.State.Dir)
	state, stateErr := stateStore.Load()
	if stateErr != nil {
		logger.Warn("state load warning", "error", stateErr)
	}

	policy, err := folders.NewPolicy(*cfg)
	if err != nil {
		return err
	}

	if *dryRun {
		logger.Info("configuration validated",
			"state_dir", cfg.State.Dir,
			"state_file", stateStore.Path(),
			"pi_binary", cfg.Pi.Binary,
			"allowed_roots", len(cfg.Folders.Roots),
			"has_selected_folder", state.SelectedFolder != "",
		)
		return nil
	}

	manager, err := session.NewManager(*cfg, policy, stateStore, nil)
	if err != nil {
		return err
	}
	bot, err := piontggram.NewRealBot(cfg.Telegram.Token)
	if err != nil {
		return err
	}
	messenger := piontggram.NewMessengerAdapter(bot)
	handler := piontggram.NewHandler(messenger, manager, policy, authz.New(cfg.Telegram.AllowedUserID), logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("starting telegram polling", "allowed_user_id", cfg.Telegram.AllowedUserID)
	return piontggram.RunPolling(ctx, bot, handler, cfg.Telegram.AllowedUserID, logger)
}

func parseLogLevel(level string) slog.Leveler {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
