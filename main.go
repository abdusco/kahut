package main

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

//go:embed assets
var assets embed.FS

func main() {
	password := os.Getenv("HOST_PASSWORD")
	if password == "" {
		log.Fatal("HOST_PASSWORD is required")
	}
	path := os.Getenv("DB_PATH")
	if path == "" {
		path = "./data/kahut.db"
	}
	s, err := openStore(path)
	if err != nil {
		log.Fatal(err)
	}
	defer s.db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := newApp(ctx, s, password)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("ADDR")
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: a.routes(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go a.timers()
	go func() {
		<-ctx.Done()
		a.closeStreams()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("kahut listening on %s", addr)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
