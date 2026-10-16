package main

import (
	"context"
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed assets
var assets embed.FS

func main() {
	users := os.Getenv("AUTH_USERS")
	if users == "" {
		log.Fatal("AUTH_USERS is required")
	}
	path := os.Getenv("DB_PATH")
	if path == "" {
		path = "./data/kahut.db"
	}
	path, err := filepath.Abs(path)
	if err != nil {
		log.Fatal(err)
	}
	s, err := openRepository(path, os.Getenv("SESSION_SIGNING_KEY"))
	if err != nil {
		log.Fatalf("open database %q: %v", path, err)
	}
	defer s.Close()
	log.Printf("using database %q", path)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := newApp(ctx, s, users)
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
