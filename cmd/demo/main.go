//go:build demo

// demo serves the page for a made-up org, with reviews in every state, for screenshots and for trying the page
// without GitHub or Claude: go run -tags demo ./cmd/demo. Nothing it does reaches GitHub, and it keeps nothing.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/badam1/personal-review-bot/internal/version"
	"github.com/badam1/personal-review-bot/internal/web"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:12398", "where to serve the page (127.0.0.1 only)")
	ver := flag.String("version", version.String(), "the version the page shows")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("demo page on http://%s (Ctrl+C to stop)\n", strings.Replace(*addr, "127.0.0.1", "localhost", 1))
	if err := web.Demo(ctx, *addr, *ver); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}
