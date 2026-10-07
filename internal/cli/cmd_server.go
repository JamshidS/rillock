package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/jamshids/rillock/internal/api"
	"github.com/jamshids/rillock/internal/auth"
	"github.com/jamshids/rillock/internal/config"
	"github.com/jamshids/rillock/internal/server"
	"github.com/jamshids/rillock/internal/store"
	"github.com/jamshids/rillock/internal/tools"
	"github.com/jamshids/rillock/internal/version"
	"github.com/jamshids/rillock/internal/worker"
)

// maxWorkers bounds --workers. Each worker runs one run at a time.
const maxWorkers = 64

func runServer(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	workers := fs.Int("workers", 1, "workers executing runs in this process (0 = API only)")
	var listen config.Listen
	var db config.Database
	var keysFile config.Keys
	rest, err := config.Parse(fs, args, a.env, &listen, &db, &keysFile)
	if err != nil {
		return err
	}
	if err := noArgs(rest); err != nil {
		return err
	}
	if *workers < 0 || *workers > maxWorkers {
		return usageErrorf("--workers must be between 0 and %d", maxWorkers)
	}

	// Check everything that can be wrong before listening, so a
	// misconfigured server fails at startup with a clear message.
	keys, err := auth.LoadReloadable(keysFile.File)
	if err != nil {
		return err
	}
	if err := store.CheckMigrated(ctx, db.URL); err != nil {
		return err
	}
	st, err := store.Open(ctx, db.URL)
	if err != nil {
		return err
	}
	defer st.Close()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen.Addr)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(a.stderr, nil))
	log.Info("starting rillock", "version", version.Version, "keys", keys.Count())

	// `kill -HUP <pid>` makes the server re-read its keys file.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go reloadOnSignal(ctx, hup, keys, log)

	// Workers stop when ctx is cancelled. They must stop before the deferred
	// st.Close runs, since they use the store: so cancel, then wait.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	exec := tools.NewHTTPExecutor(tools.DefaultTimeout)
	host, _ := os.Hostname() // an empty hostname still gives usable IDs
	for i := range *workers {
		w := worker.New(st, exec, log, worker.Config{ID: fmt.Sprintf("%s/%d/%d", host, os.Getpid(), i+1)})
		wg.Go(func() { w.Run(ctx) })
	}

	err = server.Run(ctx, ln, api.New(st, keys, log).Handler(), log)
	cancel() // if the server failed on its own, stop the workers too
	wg.Wait()
	return err
}

// reloadOnSignal reloads keys each time a value arrives on sig, until ctx is
// cancelled. It takes a channel rather than calling signal.Notify itself, so
// tests can trigger a reload without sending a real signal.
func reloadOnSignal(ctx context.Context, sig <-chan os.Signal, keys *auth.ReloadableKeys, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return // the server is shutting down; without this the goroutine would leak
		case <-sig:
			if err := keys.Reload(); err != nil {
				log.Error("reloading keys failed; still using the previous keys", "err", err)
				continue
			}
			log.Info("reloaded keys", "keys", keys.Count())
		}
	}
}

func runMigrate(ctx context.Context, a *app, fs *flag.FlagSet, args []string) error {
	var db config.Database
	rest, err := config.Parse(fs, args, a.env, &db)
	if err != nil {
		return err
	}
	if err := noArgs(rest); err != nil {
		return err
	}

	applied, err := store.Migrate(ctx, db.URL)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(a.stdout, "database schema is up to date")
		return nil
	}
	for _, v := range applied {
		fmt.Fprintf(a.stdout, "applied migration %05d\n", v)
	}
	return nil
}

func runVersion(_ context.Context, a *app, fs *flag.FlagSet, args []string) error {
	rest, err := config.Parse(fs, args, a.env)
	if err != nil {
		return err
	}
	if err := noArgs(rest); err != nil {
		return err
	}
	fmt.Fprintln(a.stdout, version.Version)
	return nil
}
