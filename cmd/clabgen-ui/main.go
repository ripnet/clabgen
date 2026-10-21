// clabgen-ui serves the web UI for many agents. Run it on the lab host (or
// anywhere that can reach the agents' management addresses); it discovers
// clabgen containers and proxies the browser's API calls to them.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ripnet/clabgen/internal/version"
	"github.com/ripnet/clabgen/internal/webui"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var agents, labs multi
	listen := flag.String("listen", "127.0.0.1:8090", "listen address (use an SSH tunnel to reach it remotely)")
	flag.Var(&agents, "agent", "agent as [name=]host[:port]; repeatable")
	flag.Var(&labs, "lab", "containerlab topology file or lab directory to discover agents from; repeatable")
	docker := flag.String("docker", "auto", "discover running containerlab containers via the Docker socket: auto, on or off")
	image := flag.String("image", "clabgen", "regexp matched against container images during discovery")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	imageRE, err := regexp.Compile(*image)
	if err != nil {
		log.Error("invalid -image", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := webui.NewRegistry()
	var static []webui.Agent
	for _, a := range agents {
		name, addr, ok := strings.Cut(a, "=")
		if !ok {
			name, addr = strings.Split(a, ":")[0], a
		}
		static = append(static, webui.Agent{ID: name, Name: name, Lab: "static", URL: addr})
	}
	if err := reg.Replace("static", static); err != nil {
		log.Error("invalid -agent", "err", err)
		os.Exit(2)
	}
	for _, l := range labs {
		go webui.DiscoverLab(ctx, reg, l, imageRE, 5*time.Second, log)
	}
	sock := webui.DockerSocket()
	useDocker := *docker == "on"
	if *docker == "auto" {
		_, err := os.Stat(sock)
		useDocker = err == nil && len(agents) == 0 && len(labs) == 0
	}
	if useDocker {
		go webui.DiscoverDocker(ctx, reg, sock, imageRE, 5*time.Second, log)
	}

	srv := &http.Server{Addr: *listen, Handler: webui.Handler(reg, webui.Options{AllowManual: true}), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("clabgen-ui listening", "url", "http://"+*listen, "docker_discovery", useDocker, "docker_socket", sock, "labs", labs.String())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
}
