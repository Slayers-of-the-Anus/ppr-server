package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"phonepilot/server/internal/config"
	"phonepilot/server/internal/httpapi"
	"phonepilot/server/internal/realtime"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	hub := realtime.NewHub(cfg)
	defer hub.Close()
	api, err := httpapi.New(cfg, hub)
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", cfg.Port), Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		log.Printf("PhonePilot signaling server (Go): http://localhost:%d", cfg.Port)
		log.Printf("Users: %s", usernames(cfg.Users))
		log.Printf("Max viewers per camera: %d; topology: Android -> source browser -> relay viewers", cfg.MaxViewersPerCamera)
		if len(cfg.IceServers) < 2 {
			log.Printf("WARNING: TURN not configured; some CGNAT/mobile paths may fail even if signaling works")
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func usernames(users []config.User) string {
	out := ""
	for i, u := range users {
		if i > 0 {
			out += ", "
		}
		out += u.Username
	}
	return out
}
