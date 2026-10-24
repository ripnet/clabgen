package main

// Wireshark integration: clabgenctl as a Wireshark extcap program (see
// extcap(4)). Installed into Wireshark's personal extcap folder, it lists
// each clabgen agent's ports as capture interfaces, offers the capture's
// filters as interface options, and streams an agent's /capture.pcapng into
// the pipe Wireshark reads.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
	"github.com/ripnet/clabgen/gen/clabgen/v1/clabgenv1connect"
)

// extcapConfig says where the agents are: a clabgen-ui, a containerlab lab
// on this host, or agent addresses. Wireshark starts extcap programs
// without the shell's environment, so it lives in a file.
type extcapConfig struct {
	UI     string   `json:"ui,omitempty"`
	Lab    string   `json:"lab,omitempty"`
	Agents []string `json:"agents,omitempty"`
}

func extcapConfigPath() string {
	if p := os.Getenv("CLABGEN_WIRESHARK_CONFIG"); p != "" {
		return p // tests
	}
	d, err := os.UserConfigDir()
	if err != nil {
		d = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(d, "clabgen", "wireshark.json")
}

func loadExtcapConfig() (extcapConfig, error) {
	var c extcapConfig
	b, err := os.ReadFile(extcapConfigPath())
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// extcapDir is Wireshark's personal extcap folder (Help → About Wireshark →
// Folders shows it).
func extcapDir() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "Wireshark", "extcap")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "lib", "wireshark", "extcap")
}

func extcapName() string {
	if runtime.GOOS == "windows" {
		return "clabgen.exe"
	}
	return "clabgen"
}

// isExtcap reports whether Wireshark is calling us as an extcap program.
func isExtcap(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--extcap-") || a == "--capture" {
			return true
		}
	}
	return false
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// An extcap interface: an agent's port (or all of them).
type xIface struct {
	value   string // clabgen.<lab-node>.<port|all>
	display string
	base    string // the agent's URL (direct, or through clabgen-ui)
	port    string // "" for all ports
}

type xAgent struct{ name, base string }

func (c extcapConfig) agents() ([]xAgent, error) {
	var out []xAgent
	switch {
	case c.UI != "":
		ui := strings.TrimRight(c.UI, "/")
		cl := &http.Client{Timeout: 3 * time.Second}
		r, err := cl.Get(ui + "/api/agents")
		if err != nil {
			return nil, err
		}
		defer r.Body.Close()
		var l struct {
			Agents []struct{ ID, Name, Lab string }
		}
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			return nil, fmt.Errorf("%s: not a clabgen-ui: %w", c.UI, err)
		}
		for _, a := range l.Agents {
			n := a.Name
			if a.Lab != "" {
				n = a.Lab + "/" + a.Name
			}
			out = append(out, xAgent{n, ui + "/agents/" + url.PathEscape(a.ID)})
		}
	case c.Lab != "":
		lab, _, err := labOf(c.Lab)
		if err != nil {
			return nil, err
		}
		nodes, err := labNodes(lab)
		if err != nil {
			return nil, err
		}
		for n, ip := range nodes {
			out = append(out, xAgent{lab + "/" + n, "http://" + ip + ":8080"})
		}
	default:
		for _, a := range c.Agents {
			out = append(out, xAgent{a, baseURL(a)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func baseURL(addr string) string {
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	if host := strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://"); !strings.Contains(host, ":") {
		addr += ":8080"
	}
	return addr
}

func agentClient(base string) clabgenv1connect.AgentServiceClient {
	return clabgenv1connect.NewAgentServiceClient(http.DefaultClient, base)
}

// interfaces lists every reachable agent's ports, agents in parallel.
func (c extcapConfig) interfaces() ([]xIface, error) {
	agents, err := c.agents()
	if err != nil {
		return nil, err
	}
	lists := make([][]xIface, len(agents))
	var wg sync.WaitGroup
	for i, a := range agents {
		wg.Add(1)
		go func(i int, a xAgent) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			r, err := agentClient(a.base).ListPorts(ctx, connect.NewRequest(&v1.ListPortsRequest{}))
			if err != nil {
				return // not reachable, or not an agent
			}
			l := []xIface{{display: fmt.Sprintf("clabgen %s: all ports", a.name), base: a.base}}
			for _, p := range r.Msg.Ports {
				l = append(l, xIface{display: fmt.Sprintf("clabgen %s: %s", a.name, p.Name), base: a.base, port: p.Name})
			}
			lists[i] = l
		}(i, a)
	}
	wg.Wait()
	var out []xIface
	for i, l := range lists {
		for _, x := range l {
			p := x.port
			if p == "" {
				p = "all"
			}
			// Stable names (Wireshark remembers interfaces by them).
			x.value = "clabgen." + safeName.ReplaceAllString(agents[i].name, "-") + "." + p
			out = append(out, x)
		}
	}
	return out, nil
}

func extcapArgs(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			continue
		}
		k, v, ok := strings.Cut(a[2:], "=")
		if !ok && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			v = args[i+1]
			i++
		}
		m[k] = v
	}
	return m
}

// runExtcap answers Wireshark; the exit code is what Wireshark sees.
func runExtcap(args []string) int {
	a := extcapArgs(args)
	cfg, err := loadExtcapConfig()
	if err != nil {
		if _, ok := a["extcap-interfaces"]; ok {
			// Nothing configured: no interfaces (Wireshark keeps quiet).
			fmt.Println("extcap {version=1}{help=https://clabgen.dev/cli/#wireshark}{display=clabgen}")
			return 0
		}
		fmt.Fprintln(os.Stderr, "clabgen: not set up: run clabgenctl wireshark install")
		return 1
	}
	find := func() (xIface, error) {
		ifs, err := cfg.interfaces()
		if err != nil {
			return xIface{}, err
		}
		for _, x := range ifs {
			if x.value == a["extcap-interface"] {
				return x, nil
			}
		}
		return xIface{}, fmt.Errorf("clabgen: no interface %q (is the agent up?)", a["extcap-interface"])
	}
	_, capture := a["capture"]
	switch {
	case has(a, "extcap-interfaces"):
		fmt.Println("extcap {version=1}{help=https://clabgen.dev/cli/#wireshark}{display=clabgen}")
		ifs, err := cfg.interfaces()
		if err != nil {
			fmt.Fprintln(os.Stderr, "clabgen:", err)
			return 0 // keep Wireshark's list working
		}
		for _, x := range ifs {
			fmt.Printf("interface {value=%s}{display=%s}\n", x.value, x.display)
		}
	case has(a, "extcap-dlts"):
		fmt.Println("dlt {number=1}{name=EN10MB}{display=Ethernet}")
	case has(a, "extcap-config"):
		x, err := find()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println("arg {number=0}{call=--device}{display=Device}{type=selector}{tooltip=Only frames to or from this device (its MAC or multicast, on its VLANs)}")
		fmt.Println("value {arg=0}{value=}{display=any}{default=true}")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if r, err := agentClient(x.base).ListDevices(ctx, connect.NewRequest(&v1.ListDevicesRequest{})); err == nil {
			for _, d := range r.Msg.Devices {
				if x.port == "" || d.Port == x.port {
					fmt.Printf("value {arg=0}{value=%s}{display=%s (%s %s)}\n", d.Name, d.Name, d.Port, strings.Join(vlanList(d), ","))
				}
			}
		}
		fmt.Println("arg {number=1}{call=--direction}{display=Direction}{type=selector}")
		fmt.Println("value {arg=1}{value=}{display=both}{default=true}")
		fmt.Println("value {arg=1}{value=rx}{display=received}")
		fmt.Println("value {arg=1}{value=tx}{display=sent}")
		fmt.Println("arg {number=2}{call=--ethertype}{display=Ethertypes}{type=string}{tooltip=Only these, after any VLAN tags: e.g. 0x0800,0x86dd (empty: all)}")
		fmt.Println("arg {number=3}{call=--snaplen}{display=Snap length}{type=unsigned}{default=0}{tooltip=Bytes kept per frame (0: all)}")
	case has(a, "extcap-capture-filter") && !capture:
		if a["extcap-capture-filter"] != "" {
			fmt.Println("clabgen captures take no capture filter: use the interface options (device, direction, ethertypes), or a display filter")
		}
	case capture:
		x, err := find()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := extcapCapture(x, a); err != nil {
			fmt.Fprintln(os.Stderr, "clabgen:", err)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "clabgen: unknown extcap call", args)
		return 1
	}
	return 0
}

func has(m map[string]string, k string) bool { _, ok := m[k]; return ok }

// extcapCapture copies the agent's live pcapng stream into Wireshark's pipe
// until Wireshark stops the capture (a signal, or the pipe closing).
func extcapCapture(x xIface, a map[string]string) error {
	q := url.Values{}
	if x.port != "" {
		q.Set("port", x.port)
	}
	for _, k := range []string{"device", "direction", "snaplen"} {
		if v := a[k]; v != "" && v != "0" {
			q.Set(k, v)
		}
	}
	for _, e := range strings.Split(a["ethertype"], ",") {
		if e = strings.TrimSpace(e); e != "" {
			q.Add("ethertype", e)
		}
	}
	if a["fifo"] == "" {
		return errors.New("no --fifo")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, "GET", x.base+"/capture.pcapng?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(r.Body, 500))
		return fmt.Errorf("%s: %s", r.Status, strings.TrimSpace(string(msg)))
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-pcapng") {
		return errors.New("the agent has no packet capture (an older clabgen): redeploy it with a newer image")
	}
	f, err := os.OpenFile(a["fifo"], os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, r.Body); err != nil && ctx.Err() == nil && !errors.Is(err, syscall.EPIPE) {
		return err
	}
	return nil
}

func wiresharkCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "wireshark", Short: "Capture from clabgen agents in Wireshark (an extcap plugin)"}
	var ui, lab, dir string
	var agents []string
	install := &cobra.Command{
		Use: "install", Short: "Add clabgen agents' ports to Wireshark's capture interfaces",
		Example: "  clabgenctl wireshark install --ui http://labhost:8090    # the agents a clabgen-ui knows\n" +
			"  clabgenctl wireshark install --lab b2b                   # a containerlab lab on this host\n" +
			"  clabgenctl wireshark install --agent 172.20.20.2 --agent 172.20.20.3",
		RunE: func(cmd *cobra.Command, args []string) error {
			n := 0
			for _, v := range []string{ui, lab} {
				if v != "" {
					n++
				}
			}
			if len(agents) > 0 {
				n++
			}
			if n != 1 {
				return errors.New("give one of --ui, --lab or --agent")
			}
			cfg := extcapConfig{UI: ui, Lab: lab, Agents: agents}
			ifs, err := cfg.interfaces()
			if err != nil {
				return err
			}
			if len(ifs) == 0 {
				fmt.Fprintln(os.Stderr, "warning: no agent answered yet; Wireshark lists them once they do")
			}
			b, _ := json.MarshalIndent(cfg, "", "  ")
			if err := os.MkdirAll(filepath.Dir(extcapConfigPath()), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(extcapConfigPath(), b, 0o644); err != nil {
				return err
			}
			if dir == "" {
				dir = extcapDir()
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			if self, err = filepath.EvalSymlinks(self); err != nil {
				return err
			}
			dst := filepath.Join(dir, extcapName())
			os.Remove(dst)
			if runtime.GOOS == "windows" {
				err = copyFile(self, dst) // symlinks need privileges there
			} else {
				err = os.Symlink(self, dst)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Installed %s → %s\nAgents from %s\n", dst, self, extcapConfigPath())
			for _, x := range ifs {
				fmt.Println("  ", x.display)
			}
			fmt.Println("Restart Wireshark (or Capture → Refresh Interfaces) to see them.")
			return nil
		}}
	install.Flags().StringVar(&ui, "ui", "", "a clabgen-ui URL: its agents")
	install.Flags().StringVar(&lab, "lab", "", "a containerlab lab on this host (name or topology file)")
	install.Flags().StringArrayVar(&agents, "agent", nil, "an agent address; repeatable")
	install.Flags().StringVar(&dir, "dir", "", "Wireshark's extcap folder (default: the personal one, "+extcapDir()+")")
	var udir string
	uninstall := &cobra.Command{
		Use: "uninstall", Short: "Remove clabgen from Wireshark",
		RunE: func(cmd *cobra.Command, args []string) error {
			if udir == "" {
				udir = extcapDir()
			}
			err1 := os.Remove(filepath.Join(udir, extcapName()))
			err2 := os.Remove(extcapConfigPath())
			if err1 != nil && err2 != nil {
				return fmt.Errorf("nothing to remove (%v)", err1)
			}
			fmt.Println("Removed. Restart Wireshark.")
			return nil
		}}
	uninstall.Flags().StringVar(&udir, "dir", "", "Wireshark's extcap folder")
	status := &cobra.Command{
		Use: "status", Short: "Show what Wireshark sees",
		RunE: func(cmd *cobra.Command, args []string) error {
			dst := filepath.Join(extcapDir(), extcapName())
			if t, err := os.Readlink(dst); err == nil {
				fmt.Printf("plugin: %s → %s\n", dst, t)
			} else if _, err := os.Stat(dst); err == nil {
				fmt.Printf("plugin: %s\n", dst)
			} else {
				fmt.Printf("plugin: not installed in %s\n", extcapDir())
			}
			cfg, err := loadExtcapConfig()
			if err != nil {
				return fmt.Errorf("not configured: clabgenctl wireshark install --ui URL | --lab LAB | --agent ADDR")
			}
			b, _ := json.Marshal(cfg)
			fmt.Printf("agents from: %s\n", b)
			ifs, err := cfg.interfaces()
			if err != nil {
				return err
			}
			for _, x := range ifs {
				fmt.Printf("  %-24s %s\n", x.value, x.display)
			}
			return nil
		}}
	cmd.AddCommand(install, uninstall, status)
	return cmd
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
