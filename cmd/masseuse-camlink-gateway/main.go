// masseuse-camlink-gateway runs inside the video enclave and terminates the
// connector's tunnel (docs/PROTOCOL.md, sections 4 and 5).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/FemLed/masseuse-camlink/internal/buildinfo"
	"github.com/FemLed/masseuse-camlink/internal/gateway"
)

func main() {
	var (
		ws       = flag.String("ws", "127.0.0.1:8090", "WebSocket listener Caddy proxies /ingest/tunnel to")
		relay    = flag.String("relay", "127.0.0.1:7441", "relay listener the RTSP server dials as the camera")
		control  = flag.String("control", "127.0.0.1:8091", "control API for the pose producer")
		logLevel = flag.String("log-level", "info", "debug, info, warn or error")
		version  = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Version(), buildinfo.GoVersion())
		return
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "bad -log-level:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	logger.Info("masseuse-camlink-gateway", "version", buildinfo.Version(), "go", buildinfo.GoVersion())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := gateway.New(gateway.Config{WSAddr: *ws, RelayAddr: *relay, ControlAddr: *control, Logger: logger})
	if err := srv.Run(ctx); err != nil {
		logger.Error("gateway failed", "err", err)
		os.Exit(1)
	}
}
