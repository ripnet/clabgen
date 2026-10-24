// clabgenctl is the command line client for clabgen agents.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
	"github.com/ripnet/clabgen/gen/clabgen/v1/clabgenv1connect"
	"github.com/ripnet/clabgen/internal/pcapng"
)

var (
	serverAddr string
	outFormat  string
	labArg     string
	timeout    time.Duration
)

func client() clabgenv1connect.AgentServiceClient {
	return clientFor(serverAddr)
}

func clientFor(addr string) clabgenv1connect.AgentServiceClient {
	if labArg != "" && !strings.ContainsAny(addr, ".:/") && addr != "localhost" {
		addr = nodeAddr(labArg, addr)
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	if host := strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://"); !strings.Contains(host, ":") {
		addr += ":8080"
	}
	return clabgenv1connect.NewAgentServiceClient(http.DefaultClient, addr)
}

// labOf reads a lab argument: a lab name, or a topology file (its name
// and container name prefix).
func labOf(arg string) (name, prefix string, err error) {
	prefix = "clab"
	if !strings.HasSuffix(arg, ".yml") && !strings.HasSuffix(arg, ".yaml") {
		return arg, prefix, nil
	}
	b, err := os.ReadFile(arg)
	if err != nil {
		return "", "", err
	}
	var t struct {
		Name   string  `json:"name"`
		Prefix *string `json:"prefix"`
	}
	j, err := yaml.YAMLToJSON(b)
	if err == nil {
		err = json.Unmarshal(j, &t)
	}
	if err != nil || t.Name == "" {
		return "", "", fmt.Errorf("%s: not a containerlab topology (no name)", arg)
	}
	if t.Prefix != nil {
		prefix = *t.Prefix
	}
	return t.Name, prefix, nil
}

// labNodes lists a running lab's nodes and their management addresses
// (from docker, through containerlab's labels).
func labNodes(lab string) (map[string]string, error) {
	ids, err := exec.Command("docker", "ps", "-q", "--filter", "label=containerlab="+lab).Output()
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if len(strings.Fields(string(ids))) == 0 {
		return map[string]string{}, nil
	}
	out, err := exec.Command("docker", append([]string{"inspect", "-f", `{{index .Config.Labels "clab-node-name"}} {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}`}, strings.Fields(string(ids))...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	nodes := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(l); len(f) >= 2 {
			nodes[f[0]] = f[1]
		}
	}
	return nodes, nil
}

// nodeAddr is the address of a lab's node: its management IP from docker,
// else its containerlab container name (prefix-lab-node).
func nodeAddr(arg, node string) string {
	lab, prefix, err := labOf(arg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
		return node
	}
	if nodes, err := labNodes(lab); err == nil {
		if ip, ok := nodes[node]; ok {
			return ip
		}
	}
	full := prefix + "-" + lab + "-"
	if prefix == "" || strings.HasPrefix(node, full) {
		return node // no prefix, or already the container's name
	}
	return full + node
}

func nodesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "nodes", Short: "List the lab's clabgen agents (with --lab)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if labArg == "" {
				return errors.New("nodes needs --lab NAME or --lab topology.clab.yml (or CLABGEN_LAB)")
			}
			lab, _, err := labOf(labArg)
			if err != nil {
				return err
			}
			nodes, err := labNodes(lab)
			if err != nil {
				return err
			}
			names := make([]string, 0, len(nodes))
			for n := range nodes {
				names = append(names, n)
			}
			sort.Strings(names)
			w := table()
			fmt.Fprintln(w, "NODE\tADDRESS\tVERSION\tUP SINCE")
			for _, n := range names {
				c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				r, err := clientFor(nodes[n]).GetInfo(c, connect.NewRequest(&v1.GetInfoRequest{}))
				cancel()
				if err != nil {
					continue // not a clabgen agent
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, nodes[n], r.Msg.Version, r.Msg.StartTime.AsTime().Local().Format(time.DateTime))
			}
			return w.Flush()
		}}
}

func ctx() (context.Context, context.CancelFunc) {
	c, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if timeout <= 0 {
		return c, stop
	}
	c2, cancel := context.WithTimeout(c, timeout)
	return c2, func() { cancel(); stop() }
}

// printMsg prints a proto message as YAML (default) or JSON.
func printMsg(m proto.Message) error {
	b, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(m)
	if err != nil {
		return err
	}
	return printJSON(b)
}

func printJSON(b []byte) error {
	if outFormat == "json" {
		var v any
		json.Unmarshal(b, &v)
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(out))
		return nil
	}
	y, err := yaml.JSONToYAML(b)
	if err != nil {
		return err
	}
	fmt.Print(string(y))
	return nil
}

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

// parseSets turns ["a.b=1", "c=[1,2]", "d=x"] into a nested map. Values that
// parse as JSON keep their type; anything else is a string.
func parseSets(sets []string) (map[string]any, error) {
	root := map[string]any{}
	for _, s := range sets {
		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("expected key=value, got %q", s)
		}
		var val any
		if err := json.Unmarshal([]byte(v), &val); err != nil {
			val = v
		}
		m := root
		parts := strings.Split(k, ".")
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = val
	}
	return root, nil
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func buildStruct(jsonArg string, sets []string) (*structpb.Struct, error) {
	m := map[string]any{}
	if jsonArg != "" {
		if strings.HasPrefix(jsonArg, "@") {
			b, err := os.ReadFile(jsonArg[1:])
			if err != nil {
				return nil, err
			}
			jsonArg = string(b)
		}
		jb, err := yaml.YAMLToJSON([]byte(jsonArg))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(jb, &m); err != nil {
			return nil, fmt.Errorf("config must be an object: %w", err)
		}
	}
	extra, err := parseSets(sets)
	if err != nil {
		return nil, err
	}
	merge(m, extra)
	return structpb.NewStruct(m)
}

// withPcap puts a capture file into a packet config (pcap.data, base64).
func withPcap(s *structpb.Struct, path string) (*structpb.Struct, error) {
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := s.AsMap()
	merge(m, map[string]any{"pcap": map[string]any{"data": base64.StdEncoding.EncodeToString(b)}})
	return structpb.NewStruct(m)
}

// printPreview shows a packet preview (the packet builder's): a summary,
// each layer's fields, and a hex dump. -o yaml or json prints it as is.
func printPreview(cmd *cobra.Command, r *structpb.Struct) error {
	if f := cmd.Flag("output"); f != nil && f.Changed {
		return printMsg(r)
	}
	m := r.AsMap()
	raw, _ := hex.DecodeString(fmt.Sprint(m["hex"]))
	if raw == nil {
		return printMsg(r)
	}
	fmt.Printf("%v (packet %v)\n", m["summary"], m["index"])
	if n, _ := m["note"].(string); n != "" {
		fmt.Println("note:", n)
	}
	w := table()
	for _, x := range asList(m["layers"]) {
		l := x.(map[string]any)
		var fs []string
		for _, y := range asList(l["fields"]) {
			f := y.(map[string]any)
			fs = append(fs, fmt.Sprintf("%v=%v", f["name"], f["value"]))
		}
		fmt.Fprintf(w, "%4.0f\t%v\t%s\n", l["offset"], l["title"], strings.Join(fs, " "))
	}
	w.Flush()
	fmt.Println()
	for off := 0; off < len(raw); off += 16 {
		end := min(off+16, len(raw))
		var hx, asc strings.Builder
		for i := off; i < off+16; i++ {
			if i < end {
				fmt.Fprintf(&hx, "%02x ", raw[i])
				if c := raw[i]; c >= 0x20 && c < 0x7f {
					asc.WriteByte(c)
				} else {
					asc.WriteByte('.')
				}
			} else {
				hx.WriteString("   ")
			}
			if i == off+7 {
				hx.WriteByte(' ')
			}
		}
		fmt.Printf("%04x  %s %s\n", off, hx.String(), asc.String())
	}
	if fr := asList(m["frames"]); len(fr) > 0 {
		fmt.Printf("\n%d frames in the capture:\n", len(fr))
		for i, x := range fr {
			if i == 20 {
				fmt.Printf("  … %d more\n", len(fr)-20)
				break
			}
			f := x.(map[string]any)
			fmt.Printf("  %4.0f  %v\n", f["index"].(float64)+1, f["summary"])
		}
	}
	return nil
}

func asList(v any) []any { l, _ := v.([]any); return l }

// vlanList shows a device's tags, outermost first: 100, 0x88a8:200, 10.5.
func vlanList(d *v1.Device) []string {
	var tags []string
	for _, t := range d.Vlans {
		s := strconv.Itoa(int(t.Vid))
		if t.Tpid != 0 && t.Tpid != 0x8100 {
			s = fmt.Sprintf("%#x:%s", t.Tpid, s)
		}
		if t.Pcp != 0 {
			s += fmt.Sprintf(".%d", t.Pcp)
		}
		tags = append(tags, s)
	}
	return tags
}

func parseVLAN(s string) (*v1.VlanTag, error) {
	// [tpid:]vid[.pcp], e.g. 100, 0x88a8:200, 100.5
	t := &v1.VlanTag{}
	if tp, rest, ok := strings.Cut(s, ":"); ok {
		v, err := strconv.ParseUint(tp, 0, 16)
		if err != nil {
			return nil, fmt.Errorf("bad TPID in %q", s)
		}
		t.Tpid, s = uint32(v), rest
	}
	if vid, pcp, ok := strings.Cut(s, "."); ok {
		p, err := strconv.ParseUint(pcp, 10, 3)
		if err != nil {
			return nil, fmt.Errorf("bad PCP in %q", s)
		}
		t.Pcp, s = uint32(p), vid
	}
	v, err := strconv.ParseUint(s, 10, 12)
	if err != nil {
		return nil, fmt.Errorf("bad VLAN ID %q", s)
	}
	t.Vid = uint32(v)
	return t, nil
}

func main() {
	root := &cobra.Command{
		Use:           "clabgenctl",
		Short:         "Control clabgen agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	def := os.Getenv("CLABGEN_SERVER")
	if def == "" {
		def = "localhost:8080"
	}
	root.PersistentFlags().StringVarP(&serverAddr, "server", "s", def, "agent address host[:port], or with --lab a node name (env CLABGEN_SERVER)")
	root.PersistentFlags().StringVar(&labArg, "lab", os.Getenv("CLABGEN_LAB"), "containerlab lab (name or topology file): -s takes its node names (env CLABGEN_LAB)")
	root.PersistentFlags().StringVarP(&outFormat, "output", "o", "yaml", "output format for detailed views: yaml or json")
	root.PersistentFlags().DurationVar(&timeout, "timeout", 0, "overall request timeout (0 = none)")

	root.AddCommand(&cobra.Command{
		Use: "info", Short: "Show agent info",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().GetInfo(c, connect.NewRequest(&v1.GetInfoRequest{}))
			if err != nil {
				return err
			}
			return printMsg(r.Msg)
		}})

	root.AddCommand(nodesCmd(), captureCmd(), portsCmd(), portCmd(), devicesCmd(), deviceCmd(), typesCmd(), protocolsCmd(), protoCmd(), previewCmd(), groupsCmd(), groupCmd(), eventsCmd(), scenarioCmd(),
		flowCmd("start"), flowCmd("stop"), pingCmd(), receivedCmd())

	if err := root.Execute(); err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			fmt.Fprintf(os.Stderr, "error: %s: %s\n", ce.Code(), ce.Message())
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
	}
}

func upDown(b bool) string {
	if b {
		return "up"
	}
	return "down"
}

func portsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ports", Short: "List data ports",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().ListPorts(c, connect.NewRequest(&v1.ListPortsRequest{}))
			if err != nil {
				return err
			}
			w := table()
			fmt.Fprintln(w, "PORT\tADMIN\tOPER\tMAC\tMTU\tRX\tTX\tTX-ERR\tUNMATCHED")
			for _, p := range r.Msg.Ports {
				k := p.Counters
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\n", p.Name, upDown(p.AdminUp), upDown(p.OperUp), p.Mac, p.Mtu,
					k.GetRxFrames(), k.GetTxFrames(), k.GetTxErrors(), k.GetRxUnmatched())
			}
			return w.Flush()
		}}
}

func portCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "port", Short: "Change a port"}
	for _, up := range []bool{true, false} {
		up := up
		cmd.AddCommand(&cobra.Command{
			Use: upDown(up) + " PORT...", Short: "Set port(s) admin " + upDown(up), Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, cancel := ctx()
				defer cancel()
				for _, p := range args {
					if _, err := client().SetPortAdmin(c, connect.NewRequest(&v1.SetPortAdminRequest{Name: p, Up: up})); err != nil {
						return err
					}
				}
				return nil
			}})
	}
	return cmd
}

// memberFilter keeps group members out of lists unless asked for: groups
// can have thousands. It returns whether to show an item of group g.
func memberFilter(all bool, only string) func(g string) bool {
	return func(g string) bool { return all || g == only }
}

func devicesCmd() *cobra.Command {
	var all bool
	var only string
	cmd := &cobra.Command{
		Use: "devices", Short: "List emulated devices (group members with --all or --group)",
		RunE: func(cmd *cobra.Command, args []string) error {
			show, hidden := memberFilter(all, only), 0
			c, cancel := ctx()
			defer cancel()
			r, err := client().ListDevices(c, connect.NewRequest(&v1.ListDevicesRequest{}))
			if err != nil {
				return err
			}
			w := table()
			fmt.Fprintln(w, "DEVICE\tPORT\tMAC\tVLANS\tADDRESSES")
			for _, d := range r.Msg.Devices {
				if !show(d.Group) {
					if d.Group != "" && only == "" {
						hidden++
					}
					continue
				}
				tags := vlanList(d)
				addrs := strings.Join(d.Addresses, ",")
				if gw := strings.Trim(d.Gateway4+","+d.Gateway6, ","); gw != "" {
					addrs += " gw " + gw
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Port, d.Mac, strings.Join(tags, ","), addrs)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if hidden > 0 {
				fmt.Printf("(%d group members not shown: --all, or --group NAME)\n", hidden)
			}
			return nil
		}}
	cmd.Flags().BoolVar(&all, "all", false, "include group members")
	cmd.Flags().StringVar(&only, "group", "", "only this group's members")
	return cmd
}

func deviceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "device", Short: "Create, change, inspect or delete devices"}
	var port, mac string
	var vlans, ips, gws []string
	add := &cobra.Command{
		Use: "add NAME", Short: "Create a device", Args: cobra.ExactArgs(1),
		Example: "  clabgenctl device add d1 --port eth1 --vlan 100 --ip 10.0.100.2/24 --gw 10.0.100.1\n" +
			"  clabgenctl device add qinq --port eth2 --vlan 0x88a8:200 --vlan 10 --ip 2001:db8::2/64",
		RunE: func(cmd *cobra.Command, args []string) error {
			d := &v1.Device{Name: args[0], Port: port, Mac: mac, Addresses: ips}
			if err := setGateways(d, gws); err != nil {
				return err
			}
			for _, v := range vlans {
				t, err := parseVLAN(v)
				if err != nil {
					return err
				}
				d.Vlans = append(d.Vlans, t)
			}
			c, cancel := ctx()
			defer cancel()
			r, err := client().CreateDevice(c, connect.NewRequest(&v1.CreateDeviceRequest{Device: d}))
			if err != nil {
				return err
			}
			return printMsg(r.Msg.Device)
		}}
	add.Flags().StringVar(&port, "port", "eth1", "port")
	add.Flags().StringVar(&mac, "mac", "", "MAC (default: derived from host and name)")
	add.Flags().StringArrayVar(&vlans, "vlan", nil, "VLAN tag [tpid:]vid[.pcp], outermost first; repeatable")
	add.Flags().StringArrayVar(&ips, "ip", nil, "address in CIDR form (IPv4 or IPv6); repeatable")
	add.Flags().StringArrayVar(&gws, "gw", nil, "default gateway (one IPv4 and/or one IPv6); repeatable")
	var setPort, setMAC string
	var setVLANs, setIPs, setGWs []string
	var untagged, noIP, noGW bool
	set := &cobra.Command{
		Use:   "set NAME",
		Short: "Change a device (port, MAC or VLAN changes restart its protocols; IP changes don't)",
		Args:  cobra.ExactArgs(1),
		Example: "  clabgenctl device set d1 --vlan 200\n  clabgenctl device set d1 --vlan 0x88a8:300 --vlan 20\n" +
			"  clabgenctl device set d1 --untagged --mac 02:00:00:00:00:01\n  clabgenctl device set d1 --mac auto\n" +
			"  clabgenctl device set d1 --ip 10.0.0.2/24 --ip 2001:db8::2/64 --gw 10.0.0.1\n  clabgenctl device set d1 --no-ip",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			cl := client()
			list, err := cl.ListDevices(c, connect.NewRequest(&v1.ListDevicesRequest{}))
			if err != nil {
				return err
			}
			var d *v1.Device
			for _, x := range list.Msg.Devices {
				if x.Name == args[0] {
					d = x
				}
			}
			if d == nil {
				return fmt.Errorf("no device %q", args[0])
			}
			f := cmd.Flags()
			if f.Changed("port") {
				d.Port = setPort
			}
			if f.Changed("mac") {
				d.Mac = setMAC
				if setMAC == "auto" {
					d.Mac = ""
				}
			}
			if untagged && len(setVLANs) > 0 {
				return errors.New("--untagged and --vlan are mutually exclusive")
			}
			if untagged {
				d.Vlans = nil
			} else if len(setVLANs) > 0 {
				d.Vlans = nil
				for _, v := range setVLANs {
					t, err := parseVLAN(v)
					if err != nil {
						return err
					}
					d.Vlans = append(d.Vlans, t)
				}
			}
			if noIP && len(setIPs) > 0 {
				return errors.New("--no-ip and --ip are mutually exclusive")
			}
			if noIP {
				d.Addresses, d.Gateway4, d.Gateway6 = nil, "", ""
			} else if len(setIPs) > 0 {
				d.Addresses = setIPs
			}
			if noGW {
				d.Gateway4, d.Gateway6 = "", ""
			}
			if err := setGateways(d, setGWs); err != nil {
				return err
			}
			r, err := cl.UpdateDevice(c, connect.NewRequest(&v1.UpdateDeviceRequest{Device: d}))
			if err != nil {
				return err
			}
			return printMsg(r.Msg.Device)
		}}
	set.Flags().StringVar(&setPort, "port", "", "new port")
	set.Flags().StringVar(&setMAC, "mac", "", "new MAC, or 'auto' for the derived one")
	set.Flags().StringArrayVar(&setVLANs, "vlan", nil, "new VLAN stack [tpid:]vid[.pcp], outermost first; repeatable")
	set.Flags().BoolVar(&untagged, "untagged", false, "remove all VLAN tags")
	set.Flags().StringArrayVar(&setIPs, "ip", nil, "new address list in CIDR form; repeatable")
	set.Flags().BoolVar(&noIP, "no-ip", false, "remove all addresses and gateways")
	set.Flags().StringArrayVar(&setGWs, "gw", nil, "set the IPv4 and/or IPv6 gateway; repeatable")
	set.Flags().BoolVar(&noGW, "no-gw", false, "remove the gateways")
	show := &cobra.Command{
		Use: "show NAME", Short: "Show a device: port, MAC, VLANs, addresses, neighbors and IP counters (-o json/yaml: all of it as data)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			cl := client()
			ds, err := cl.ListDevices(c, connect.NewRequest(&v1.ListDevicesRequest{}))
			if err != nil {
				return err
			}
			var d *v1.Device
			for _, x := range ds.Msg.Devices {
				if x.Name == args[0] {
					d = x
				}
			}
			if d == nil {
				return fmt.Errorf("device %s: not found", args[0])
			}
			r, err := cl.GetDeviceIP(c, connect.NewRequest(&v1.GetDeviceIPRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			m := r.Msg
			if f := cmd.Flag("output"); f != nil && f.Changed {
				mo := protojson.MarshalOptions{UseProtoNames: true}
				db, _ := mo.Marshal(d)
				ib, _ := mo.Marshal(m)
				b, _ := json.Marshal(map[string]json.RawMessage{"device": db, "ip": ib})
				return printJSON(b)
			}
			mac := d.Mac
			if d.MacDerived {
				mac += " (derived)"
			}
			fmt.Printf("port:      %s\n", d.Port)
			fmt.Printf("mac:       %s\n", mac)
			fmt.Printf("vlans:     %s\n", orNone(strings.Join(vlanList(d), ", ")))
			if d.Group != "" {
				fmt.Printf("group:     %s\n", d.Group)
			}
			fmt.Printf("addresses: %s\n", orNone(strings.Join(m.Addresses, ", ")))
			fmt.Printf("gateways:  %s\n\n", orNone(strings.Trim(m.Gateway4+", "+m.Gateway6, ", ")))
			w := table()
			fmt.Fprintln(w, "NEIGHBOR\tMAC\tSTATE\tAGE")
			for _, n := range m.Neighbors {
				age := "-"
				if n.Mac != "" {
					age = (time.Duration(n.AgeMs) * time.Millisecond).Round(time.Second).String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.Ip, orNone(n.Mac), n.State, age)
			}
			w.Flush()
			fmt.Println()
			keys := make([]string, 0, len(m.Counters))
			for k, v := range m.Counters {
				if v != 0 {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			w = table()
			for _, k := range keys {
				fmt.Fprintf(w, "%s\t%d\n", k, m.Counters[k])
			}
			return w.Flush()
		}}
	resolve := &cobra.Command{
		Use: "resolve NAME IP", Aliases: []string{"arp"}, Short: "Resolve an address with ARP or ND (off-link: the gateway)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().ResolveNeighbor(c, connect.NewRequest(&v1.ResolveNeighborRequest{Device: args[0], Ip: args[1]}))
			if err != nil {
				return err
			}
			m := r.Msg
			via := ""
			if m.NextHop != args[1] {
				via = " via " + m.NextHop
			}
			how := fmt.Sprintf("%.2f ms", float64(m.RttUs)/1000)
			if m.Cached {
				how = "cached"
			}
			fmt.Printf("%s%s is at %s (%s)\n", args[1], via, m.Mac, how)
			return nil
		}}
	clearNeigh := &cobra.Command{
		Use: "clear-neighbors NAME", Short: "Empty a device's neighbor cache", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			_, err := client().ClearNeighbors(c, connect.NewRequest(&v1.ClearNeighborsRequest{Device: args[0]}))
			return err
		}}
	del := &cobra.Command{
		Use: "del NAME...", Aliases: []string{"delete", "rm"}, Short: "Delete devices", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			for _, n := range args {
				if _, err := client().DeleteDevice(c, connect.NewRequest(&v1.DeleteDeviceRequest{Name: n})); err != nil {
					return err
				}
			}
			return nil
		}}
	cmd.AddCommand(add, set, show, resolve, clearNeigh, del)
	return cmd
}

func typesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "types [TYPE]", Short: "List protocol types, or show one type's config and action schemas",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().ListProtocolTypes(c, connect.NewRequest(&v1.ListProtocolTypesRequest{}))
			if err != nil {
				return err
			}
			if len(args) == 0 {
				w := table()
				fmt.Fprintln(w, "TYPE\tACTIONS\tDESCRIPTION")
				for _, t := range r.Msg.Types {
					var as []string
					for _, a := range t.Actions {
						as = append(as, a.Name)
					}
					fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, strings.Join(as, ","), t.Description)
				}
				return w.Flush()
			}
			for _, t := range r.Msg.Types {
				if t.Name != args[0] {
					continue
				}
				out := map[string]any{"name": t.Name, "description": t.Description}
				var cfg any
				json.Unmarshal([]byte(t.ConfigSchema), &cfg)
				out["config"] = cfg
				acts := map[string]any{}
				for _, a := range t.Actions {
					var s any
					json.Unmarshal([]byte(a.ArgsSchema), &s)
					acts[a.Name] = map[string]any{"description": a.Description, "args": s}
				}
				out["actions"] = acts
				b, _ := json.Marshal(out)
				return printJSON(b)
			}
			return fmt.Errorf("no protocol type %q", args[0])
		}}
}

// summary picks a one-line status for list views.
func summary(p *v1.Protocol) string {
	if p.Status == nil {
		return p.Error
	}
	m := p.Status.AsMap()
	if p.Type == "cfm-mep" {
		var rs []string
		if l, ok := m["remote_meps"].([]any); ok {
			for _, x := range l {
				r := x.(map[string]any)
				rs = append(rs, fmt.Sprintf("%v:%v", r["mep_id"], r["state"]))
			}
		}
		return fmt.Sprintf("lvl=%v mep=%v %v rmeps=[%s] defect=%v", m["level"], m["mep_id"], m["maid"], strings.Join(rs, " "), m["highest_defect"])
	}
	if p.Type == "bgp" {
		var ps []string
		if l, ok := m["peers"].([]any); ok {
			for _, x := range l {
				n := x.(map[string]any)
				sum := func(k string) int {
					t := 0
					if mm, ok := n[k].(map[string]any); ok {
						for _, v := range mm {
							f, _ := v.(float64)
							t += int(f)
						}
					}
					return t
				}
				ps = append(ps, fmt.Sprintf("%v:%v(rx %d tx %d)", n["address"], n["state"], sum("received"), sum("advertised")))
			}
		}
		return fmt.Sprintf("AS %v id %v peers=[%s]", m["local_as"], m["router_id"], strings.Join(ps, " "))
	}
	if p.Type == "ospf" || p.Type == "ospfv3" {
		var ns []string
		if l, ok := m["neighbors"].([]any); ok {
			for _, x := range l {
				n := x.(map[string]any)
				who := fmt.Sprint(n["router_id"])
				if r, _ := n["role"].(string); r != "" {
					who += "/" + strings.ToUpper(r)
				}
				st := fmt.Sprint(n["state"])
				if b, ok := n["bfd"].(map[string]any); ok {
					st += fmt.Sprintf("/bfd-%v", b["state"])
				}
				ns = append(ns, fmt.Sprintf("%s:%s", who, st))
			}
		}
		areas, _ := m["areas"].([]any)
		var ifs []string
		if l, ok := m["interfaces"].([]any); ok {
			for _, x := range l {
				i := x.(map[string]any)
				s := fmt.Sprintf("%v:%v", i["name"], i["state"])
				if len(areas) > 1 {
					s += fmt.Sprintf("@%v", i["area"])
				}
				ifs = append(ifs, s)
			}
		}
		out := fmt.Sprintf("%v area %v if=[%s] nbrs=[%s] lsdb=%v", m["router_id"], m["area"], strings.Join(ifs, " "), strings.Join(ns, " "), m["lsdb_total"])
		if m["abr"] == true {
			out += fmt.Sprintf(" abr(%d areas)", len(areas))
		}
		if sr, ok := m["segment_routing"].(map[string]any); ok && sr["srgb"] != nil {
			out += fmt.Sprintf(" sr=%v", sr["srgb"])
		}
		if g, ok := m["graceful_restart"].(map[string]any); ok && g["restart"] != nil {
			out += fmt.Sprintf(" gr=%v", g["restart"])
		}
		if m["max_metric"] == true {
			out += " max-metric"
		}
		return out
	}
	if p.Type == "lldp" {
		var ns []string
		if l, ok := m["neighbors"].([]any); ok {
			for _, x := range l {
				n := x.(map[string]any)
				who := n["system_name"]
				if who == nil || who == "" {
					who = n["chassis_id"]
				}
				ns = append(ns, fmt.Sprintf("%v:%v", who, n["port_id"]))
			}
		}
		return fmt.Sprintf("%v tx=%v neighbors=[%s]", m["mode"], m["transmit"], strings.Join(ns, " "))
	}
	if p.Type == "isis" {
		adj, _ := m["adjacency"].(map[string]any)
		nbr := fmt.Sprint(adj["neighbor"])
		if h, _ := adj["hostname"].(string); h != "" {
			nbr = h
		}
		out := fmt.Sprintf("%v %v adj=%v", m["system_id"], m["level"], adj["state"])
		if ifs, _ := m["interfaces"].([]any); len(ifs) > 1 {
			var per []string
			for _, x := range ifs {
				i := x.(map[string]any)
				st := fmt.Sprint(i["adjacencies_up"])
				if i["attached"] != true {
					st = "detached"
				} else if i["passive"] == true {
					st = "passive"
				}
				per = append(per, fmt.Sprintf("%v:%s", i["name"], st))
			}
			out = fmt.Sprintf("%v %v interfaces=[%s]", m["system_id"], m["level"], strings.Join(per, " "))
		} else if lan, ok := m["lan"].([]any); ok {
			up := 0
			if as, ok := m["adjacencies"].([]any); ok {
				for _, a := range as {
					if a.(map[string]any)["state"] == "up" {
						up++
					}
				}
			}
			var dis []string
			for _, l := range lan {
				x := l.(map[string]any)
				if x["we_are_dis"] == true {
					dis = append(dis, "us")
				} else {
					dis = append(dis, fmt.Sprint(x["dis"]))
				}
			}
			out = fmt.Sprintf("%v %v lan neighbors=%d dis=%s", m["system_id"], m["level"], up, strings.Join(dis, "/"))
		} else if adj["state"] != "down" {
			out += " nbr=" + nbr
		}
		out += fmt.Sprintf(" lsdb=%v prefixes=%v", m["lsdb_total"], m["advertised_prefixes"])
		if m["overload"] == true {
			out += " overload"
		}
		return out
	}
	if p.Type == "l2-echo" {
		return fmt.Sprintf("received=%v sent=%v", m["received"], m["sent"])
	}
	if p.Type == "icmp" || p.Type == "udp" || p.Type == "l2" || p.Type == "packet" {
		out := fmt.Sprintf("%v → %v %v@%vpps tx=%v rx=%v lost=%v", m["state"], m["destination"], m["kind"], m["rate"], m["tx"], m["rx"], m["lost"])
		if m["tracks_replies"] == false {
			// One way: receivers report rx/loss (clabgenctl received).
			out = fmt.Sprintf("%v → %v %v@%vpps tx=%v", m["state"], m["destination"], m["kind"], m["rate"], m["tx"])
			if e, _ := m["tx_errors"].(float64); e > 0 {
				out += fmt.Sprintf(" tx_errors=%v", e)
			}
		}
		if o, _ := m["outage_max_ms"].(float64); o > 0 {
			out += fmt.Sprintf(" outage_max=%gms", o)
		}
		if r, ok := m["rtt"].(map[string]any); ok {
			out += fmt.Sprintf(" rtt=%.2fms", r["avg_us"].(float64)/1000)
		}
		if v, ok := m["verdict"].(map[string]any); ok {
			out += " checks=" + fmt.Sprint(v["result"])
			if f, _ := v["failed"].(string); f != "" {
				out += " (" + f + ")"
			}
		}
		if e, _ := m["error"].(string); e != "" {
			out += " (" + e + ")"
		}
		return out
	}
	return ""
}

func protocolsCmd() *cobra.Command {
	var all bool
	var only string
	cmd := &cobra.Command{
		Use: "protocols", Aliases: []string{"protos"}, Short: "List protocol instances (group members with --all or --group)",
		RunE: func(cmd *cobra.Command, args []string) error {
			show, hidden := memberFilter(all, only), 0
			c, cancel := ctx()
			defer cancel()
			r, err := client().ListProtocols(c, connect.NewRequest(&v1.ListProtocolsRequest{WithStatus: true, Brief: true, NoGroupMembers: !all && only == ""}))
			if err != nil {
				return err
			}
			w := table()
			fmt.Fprintln(w, "NAME\tTYPE\tDEVICE\tSTATE\tSUMMARY")
			for _, p := range r.Msg.Protocols {
				if !show(p.Group) {
					if p.Group != "" && only == "" {
						hidden++
					}
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Type, p.Device, p.State, summary(p))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if !all && only == "" {
				if g, err := client().ListDeviceGroups(c, connect.NewRequest(&v1.ListDeviceGroupsRequest{})); err == nil && len(g.Msg.Groups) > 0 {
					fmt.Printf("(group members not shown: --all, or --group NAME; `groups` summarises them)\n")
				}
			}
			return nil
		}}
	cmd.Flags().BoolVar(&all, "all", false, "include group members")
	cmd.Flags().StringVar(&only, "group", "", "only this group's members")
	return cmd
}

func protoCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "proto", Short: "Create, inspect, change and drive protocol instances"}

	var typ, dev, cfgJSON, pcapFile string
	add := &cobra.Command{
		Use: "add NAME [key=value...]", Short: "Create a protocol instance", Args: cobra.MinimumNArgs(1),
		Example: "  clabgenctl proto add mep1 --type cfm-mep --device d1 level=3 mep_id=1 ma_format=icc ma_name=CLABGEN00001 remote_meps=[2]\n" +
			"  clabgenctl proto add replay --type packet --device d1 --pcap capture.pcapng rate=100 auto_start=true",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := buildStruct(cfgJSON, args[1:])
			if err == nil {
				cfg, err = withPcap(cfg, pcapFile)
			}
			if err != nil {
				return err
			}
			c, cancel := ctx()
			defer cancel()
			r, err := client().CreateProtocol(c, connect.NewRequest(&v1.CreateProtocolRequest{
				Protocol: &v1.Protocol{Name: args[0], Type: typ, Device: dev, Config: cfg}}))
			if err != nil {
				return err
			}
			return printMsg(r.Msg.Protocol)
		}}
	add.Flags().StringVar(&typ, "type", "cfm-mep", "protocol type")
	add.Flags().StringVar(&dev, "device", "", "device name")
	add.Flags().StringVar(&cfgJSON, "config", "", "config as JSON/YAML, or @file")
	add.Flags().StringVar(&pcapFile, "pcap", "", "packet type: replay this capture (pcap or pcapng)")
	add.MarkFlagRequired("device")

	show := &cobra.Command{
		Use: "show NAME", Short: "Show config and live status", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().GetProtocol(c, connect.NewRequest(&v1.GetProtocolRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			return printMsg(r.Msg.Protocol)
		}}

	var setJSON, setPcap string
	set := &cobra.Command{
		Use: "set NAME key=value...", Short: "Change config (merged onto current config; restarts the instance)", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			cl := client()
			cur, err := cl.GetProtocol(c, connect.NewRequest(&v1.GetProtocolRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			upd, err := buildStruct(setJSON, args[1:])
			if err == nil {
				upd, err = withPcap(upd, setPcap)
			}
			if err != nil {
				return err
			}
			m := cur.Msg.Protocol.Config.AsMap()
			merge(m, upd.AsMap())
			cfg, err := structpb.NewStruct(m)
			if err != nil {
				return err
			}
			r, err := cl.UpdateProtocol(c, connect.NewRequest(&v1.UpdateProtocolRequest{Name: args[0], Config: cfg}))
			if err != nil {
				return err
			}
			return printMsg(r.Msg.Protocol.Config)
		}}
	set.Flags().StringVar(&setJSON, "config", "", "config fragment as JSON/YAML, or @file")
	set.Flags().StringVar(&setPcap, "pcap", "", "packet type: replay this capture instead")

	del := &cobra.Command{
		Use: "del NAME...", Aliases: []string{"delete", "rm"}, Short: "Delete protocol instances", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			for _, n := range args {
				if _, err := client().DeleteProtocol(c, connect.NewRequest(&v1.DeleteProtocolRequest{Name: n})); err != nil {
					return err
				}
			}
			return nil
		}}

	toggle := func(use, short string, disabled bool) *cobra.Command {
		return &cobra.Command{
			Use: use + " NAME...", Short: short, Args: cobra.MinimumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, cancel := ctx()
				defer cancel()
				for _, n := range args {
					r, err := client().SetProtocolDisabled(c, connect.NewRequest(&v1.SetProtocolDisabledRequest{Name: n, Disabled: disabled}))
					if err != nil {
						return err
					}
					fmt.Printf("%s: %s\n", n, r.Msg.Protocol.State)
				}
				return nil
			}}
	}
	disable := toggle("disable", "Stop protocol instances, keeping them and their config", true)
	enable := toggle("enable", "Start disabled protocol instances again", false)

	var argsJSON string
	run := &cobra.Command{
		Use: "run NAME ACTION [key=value...]", Short: "Run an action (see `types TYPE` for actions and arguments)", Args: cobra.MinimumNArgs(2),
		Example: "  clabgenctl proto run mep1 loopback target_mep_id=2 count=5\n  clabgenctl proto run mep1 fault ccm_enabled=false",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := buildStruct(argsJSON, args[2:])
			if err != nil {
				return err
			}
			c, cancel := ctx()
			defer cancel()
			r, err := client().RunProtocolAction(c, connect.NewRequest(&v1.RunProtocolActionRequest{Name: args[0], Action: args[1], Args: a}))
			if err != nil {
				return err
			}
			if args[1] == "preview" {
				return printPreview(cmd, r.Msg.Result)
			}
			return printMsg(r.Msg.Result)
		}}
	run.Flags().StringVar(&argsJSON, "args", "", "arguments as JSON/YAML, or @file")

	cmd.AddCommand(add, show, set, disable, enable, del, run)
	return cmd
}

func captureCmd() *cobra.Command {
	var dev, dir, out string
	var ets []string
	var snaplen, count uint32
	var seconds float64
	cmd := &cobra.Command{
		Use: "capture [PORT...]", Short: "Capture the frames ports receive and send (live, or to a pcapng file)",
		Example: "  clabgenctl capture eth1                       # one line per frame, like tcpdump\n" +
			"  clabgenctl capture --device v100 -c 20\n" +
			"  clabgenctl capture eth1 -w trace.pcapng --seconds 30\n" +
			"  clabgenctl capture eth1 -w - | wireshark -k -i -   # live in Wireshark",
		RunE: func(cmd *cobra.Command, args []string) error {
			req := &v1.CaptureRequest{Ports: args, Device: dev, Direction: dir, Snaplen: snaplen, Count: count, Seconds: seconds, Summaries: out == ""}
			for _, e := range ets {
				for _, x := range strings.Split(e, ",") {
					v, err := strconv.ParseUint(x, 0, 16)
					if err != nil {
						return fmt.Errorf("ethertype %q: a number like 0x0800", x)
					}
					req.Ethertypes = append(req.Ethertypes, uint32(v))
				}
			}
			c, cancel := ctx()
			defer cancel()
			cl := client()
			var pw *pcapng.Writer
			var names []string
			if out != "" {
				ports, err := cl.ListPorts(c, connect.NewRequest(&v1.ListPortsRequest{}))
				if err != nil {
					return err
				}
				var ifs []pcapng.Interface
				for _, p := range ports.Msg.Ports {
					if len(args) == 0 || slices.Contains(args, p.Name) {
						names = append(names, p.Name)
						ifs = append(ifs, pcapng.Interface{Name: p.Name, Snaplen: int(snaplen)})
					}
				}
				w := os.Stdout
				if out != "-" {
					f, err := os.Create(out)
					if err != nil {
						return err
					}
					defer f.Close()
					w = f
				}
				if pw, err = pcapng.NewWriter(w, ifs, "clabgenctl"); err != nil {
					return err
				}
			}
			st, err := cl.Capture(c, connect.NewRequest(req))
			if err != nil {
				return err
			}
			defer st.Close()
			n, dropped := 0, uint64(0)
			for st.Receive() {
				f := st.Msg()
				n++
				dropped = f.Dropped
				if pw == nil {
					arrow := "In "
					if f.Direction == "tx" {
						arrow = "Out"
					}
					fmt.Printf("%s %s %s %s\n", f.Time.AsTime().Local().Format("15:04:05.000000"), f.Port, arrow, f.Summary)
					continue
				}
				d := pcapng.Inbound
				if f.Direction == "tx" {
					d = pcapng.Outbound
				}
				if err := pw.WritePacket(max(slices.Index(names, f.Port), 0), f.Time.AsTime(), f.Data, int(f.Length), d); err != nil {
					return err
				}
			}
			if err := st.Err(); err != nil && c.Err() == nil {
				if connect.CodeOf(err) == connect.CodeUnimplemented {
					return errors.New("this agent has no packet capture: redeploy its node with a newer clabgen image")
				}
				return err
			}
			msg := fmt.Sprintf("%d frames captured", n)
			if dropped > 0 {
				msg += fmt.Sprintf(", %d dropped (the reader fell behind; try a filter or --snaplen)", dropped)
			}
			fmt.Fprintln(os.Stderr, msg)
			return nil
		}}
	cmd.Flags().StringVar(&dev, "device", "", "only frames to or from this device (its MAC or multicast, on its VLANs)")
	cmd.Flags().StringVar(&dir, "direction", "", "rx or tx (default both)")
	cmd.Flags().StringArrayVar(&ets, "ethertype", nil, "only these ethertypes, e.g. 0x0800,0x86dd; repeatable")
	cmd.Flags().Uint32Var(&snaplen, "snaplen", 0, "bytes kept per frame (default all)")
	cmd.Flags().Uint32VarP(&count, "count", "c", 0, "stop after this many frames")
	cmd.Flags().Float64Var(&seconds, "seconds", 0, "stop after this many seconds")
	cmd.Flags().StringVarP(&out, "write", "w", "", "write pcapng to this file (- for stdout) instead of printing")
	return cmd
}

func previewCmd() *cobra.Command {
	var typ, dev, cfgJSON, pcapFile string
	var index uint64
	cmd := &cobra.Command{
		Use: "preview [key=value...]", Short: "Show the packet a config would send, without creating anything (packet type)",
		Example: "  clabgenctl preview --device d1 --config '{layers: [{ipv4: {dst: 10.0.0.2}}, {udp: {dst_port: 53}}]}'\n" +
			"  clabgenctl preview --device d1 --config @packet.yml --index 5\n" +
			"  clabgenctl preview --device d1 --pcap capture.pcapng --index 2",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := buildStruct(cfgJSON, args)
			if err == nil {
				cfg, err = withPcap(cfg, pcapFile)
			}
			if err != nil {
				return err
			}
			a, _ := structpb.NewStruct(map[string]any{"index": float64(index)})
			c, cancel := ctx()
			defer cancel()
			r, err := client().PreviewProtocol(c, connect.NewRequest(&v1.PreviewProtocolRequest{Type: typ, Device: dev, Config: cfg, Args: a}))
			if err != nil {
				return err
			}
			return printPreview(cmd, r.Msg.Result)
		}}
	cmd.Flags().StringVar(&typ, "type", "packet", "protocol type")
	cmd.Flags().StringVar(&dev, "device", "", "device to send from")
	cmd.Flags().StringVar(&cfgJSON, "config", "", "config as JSON/YAML, or @file")
	cmd.Flags().StringVar(&pcapFile, "pcap", "", "a capture (pcap or pcapng) to replay")
	cmd.Flags().Uint64Var(&index, "index", 0, "which packet (0-based: modifiers change packets) or captured frame")
	cmd.MarkFlagRequired("device")
	return cmd
}

func eventsCmd() *cobra.Command {
	var replay bool
	cmd := &cobra.Command{
		Use: "events", Short: "Stream agent events",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			st, err := client().WatchEvents(c, connect.NewRequest(&v1.WatchEventsRequest{Replay: replay}))
			if err != nil {
				return err
			}
			for st.Receive() {
				e := st.Msg()
				if outFormat == "json" {
					b, _ := protojson.Marshal(e)
					fmt.Println(string(b))
					continue
				}
				extra := ""
				if e.Data != nil {
					m := e.Data.AsMap()
					keys := make([]string, 0, len(m))
					for k := range m {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					var kv []string
					for _, k := range keys {
						if s, ok := m[k].(string); ok && s == "" {
							continue
						}
						kv = append(kv, fmt.Sprintf("%s=%v", k, m[k]))
					}
					extra = "  " + strings.Join(kv, " ")
				}
				fmt.Printf("%s %-16s %-16s %s%s\n", e.Time.AsTime().Local().Format("15:04:05.000"), e.Source, e.Kind, e.Message, extra)
			}
			if err := st.Err(); err != nil && !errors.Is(err, context.Canceled) && c.Err() == nil {
				return err
			}
			return nil
		}}
	cmd.Flags().BoolVar(&replay, "replay", false, "print recent buffered events first")
	return cmd
}

func scenarioCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "scenario", Short: "Export or apply the agent's whole config"}
	cmd.AddCommand(&cobra.Command{
		Use: "get", Short: "Print the current scenario YAML",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().GetScenario(c, connect.NewRequest(&v1.GetScenarioRequest{}))
			if err != nil {
				return err
			}
			fmt.Print(r.Msg.Yaml)
			return nil
		}})
	var replace bool
	apply := &cobra.Command{
		Use: "apply FILE", Short: "Apply a scenario file ('-' for stdin)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var b []byte
			var err error
			if args[0] == "-" {
				b, err = readAll(os.Stdin)
			} else {
				b, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}
			c, cancel := ctx()
			defer cancel()
			_, err = client().ApplyScenario(c, connect.NewRequest(&v1.ApplyScenarioRequest{Yaml: string(b), Replace: replace}))
			return err
		}}
	apply.Flags().BoolVar(&replace, "replace", false, "delete existing devices and protocols first")
	cmd.AddCommand(apply)
	return cmd
}

func readAll(f *os.File) ([]byte, error) { return io.ReadAll(f) }

// setGateways assigns each --gw value to gateway4 or gateway6 by family.
func setGateways(d *v1.Device, gws []string) error {
	for _, g := range gws {
		a, err := netip.ParseAddr(g)
		if err != nil {
			return fmt.Errorf("--gw %q: %w", g, err)
		}
		if a.Is4() {
			d.Gateway4 = g
		} else {
			d.Gateway6 = g
		}
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// flowCmd runs the start or stop action on traffic flows.
func flowCmd(action string) *cobra.Command {
	return &cobra.Command{
		Use: action + " FLOW...", Short: strings.ToUpper(action[:1]) + action[1:] + " traffic flows", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			for _, n := range args {
				if _, err := client().RunProtocolAction(c, connect.NewRequest(&v1.RunProtocolActionRequest{Name: n, Action: action})); err != nil {
					return fmt.Errorf("%s: %w", n, err)
				}
			}
			return nil
		}}
}

// pingCmd runs a temporary ICMP flow and prints it like ping(8).
func pingCmd() *cobra.Command {
	var count uint64
	var interval time.Duration
	var size int
	var ttl uint8
	var keep bool
	cmd := &cobra.Command{
		Use:   "ping DEVICE DESTINATION",
		Short: "Ping from a device (a temporary icmp flow; Ctrl-C to stop)",
		Example: "  clabgenctl ping v400 10.0.4.1 -c 5\n  clabgenctl ping v400 2001:db8:4::1 -i 10ms\n" +
			"  clabgenctl ping v400 192.0.2.9 --keep    # leave the flow (ping-v400) running",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			cl := client()
			name := "ping-" + args[0]
			if !keep {
				name += fmt.Sprintf("-%d", time.Now().UnixNano()%100000)
			}
			mode := "continuous"
			if count > 0 {
				mode = "count"
			}
			cfg, err := structpb.NewStruct(map[string]any{"destination": args[1], "mode": mode, "count": float64(count),
				"rate": float64(time.Second) / float64(interval), "payload_size": float64(size), "ttl": float64(ttl), "auto_start": true})
			if err != nil {
				return err
			}
			if _, err := cl.CreateProtocol(c, connect.NewRequest(&v1.CreateProtocolRequest{
				Protocol: &v1.Protocol{Name: name, Type: "icmp", Device: args[0], Config: cfg}})); err != nil {
				return err
			}
			bg := context.Background()
			cleanup := func() {
				if !keep {
					cl.DeleteProtocol(bg, connect.NewRequest(&v1.DeleteProtocolRequest{Name: name}))
				}
			}
			defer cleanup()
			fmt.Printf("PING %s from %s: %d data bytes\n", args[1], args[0], size)
			printed := map[string]bool{}
			status := func() (map[string]any, error) {
				r, err := cl.GetProtocol(bg, connect.NewRequest(&v1.GetProtocolRequest{Name: name}))
				if err != nil {
					return nil, err
				}
				return r.Msg.Protocol.Status.AsMap(), nil
			}
			printNew := func(m map[string]any) {
				// recent is a sliding window: remember only what is still in it.
				lines, _ := m["recent"].([]any)
				now := map[string]bool{}
				for _, x := range lines {
					l := x.(map[string]any)
					seq := uint64(l["seq"].(float64))
					key := fmt.Sprint(seq, l["result"], l["time"])
					now[key] = true
					if printed[key] {
						continue
					}
					switch l["result"] {
					case "reply":
						fmt.Printf("%v: seq=%d time=%.3f ms\n", l["detail"], seq, l["rtt_us"].(float64)/1000)
					case "timeout":
						fmt.Printf("seq=%d timeout\n", seq)
					default:
						fmt.Printf("seq=%d %v %v\n", seq, l["result"], l["detail"])
					}
				}
				printed = now
			}
			poll := interval
			if poll < 200*time.Millisecond {
				poll = 200 * time.Millisecond
			}
			if poll > time.Second {
				poll = time.Second
			}
			tk := time.NewTicker(poll)
			defer tk.Stop()
			var m map[string]any
		loop:
			for {
				select {
				case <-c.Done():
					if !keep {
						cl.RunProtocolAction(bg, connect.NewRequest(&v1.RunProtocolActionRequest{Name: name, Action: "stop"}))
					}
					break loop
				case <-tk.C:
					if m, err = status(); err != nil {
						return err
					}
					printNew(m)
					if e, _ := m["error"].(string); e != "" && m["state"] == "resolving" {
						fmt.Fprintln(os.Stderr, "waiting:", e)
					}
					if m["state"] == "done" {
						break loop
					}
				}
			}
			if m, err = status(); err != nil {
				return err
			}
			printNew(m)
			fmt.Printf("--- %s ping statistics ---\n", args[1])
			fmt.Printf("%v transmitted, %v received, %v lost (%.1f%%)", m["tx"], m["rx"], m["lost"], m["loss_pct"])
			if o, _ := m["outage_max_ms"].(float64); o > 0 {
				fmt.Printf(", %v outages (max %g ms)", m["outages"], o)
			}
			fmt.Println()
			if r, ok := m["rtt"].(map[string]any); ok {
				fmt.Printf("rtt min/avg/max/jitter = %.3f/%.3f/%.3f/%.3f ms\n", r["min_us"].(float64)/1000, r["avg_us"].(float64)/1000,
					r["max_us"].(float64)/1000, r["jitter_us"].(float64)/1000)
			}
			if keep {
				fmt.Printf("flow %s kept\n", name)
			}
			return nil
		}}
	cmd.Flags().Uint64VarP(&count, "count", "c", 0, "stop after this many packets (default: until Ctrl-C)")
	cmd.Flags().DurationVarP(&interval, "interval", "i", time.Second, "time between packets")
	cmd.Flags().IntVar(&size, "size", 56, "data bytes (at least 24, for the signature)")
	cmd.Flags().Uint8VarP(&ttl, "ttl", "t", 64, "TTL / hop limit")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the flow (named ping-DEVICE) after exiting, still running")
	return cmd
}

// receivedCmd lists the signed flows devices have received.
func receivedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "received [DEVICE]", Short: "Flows received by devices (one-way loss, reordering, latency, outages)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			dev := ""
			if len(args) == 1 {
				dev = args[0]
			}
			r, err := client().ListReceivedFlows(c, connect.NewRequest(&v1.ListReceivedFlowsRequest{Device: dev}))
			if err != nil {
				return err
			}
			w := table()
			fmt.Fprintln(w, "DEVICE\tFLOW-ID\tPROTO\tFROM\tTO\tRX\tPPS\tLOST\tREORDER\tDUP\tLATENCY(avg/max)\tOUTAGES(max)\tIDLE")
			for _, f := range r.Msg.Flows {
				m := f.Stats.AsMap()
				g := func(k string) float64 { v, _ := m[k].(float64); return v }
				from, to := fmt.Sprint(m["src"]), fmt.Sprint(m["dst"])
				if m["proto"] == "udp" {
					hp := func(h any, p float64) string { return net.JoinHostPort(fmt.Sprint(h), strconv.Itoa(int(p))) }
					from, to = hp(m["src"], g("src_port")), hp(m["dst"], g("dst_port"))
				}
				if m["proto"] == "l2" {
					if n := g("src_macs"); n > 1 {
						from += fmt.Sprintf(" ×%.0f", n)
					}
					to += fmt.Sprintf(" %v", m["ethertype"])
				}
				fmt.Fprintf(w, "%s\t%v\t%v\t%s\t%s\t%.0f\t%.0f\t%.0f (%.2f%%)\t%.0f\t%.0f\t%.3f/%.3f ms\t%.0f (%.0f ms)\t%s\n",
					f.Device, m["flow_id"], m["proto"], from, to, g("rx"), g("rate_pps"), g("lost"), g("loss_pct"), g("reordered"), g("duplicate"),
					g("latency_avg_us")/1000, g("latency_max_us")/1000, g("outages"), g("outage_max_ms"),
					(time.Duration(g("idle_ms")) * time.Millisecond).Round(100*time.Millisecond))
			}
			return w.Flush()
		}}
	var flowID string
	clr := &cobra.Command{
		Use: "clear [DEVICE]", Short: "Forget received flows", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			dev := ""
			if len(args) == 1 {
				dev = args[0]
			}
			_, err := client().ClearReceivedFlows(c, connect.NewRequest(&v1.ClearReceivedFlowsRequest{Device: dev, FlowId: flowID}))
			return err
		}}
	clr.Flags().StringVar(&flowID, "flow", "", "only this flow ID (8 hex digits)")
	cmd.AddCommand(clr)
	return cmd
}
