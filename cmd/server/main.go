package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"phonepilot/server/internal/config"
	"phonepilot/server/internal/httpapi"
	"phonepilot/server/internal/lan"
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

	mainAddr := net.JoinHostPort(cfg.MainBind, fmt.Sprint(cfg.Port))
	mainListener, err := net.Listen("tcp", mainAddr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	var lanSrv *http.Server
	var lanListener net.Listener
	mdnsStop := make(chan struct{})
	binding, err := lan.Resolve(cfg.LANBind)
	if err != nil {
		log.Printf("LAN disabled: %v", err)
	}
	if binding != nil {
		lanListener, err = net.Listen("tcp", binding.Address)
		if err != nil {
			log.Printf("LAN disabled: cannot listen on %s: %v", binding.Address, err)
		} else {
			lanSrv = &http.Server{Handler: api.LANHandler(binding.Subnet), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
			if err := lan.Advertise(binding, cfg.ServerID, mdnsStop); err != nil {
				log.Printf("DNS-SD disabled: %v", err)
			}
			go func() {
				if err := lanSrv.Serve(lanListener); err != nil && err != http.ErrServerClosed {
					log.Printf("LAN listener stopped: %v", err)
				}
			}()
			log.Printf("LAN listener: http://%s", binding.Address)
		}
	}
	go func() {
		log.Printf("PhonePilot signaling server (Go): http://%s", mainAddr)
		if cfg.MainBind == "0.0.0.0" || cfg.MainBind == "::" {
			log.Printf("WARNING: main listener is reachable beyond loopback; firewall and proxy access must be restricted")
		}
		log.Printf("Users: %s", usernames(cfg.Users))
		log.Printf("Max viewers per camera: %d; topology: Android -> source browser -> relay viewers", cfg.MaxViewersPerCamera)
		if len(cfg.IceServers) < 2 {
			log.Printf("WARNING: TURN not configured; some CGNAT/mobile paths may fail even if signaling works")
		}
		if err := srv.Serve(mainListener); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	close(mdnsStop)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	if lanSrv != nil {
		_ = lanSrv.Shutdown(ctx)
	}
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
