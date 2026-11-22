// Command harness owns Claude Code sessions on the box and serves them over a Unix socket.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness serve [--socket PATH] [--data DIR] [--permission-mode MODE] [--debug]")
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
	mode := fs.String("permission-mode", "bypassPermissions", "Claude Code --permission-mode for every turn")
	debug := fs.Bool("debug", false, "debug logging (includes prompt and message bodies)")
	fs.Parse(os.Args[2:])
	if fs.NArg() > 0 {
		usage()
	}
	_, _, _, _ = socket, data, mode, debug
}
