package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/rendezvous"
)

const rendezvousUsage = `factor rendezvous — the meeting point of a swarm

Agents keep one outbound WebSocket to it; it holds presence, pairing
mailboxes and each swarm's roster, and relays bytes between members of the
same roster. Run it behind a TLS proxy and point agents at it with
swarm.rendezvous (wss://...).

Usage:
  factor rendezvous [flags]

Flags:
`

// runRendezvous serves a rendezvous until interrupted.
func runRendezvous(args []string) error {
	fs := flag.NewFlagSet("rendezvous", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8740", "address to serve on, behind the TLS proxy")
	data := fs.String("data", filepath.Join(config.Home(), "rendezvous"), "store directory (each swarm's roster head)")
	create := fs.Bool("init", false, "create the store if it does not exist; without it a missing store refuses roster changes")
	rate := fs.Int("rate", 32<<10, "bytes a second one agent may relay, sustained")
	burst := fs.Int("burst", 256<<10, "bytes one agent may relay ahead of the rate")
	daily := fs.Int64("daily", 15<<20, "bytes one agent may relay in a day")
	work := fs.Int("work-bits", rendezvous.DefaultWorkBits, "proof of work a new key or a mailbox owes, in bits")
	perIP := fs.Int("per-ip", 30, "connections one address may open a minute")
	trustProxy := fs.Bool("trust-proxy", false, "read the client address from X-Forwarded-For")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), rendezvousUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	srv, err := rendezvous.New(rendezvous.Config{Data: *data, Create: *create, RateBytes: *rate, BurstBytes: *burst,
		DailyBytes: *daily, WorkBits: *work, PerIPMinute: *perIP, TrustProxy: *trustProxy})
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signalContext()
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(shutdown)
	}()
	slog.Info("factor rendezvous serving", "listen", *listen, "store", *data)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
