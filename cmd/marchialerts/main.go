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

	// PR4: real in-process AM — one default route, aggregation groups with
	// the three timers, nflog dedup. Notifications are logged for now;
	// stdout/webhook adapters land in PR5.
	dispatcher := am.NewDispatcher(am.RouteOpts{
		Receiver:       "default",
		GroupBy:        cfg.GroupBy,
		GroupWait:      time.Duration(cfg.GroupWait),
		GroupInterval:  time.Duration(cfg.GroupInterval),
		RepeatInterval: time.Duration(cfg.RepeatInterval),
	}, am.NotifierFunc(func(groupKey string, alerts []am.PostableAlert) error {
		for _, a := range alerts {
			status := "firing"
			if a.ResolvedAt(time.Now()) {
				status = "resolved"
			}
			log.Info("notify",
				"group", groupKey,
				"fingerprint", a.Fingerprint().String(),
				"status", status,
				"labels", a.Labels,
			)
		}
		return nil
	}))

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
	log.Info("eval loop + AM dispatcher running (PR4: groups + timers + dedup; contact points in PR5)")

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
