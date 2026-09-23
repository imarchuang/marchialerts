package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "marchialerts:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listenFlag   = flag.String("httpListenAddr", "", "HTTP listen address (overrides config `listen`)")
		configFlag   = flag.String("config", "", "path to YAML config file")
		evalInterval = flag.Duration("evalInterval", 0, "rule evaluation interval (overrides config `eval_interval`)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := LoadConfig(*configFlag, log)
	if err != nil {
		return err
	}
	if *listenFlag != "" {
		cfg.Listen = *listenFlag
	}
	if *evalInterval != 0 {
		cfg.EvalInterval = Duration(*evalInterval)
	}

	log.Info("starting marchialerts",
		"listen", cfg.Listen,
		"eval_interval", cfg.EvalInterval.String(),
		"group_wait", cfg.GroupWait.String(),
		"group_interval", cfg.GroupInterval.String(),
		"repeat_interval", cfg.RepeatInterval.String(),
		"group_by", cfg.GroupBy,
		"rules", len(cfg.Rules),
		"contact_points", len(cfg.ContactPoints),
	)
	log.Info("PR0 scaffold: only /healthz is served; eval loop lands in PR1, AM pipeline in PR4")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", "signal", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
