package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	login := flag.Bool("login", false, "repeat Google OAuth login before serving")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *login); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run(ctx context.Context, login bool) error {
	env, err := readEnv(".env")
	if err != nil {
		return err
	}
	key := env.values["AGY_API_KEY"]
	if key == "" {
		random, err := randomString()
		if err != nil {
			return err
		}
		key = "agy_" + random
		if err := env.save(map[string]string{"AGY_API_KEY": key}); err != nil {
			return err
		}
	}
	fmt.Println("API key:", key)

	client := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	oauth := newOAuth(env, client)
	if login || oauth.tokens.Refresh == "" {
		if err := oauth.login(ctx); err != nil {
			return err
		}
	}
	addr := env.config("AGY_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	upstream := &antigravity{
		client:     client,
		auth:       oauth,
		baseURL:    "https://daily-cloudcode-pa.googleapis.com",
		projectURL: "https://cloudcode-pa.googleapis.com",
		project:    env.config("ANTIGRAVITY_PROJECT_ID"),
	}
	server := &http.Server{
		Handler:           proxy{key: key, upstream: upstream}.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("Base URL: http://%s/v1\n", listener.Addr())
	discoveryDone := make(chan struct{})
	go func() {
		defer close(discoveryDone)
		discovery, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		models, err := upstream.models(discovery)
		if err != nil {
			log.Printf("Could not list models on startup: %s", upstreamLogError(err))
			return
		}
		fmt.Println("Available models:")
		for _, model := range models {
			fmt.Println(" -", model)
		}
	}()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		<-stopped
		<-discoveryDone
		return nil
	}
	return err
}

func randomString() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
