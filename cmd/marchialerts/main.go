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

	"marchialerts/am"
	"marchialerts/engine"
	"marchialerts/metrics"
	"marchialerts/notify"
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

	rules, err := engineRules(cfg.Rules)
	if err != nil {
		return err
	}

	store := metrics.NewStore()

	// PR5: real contact points. Each configured adapter is wrapped in a
	// bounded-retry stage; the fanout delivers every group payload to all
	// of them (AM FanoutStage). Adapters stay dumb — no grouping here (H4).
	var points notify.Fanout
	for _, cp := range cfg.ContactPoints {
		switch {
		case cp.Stdout != nil:
			points = append(points, notify.NewStdout(log))
			log.Info("contact point configured", "name", cp.Name, "type", "stdout")
		case cp.Webhook != nil:
			points = append(points, notify.NewRetry(notify.NewWebhook(cp.Name, cp.Webhook.URL)))
			log.Info("contact point configured", "name", cp.Name, "type", "webhook", "url", cp.Webhook.URL)
		default:
			log.Warn("contact point has no adapter config, skipped", "name", cp.Name)
		}
	}

	dispatcher := am.NewDispatcher(am.RouteOpts{
		Receiver:       "default",
		GroupBy:        cfg.GroupBy,
		GroupWait:      time.Duration(cfg.GroupWait),
		GroupInterval:  time.Duration(cfg.GroupInterval),
		RepeatInterval: time.Duration(cfg.RepeatInterval),
	}, points)

	ev := &engine.Evaluator{
		Rules:  rules,
		Store:  store,
		States: engine.NewStateManager(),
		Sender: &engine.Sender{AM: dispatcher},
		Log:    log,
	}

	evalCtx, stopEval := context.WithCancel(context.Background())
	defer stopEval()
	go ev.Run(evalCtx, time.Duration(cfg.EvalInterval))
	go func() {
		// AM flush ticker: checks every group's next flush time.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-evalCtx.Done():
				return
			case <-t.C:
				dispatcher.Tick()
			}
		}
	}()
	log.Info("eval loop + AM dispatcher running (PR5: stdout/webhook contact points + retry; silences in PR6)")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("POST /api/v1/import", metrics.ImportHandler(store, time.Now))

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
		stopEval()
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
