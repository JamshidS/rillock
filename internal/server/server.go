// Package server runs an HTTP handler until its context is cancelled, then
// shuts down gracefully. Routes live in package api.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const (
	// shutdownTimeout is how long in-flight requests get to finish on shutdown.
	shutdownTimeout = 10 * time.Second
	// readHeaderTimeout stops clients that open a connection and send nothing
	// (a "slowloris" attack) from holding it open forever.
	readHeaderTimeout = 5 * time.Second
)

// Run serves handler on ln until ctx is cancelled, then stops accepting
// connections and waits up to shutdownTimeout for in-flight requests.
func Run(ctx context.Context, ln net.Listener, handler http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		// Request handlers inherit ctx, so shutting down also tells them to stop.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("server listening", "addr", ln.Addr().String())
		errc <- srv.Serve(ln)
	}()

	select {
	case err := <-errc:
		return err // Serve failed before we were asked to stop
	case <-ctx.Done():
	}

	log.Info("server shutting down")
	// ctx is already cancelled, so shutdown needs a context of its own.
	// WithoutCancel keeps ctx's values but not its cancellation.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
