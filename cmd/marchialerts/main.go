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
	amStub := &am.Stub{} // PR3: records PutAlerts; real groups land in PR4
	ev := &engine.Evaluator{
		Rules:  rules,
		Store:  store,
		States: engine.NewStateManager(),
		Sender: &engine.Sender{AM: amStub},
		Log:    log,
	}

	evalCtx, stopEval := context.WithCancel(context.Background())
	defer stopEval()
	go ev.Run(evalCtx, time.Duration(cfg.EvalInterval))
	log.Info("eval loop running (PR3: transitions → PutAlerts via am.Stub; groups + notify pipeline in PR4)")

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
