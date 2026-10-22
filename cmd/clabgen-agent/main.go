//go:build linux

// clabgen-agent runs inside a containerlab node and emulates protocols on
// the node's data interfaces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/ripnet/clabgen/gen/clabgen/v1/clabgenv1connect"
	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/events"
	"github.com/ripnet/clabgen/internal/port"
	"github.com/ripnet/clabgen/internal/server"
	"github.com/ripnet/clabgen/internal/version"
	"github.com/ripnet/clabgen/internal/webui"

	// Protocol plugins register themselves.
	_ "github.com/ripnet/clabgen/protocols/bgp"
	_ "github.com/ripnet/clabgen/protocols/cfm"
	_ "github.com/ripnet/clabgen/protocols/icmp"
	_ "github.com/ripnet/clabgen/protocols/isis"
	_ "github.com/ripnet/clabgen/protocols/lldp"
	_ "github.com/ripnet/clabgen/protocols/udp"
	_ "github.com/ripnet/clabgen/protocols/udpecho"
)

func main() {
	listen := flag.String("listen", ":8080", "API listen address")
	portsRE := flag.String("ports", `^eth[1-9][0-9]*$`, "regexp of interfaces to adopt as data ports")
	scenario := flag.String("scenario", envOr("CLABGEN_SCENARIO", ""), "YAML scenario to apply at startup (env CLABGEN_SCENARIO)")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print version and exit")
	profile := flag.Bool("pprof", envOr("CLABGEN_PPROF", "") != "", "serve Go profiling at /debug/pprof/ on the API port (env CLABGEN_PPROF)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "invalid -log-level:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	re, err := regexp.Compile(*portsRE)
	if err != nil {
		log.Error("invalid -ports regexp", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	bus := events.NewBus()
	var eng *core.Engine
	pm := port.New(re, log,
		func(p string, b []byte, ts time.Time) bool { return eng.Receive(p, b, ts) },
		func(e port.Event) { eng.PublishPortEvent(e.Port, e.Kind, e.Message) })
	eng = core.New(pm, bus, log)

	if *scenario != "" {
		b, err := os.ReadFile(*scenario)
		if err != nil {
			log.Error("read scenario", "err", err)
			os.Exit(1)
		}
		sc, err := core.ParseScenario(b)
		if err != nil {
			log.Error("parse scenario", "err", err)
			os.Exit(1)
		}
		if err := eng.Apply(sc, false); err != nil {
			log.Error("scenario applied with errors", "err", err)
		} else {
			log.Info("scenario applied", "file", *scenario)
		}
	}

	go func() {
		if err := pm.Run(ctx); err != nil {
			log.Error("port manager stopped", "err", err)
			stop()
		}
	}()

	api := http.NewServeMux()
	path, h := clabgenv1connect.NewAgentServiceHandler(server.New(eng, pm))
	api.Handle(path, h)

	// The UI reaches this agent as "self" without a network hop and can be
	// pointed at other agents by address.
	host, _ := os.Hostname()
	reg := webui.NewRegistry()
	reg.AddLocal("self", host, api)

	mux := http.NewServeMux()
	mux.Handle(path, h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	if *profile {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	mux.Handle("/", webui.Handler(reg, webui.Options{AllowManual: true}))
	srv := &http.Server{Addr: *listen, Handler: h2c.NewHandler(mux, &http2.Server{}), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("clabgen-agent starting", "version", version.Version, "listen", *listen, "ports", *portsRE)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
