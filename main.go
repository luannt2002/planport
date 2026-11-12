// Command planport serves a local OpenAI-compatible API backed by subscription
// accounts or API keys. Config: env / .env (see internal/config).
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/luannt2002/planport/internal/config"
	"github.com/luannt2002/planport/internal/server"
)

func main() {
	envFile := flag.String("env", ".env", "KEY=VALUE file loaded into env (existing env vars win)")
	flag.Parse()
	explicit := false // an explicit -env must exist; the default may be absent
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "env" })
	if err := config.LoadDotEnv(*envFile, explicit); err != nil {
		log.Fatal(err)
	}
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		log.Printf("warning: %s", w)
	}
	ids := make([]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		ids[i] = p.ID()
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	log.Printf("planport listening on http://%s (providers: %s)", addr, strings.Join(ids, ", "))
	// Header/idle timeouts stop slow clients pinning goroutines; a Read/WriteTimeout would cut long streams.
	srv := &http.Server{Addr: addr, Handler: server.New(cfg.Providers, cfg.APIKey), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	stop() // a second signal exits at once
	// Drain in-flight requests: killing a token refresh between rotation and
	// file write logs the account out.
	log.Printf("shutting down: waiting for in-flight requests (signal again to exit now)")
	_ = srv.Shutdown(context.Background())
	log.Printf("stopped")
}
