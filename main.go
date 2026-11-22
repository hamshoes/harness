// Command harness owns Claude Code sessions on the box and serves them over a Unix socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/hamshoes/harness/internal/api"
	"github.com/hamshoes/harness/internal/approvals"
	"github.com/hamshoes/harness/internal/sessions"
	"github.com/hamshoes/harness/internal/store"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness serve [--socket PATH] [--data DIR] [--spool DIR] [--http ADDR] [--permission-mode MODE] [--debug]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		usage()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness:", err)
		os.Exit(1)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	socket := fs.String("socket", filepath.Join(home, ".local", "state", "harness", "harness.sock"), "Unix socket to serve the API on")
	data := fs.String("data", filepath.Join(home, ".local", "share", "harness"), "data directory (harness.db, blobs/)")
	spool := fs.String("spool", "", "notify spool directory (default: $BOX_NOTIFY_SPOOL, else /etc/box/platform.env)")
	httpAddr := fs.String("http", "", "also serve the API and the web page on this TCP address (e.g. :8085); empty: socket only")
	mode := fs.String("permission-mode", "bypassPermissions", "Claude Code --permission-mode for every turn")
	debugLog := fs.Bool("debug", false, "debug logging (includes prompt and message bodies)")
	fs.Parse(os.Args[2:])
	if fs.NArg() > 0 {
		usage()
	}
	level := slog.LevelInfo
	if *debugLog {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := serve(log, *socket, *data, *spool, *httpAddr, *mode); err != nil {
		log.Error("harness serve", "err", err)
		os.Exit(1)
	}
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func serve(log *slog.Logger, socket, data, spoolFlag, httpAddr, mode string) error {
	for _, bin := range []string{"claude", "agentic"} {
		if _, err := exec.LookPath(bin); err != nil {
			return fmt.Errorf("%s is not on PATH", bin)
		}
	}
	spool, err := approvals.FindSpool(spoolFlag)
	if err != nil {
		return err
	}
	if err := approvals.CheckSpool(spool); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	cancel()
	if err != nil {
		return fmt.Errorf("claude --version: %w", err)
	}
	claudeVersion := strings.TrimSpace(string(out))

	// The socket first: a second server must fail before it touches the data.
	ln, err := listen(socket)
	if err != nil {
		return err
	}
	defer os.Remove(socket)
	listeners := []net.Listener{ln}
	closeAll := func() {
		for _, l := range listeners {
			l.Close()
		}
	}
	if httpAddr != "" {
		tcp, err := net.Listen("tcp", httpAddr)
		if err != nil {
			ln.Close()
			return fmt.Errorf("--http: %w", err)
		}
		listeners = append(listeners, tcp)
	}

	if err := os.MkdirAll(data, 0o700); err != nil {
		closeAll()
		return err
	}
	st, err := store.Open(data)
	if err != nil {
		closeAll()
		return fmt.Errorf("opening store: %w", err)
	}
	defer st.Close()

	mgr := sessions.New(st, mode, log)
	if err := mgr.Recover(); err != nil {
		closeAll()
		return fmt.Errorf("recovering sessions: %w", err)
	}
	watcher := approvals.New(spool, st, mgr, log)
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watched := make(chan struct{})
	go func() { watcher.Run(watchCtx); close(watched) }()

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Handler:           (&api.Server{Sessions: mgr, Store: st, Version: version(), ClaudeVersion: claudeVersion, Spool: spool, Log: log}).Handler(),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	served := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() { served <- srv.Serve(l) }()
	}
	if httpAddr != "" {
		log.Info("serving http", "addr", listeners[1].Addr().String())
	}
	log.Info("serving", "socket", socket, "data", data, "spool", spool, "permission_mode", mode, "claude", claudeVersion)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigs:
		log.Info("shutting down", "signal", sig.String())
	case err := <-served:
		log.Error("server stopped", "err", err)
	}
	signal.Stop(sigs)

	// Stop accepting, end the event streams, then interrupt running turns.
	cancelBase()
	shutCtx, cancelShut := context.WithTimeout(context.Background(), 5*time.Second)
	go srv.Shutdown(shutCtx)
	mgr.Shutdown()
	stopWatch() // after the turns, so withdrawals during shutdown are recorded when seen
	<-watched
	cancelShut()
	srv.Close()
	log.Info("stopped")
	return nil
}

// listen opens the Unix socket: its directory created 0700, the socket 0600. A stale socket is removed; one a
// live process answers on is an error.
func listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("socket %s is in use by a running server", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false) // removed by serve, after shutdown
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
