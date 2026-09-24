package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/evolvue/nexa-gateway/internal/server"
	"github.com/evolvue/nexa-gateway/internal/store"
)

var version = "dev"

func main() {
	addr := flag.String("addr", env("NEXA_ADDR", ":8080"), "HTTP listen address")
	data := flag.String("data", env("NEXA_DATA", "./data"), "persistent data directory")
	healthcheck := flag.Bool("healthcheck", false, "check the running gateway and exit")
	resetMaster := flag.Bool("reset-master", false, "rotate the master key and exit")
	flag.Parse()
	if *healthcheck {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(env("NEXA_HEALTH_URL", "http://127.0.0.1:8080/healthz"))
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	db, master, err := store.Open(*data)
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	if *resetMaster {
		master, err = db.ResetMasterKey()
		if err != nil {
			log.Error("master-key reset failed", "error", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stdout, "New Nexa master key: %s\n", master)
		fmt.Fprintln(os.Stdout, "Save it now. It will not be printed again.")
		return
	}
	if master != "" {
		fmt.Fprintln(os.Stdout, "\n╭────────────────────────────────────────────────────────────╮")
		fmt.Fprintln(os.Stdout, "│  NEXA GATEWAY · FIRST START                                │")
		fmt.Fprintf(os.Stdout, "│  Master key: %-44s │\n", master)
		fmt.Fprintln(os.Stdout, "│  Save it now. It will not be printed again.                │")
		fmt.Fprintln(os.Stdout, "╰────────────────────────────────────────────────────────────╯")
		fmt.Fprintln(os.Stdout)
	}
	h := server.New(db, log, env("NEXA_SECURE_COOKIES", "") == "true")
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	go func() {
		log.Info("Nexa Gateway ready", "version", version, "address", *addr, "data", *data)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
