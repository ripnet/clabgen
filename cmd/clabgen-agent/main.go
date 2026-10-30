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
	"github.com/ripnet/clabgen/internal/state"
	"github.com/ripnet/clabgen/internal/version"
	"github.com/ripnet/clabgen/internal/webui"

	// Protocol plugins register themselves.
	_ "github.com/ripnet/clabgen/protocols/bgp"
	_ "github.com/ripnet/clabgen/protocols/cfm"
	_ "github.com/ripnet/clabgen/protocols/dhcp"
	_ "github.com/ripnet/clabgen/protocols/icmp"
	_ "github.com/ripnet/clabgen/protocols/isis"
	_ "github.com/ripnet/clabgen/protocols/l2stream"
	_ "github.com/ripnet/clabgen/protocols/lldp"
	_ "github.com/ripnet/clabgen/protocols/ospf"
	_ "github.com/ripnet/clabgen/protocols/packet"
	_ "github.com/ripnet/clabgen/protocols/udp"
	_ "github.com/ripnet/clabgen/protocols/udpecho"
)

func main() {
	listen := flag.String("listen", ":8080", "API listen address")
	portsRE := flag.String("ports", `^eth[1-9][0-9]*$`, "regexp of interfaces to adopt as data ports")
	scenario := flag.String("scenario", envOr("CLABGEN_SCENARIO", ""), "YAML scenario to apply at startup (env CLABGEN_SCENARIO); a relative path is relative to the topology's folder in containerlab")
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

	// The config survives lab restarts: a saved one (clab-<lab>/<node>/
	// clabgen.yml) wins over the topology's scenario, which seeds a fresh lab.
	spath, shost, why := state.Locate(os.Getenv)
	if spath == "" {
		log.Info("config not saved across restarts", "why", why)
	}
	scen, scenHost := state.Scenario(*scenario, os.Getenv)
	store := state.New(eng, log, spath, shost, why, scen, scenHost)
	store.Load()
	saved := make(chan struct{})
	go func() {
		store.Run(ctx, 2*time.Second) // saves once more when stopping
		close(saved)
	}()

	go func() {
		if err := pm.Run(ctx); err != nil {
			log.Error("port manager stopped", "err", err)
			stop()
		}
	}()

	api := http.NewServeMux()
	srv := server.New(eng, pm)
	srv.SetState(store)
	path, h := clabgenv1connect.NewAgentServiceHandler(srv)
	api.Handle(path, h)
	api.Handle("/capture.pcapng", srv.CaptureHandler())

	// The UI reaches this agent as "self" without a network hop and can be
	// pointed at other agents by address.
	host, _ := os.Hostname()
	reg := webui.NewRegistry()
	reg.AddLocal("self", host, api)

	mux := http.NewServeMux()
	mux.Handle(path, h)
	mux.Handle("/capture.pcapng", srv.CaptureHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	if *profile {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	mux.Handle("/", webui.Handler(reg, webui.Options{AllowManual: true, Version: version.Version}))
	hs := &http.Server{Addr: *listen, Handler: h2c.NewHandler(mux, &http2.Server{}), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Info("clabgen-agent starting", "version", version.Version, "listen", *listen, "ports", *portsRE)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	<-saved // the last changes are on disk before the container stops
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
