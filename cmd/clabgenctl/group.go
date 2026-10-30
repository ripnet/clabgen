package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
)

// readSpec reads a YAML/JSON object from a file ('-' for stdin).
func readSpec(path string) (*structpb.Struct, error) {
	var b []byte
	var err error
	if path == "-" {
		b, err = readAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return structpb.NewStruct(m)
}

// counts renders {"established": 3, "active": 1} as "3 established, 1 active".
func counts(m map[string]any) string {
	type kv struct {
		k string
		n int
	}
	var l []kv
	for k, v := range m {
		f, _ := v.(float64)
		l = append(l, kv{k, int(f)})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n || l[i].n == l[j].n && l[i].k < l[j].k })
	var out []string
	for _, x := range l {
		out = append(out, fmt.Sprintf("%d %s", x.n, x.k))
	}
	return strings.Join(out, ", ")
}

// groupProtocols summarises each group protocol: health where the
// protocol reports it, else instance states.
func groupProtocols(g *v1.DeviceGroup) []string {
	var out []string
	ps, _ := g.Status.AsMap()["protocols"].([]any)
	for _, x := range ps {
		p := x.(map[string]any)
		states, _ := p["states"].(map[string]any)
		s := counts(states)
		if h, ok := p["health"].(map[string]any); ok {
			s = counts(h)
			if r, _ := states["running"].(float64); int(r) < total(states) {
				s += " (" + counts(states) + ")"
			}
		}
		out = append(out, fmt.Sprintf("%v: %s", p["name"], s))
	}
	return out
}

func total(m map[string]any) int {
	t := 0
	for _, v := range m {
		f, _ := v.(float64)
		t += int(f)
	}
	return t
}

// memberList parses "1,2,57" or "1-5,57" into member indices.
func memberList(s string) ([]uint32, error) {
	var out []uint32
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		a, b, isRange := strings.Cut(part, "-")
		lo, err := strconv.ParseUint(a, 10, 32)
		hi := lo
		if err == nil && isRange {
			hi, err = strconv.ParseUint(b, 10, 32)
		}
		if err != nil || lo < 1 || hi < lo || hi-lo > 4096 {
			return nil, fmt.Errorf("members %q: indices and ranges like 1-5,57", s)
		}
		for i := lo; i <= hi; i++ {
			out = append(out, uint32(i))
		}
	}
	return out, nil
}

// flatten lists a config's scalar values by path ("peers[0].address").
func flatten(v any, at string, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, y := range x {
			p := k
			if at != "" {
				p = at + "." + k
			}
			flatten(y, p, out)
		}
	case []any:
		for i, y := range x {
			flatten(y, fmt.Sprintf("%s[%d]", at, i), out)
		}
	default:
		out[at] = fmt.Sprint(x)
	}
}

// previewGroup prints what a spec renders to without applying it: for an
// existing group what applying changes, then the chosen members (default:
// the first two and the last) with the protocol values that differ
// between them. raw (-o given) prints the preview as YAML or JSON.
func previewGroup(spec *structpb.Struct, members string, raw bool) error {
	which, err := memberList(members)
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	r, err := client().PreviewDeviceGroup(c, connect.NewRequest(&v1.PreviewDeviceGroupRequest{Group: &v1.DeviceGroup{Spec: spec}, Members: which}))
	if err != nil {
		return err
	}
	res := r.Msg.Result.AsMap()
	if raw {
		return printMsg(r.Msg.Result)
	}
	if p, ok := res["plan"].(map[string]any); ok {
		n := func(k string) int { f, _ := p[k].(float64); return int(f) }
		if n("changed")+n("added")+n("removed") == 0 {
			fmt.Println("applying changes nothing")
		} else {
			fmt.Printf("applying: %d changed, %d added, %d removed, %d unchanged (keep running)\n", n("changed"), n("added"), n("removed"), n("unchanged"))
		}
	}
	ms, _ := res["members"].([]any)
	fmt.Printf("%v members; showing %d\n\n", res["count"], len(ms))
	// The protocol values that differ between the members shown.
	vals := make([]map[string]string, len(ms))
	for i, x := range ms {
		vals[i] = map[string]string{}
		ps, _ := x.(map[string]any)["protocols"].([]any)
		for _, y := range ps {
			p := y.(map[string]any)
			dev := x.(map[string]any)["device"].(map[string]any)["name"].(string)
			flatten(p["config"], strings.TrimPrefix(p["name"].(string), dev+"-"), vals[i])
		}
	}
	var varying []string
	if len(vals) > 0 {
		for k, v := range vals[0] {
			for _, o := range vals[1:] {
				if o[k] != v {
					varying = append(varying, k)
					break
				}
			}
		}
	}
	sort.Strings(varying)
	w := table()
	fmt.Fprintln(w, "#\tDEVICE\tVLANS\tADDRESSES\tMAC\tVARIES")
	for i, x := range ms {
		m := x.(map[string]any)
		d := m["device"].(map[string]any)
		var vl []string
		tags, _ := d["vlans"].([]any)
		for _, t := range tags {
			vl = append(vl, fmt.Sprint(t.(map[string]any)["vid"]))
		}
		var addrs []string
		al, _ := d["addresses"].([]any)
		for _, a := range al {
			addrs = append(addrs, fmt.Sprint(a))
		}
		var vs []string
		for _, k := range varying {
			vs = append(vs, k+"="+vals[i][k])
		}
		fmt.Fprintf(w, "%v\t%v\t%s\t%s\t%v\t%s\n", m["index"], d["name"], dash(strings.Join(vl, ".")), dash(strings.Join(addrs, ",")), m["mac"], dash(strings.Join(vs, " ")))
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func groupsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "groups", Short: "List device groups",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().ListDeviceGroups(c, connect.NewRequest(&v1.ListDeviceGroupsRequest{}))
			if err != nil {
				return err
			}
			w := table()
			fmt.Fprintln(w, "GROUP\tDEVICES\tPROTOCOLS")
			for _, g := range r.Msg.Groups {
				count := g.Spec.AsMap()["count"]
				fmt.Fprintf(w, "%s\t%v/%v\t%s\n", g.Name, g.Status.AsMap()["devices"], count, strings.Join(groupProtocols(g), "; "))
			}
			return w.Flush()
		}}
}

func groupCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "group", Short: "Create, inspect, change and drive device groups"}

	var dryRun bool
	var showMembers string
	add := &cobra.Command{
		Use: "add FILE", Short: "Create a group from a YAML spec ('-' for stdin)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := readSpec(args[0])
			if err != nil {
				return err
			}
			if dryRun {
				return previewGroup(spec, showMembers, cmd.Flag("output").Changed)
			}
			c, cancel := ctx()
			defer cancel()
			r, err := client().CreateDeviceGroup(c, connect.NewRequest(&v1.CreateDeviceGroupRequest{Group: &v1.DeviceGroup{Spec: spec}}))
			if err != nil {
				return err
			}
			fmt.Printf("%s: %v devices; %s\n", r.Msg.Group.Name, r.Msg.Group.Status.AsMap()["devices"], strings.Join(groupProtocols(r.Msg.Group), "; "))
			return nil
		}}

	set := &cobra.Command{
		Use: "set FILE", Short: "Replace a group's spec (unchanged members keep running)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := readSpec(args[0])
			if err != nil {
				return err
			}
			if dryRun {
				return previewGroup(spec, showMembers, cmd.Flag("output").Changed)
			}
			c, cancel := ctx()
			defer cancel()
			r, err := client().UpdateDeviceGroup(c, connect.NewRequest(&v1.UpdateDeviceGroupRequest{Group: &v1.DeviceGroup{Spec: spec}}))
			if err != nil {
				return err
			}
			fmt.Printf("%s: %v devices; %s\n", r.Msg.Group.Name, r.Msg.Group.Status.AsMap()["devices"], strings.Join(groupProtocols(r.Msg.Group), "; "))
			return nil
		}}

	for _, c := range []*cobra.Command{add, set} {
		c.Flags().BoolVar(&dryRun, "dry-run", false, "show what the spec renders to (and for set, what applying changes) without applying it")
		c.Flags().StringVar(&showMembers, "members", "", "with --dry-run: members to show, e.g. 1-3,57 (default: the first two and the last)")
	}

	var specOnly bool
	show := &cobra.Command{
		Use: "show NAME", Short: "Show a group's members and their protocol states", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			r, err := client().GetDeviceGroup(c, connect.NewRequest(&v1.GetDeviceGroupRequest{Name: args[0], SummaryOnly: specOnly}))
			if err != nil {
				return err
			}
			g := r.Msg.Group
			if specOnly {
				return printMsg(g.Spec)
			}
			fmt.Printf("%s: %v/%v devices; %s\n\n", g.Name, g.Status.AsMap()["devices"], g.Spec.AsMap()["count"], strings.Join(groupProtocols(g), "; "))
			w := table()
			fmt.Fprintln(w, "#\tDEVICE\tVLANS\tADDRESSES\tPROTOCOLS")
			ms, _ := g.Status.AsMap()["members"].([]any)
			for _, x := range ms {
				m := x.(map[string]any)
				var ps []string
				pl, _ := m["protocols"].([]any)
				for _, y := range pl {
					p := y.(map[string]any)
					st := p["state"]
					if h, _ := p["health"].(string); h != "" && st == "running" {
						st = h
					} else if e, _ := p["error"].(string); e != "" {
						st = fmt.Sprintf("%v (%s)", st, e)
					}
					ps = append(ps, fmt.Sprintf("%v:%v", p["name"], st))
				}
				// Configured addresses, then leased ones and delegated prefixes, tagged.
				var addrs []string
				for _, f := range []struct{ key, tag string }{{"addresses", ""}, {"leased", " (dhcp)"}, {"delegated", " (pd)"}} {
					al, _ := m[f.key].([]any)
					for _, a := range al {
						addrs = append(addrs, fmt.Sprint(a)+f.tag)
					}
				}
				fmt.Fprintf(w, "%v\t%v\t%v\t%s\t%s\n", m["index"], m["device"], m["vlans"], strings.Join(addrs, ","), strings.Join(ps, " "))
			}
			return w.Flush()
		}}
	show.Flags().BoolVar(&specOnly, "spec", false, "print the group's spec instead")

	del := &cobra.Command{
		Use: "del NAME...", Aliases: []string{"delete", "rm"}, Short: "Delete groups and their members", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cancel := ctx()
			defer cancel()
			for _, n := range args {
				if _, err := client().DeleteDeviceGroup(c, connect.NewRequest(&v1.DeleteDeviceGroupRequest{Name: n})); err != nil {
					return err
				}
			}
			return nil
		}}

	var argsJSON, members, proto string
	var random uint32
	var full bool
	run := &cobra.Command{
		Use: "run NAME ACTION [key=value...]", Short: "Run a protocol action on group members", Args: cobra.MinimumNArgs(2),
		Example: "  clabgenctl group run ce flap --members 1-20 seconds=10\n  clabgenctl group run ce reset --random 5\n  clabgenctl group run ce withdraw --protocol bgp --args @routes.yml",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := buildStruct(argsJSON, args[2:])
			if err != nil {
				return err
			}
			c, cancel := ctx()
			defer cancel()
			r, err := client().RunGroupAction(c, connect.NewRequest(&v1.RunGroupActionRequest{Group: args[0], Protocol: proto, Action: args[1],
				Args: a, Members: members, Random: random}))
			if err != nil {
				return err
			}
			if full {
				return printMsg(r.Msg)
			}
			failed := 0
			for _, x := range r.Msg.Results {
				if x.Error != "" {
					failed++
					fmt.Printf("%d %s: %s\n", x.Index, x.Protocol, x.Error)
				}
			}
			fmt.Printf("%s on %d members: %d ok, %d failed\n", args[1], len(r.Msg.Results), len(r.Msg.Results)-failed, failed)
			if failed > 0 {
				return fmt.Errorf("%d members failed", failed)
			}
			return nil
		}}
	run.Flags().StringVar(&argsJSON, "args", "", "arguments as JSON/YAML, or @file")
	run.Flags().StringVar(&members, "members", "", "members by 1-based index, e.g. 1-10,15 (default all)")
	run.Flags().Uint32Var(&random, "random", 0, "run on this many of the selected members, picked at random")
	run.Flags().StringVar(&proto, "protocol", "", "the group protocol (needed when the group has several)")
	run.Flags().BoolVar(&full, "results", false, "print every member's result")

	toggle := func(use, short string, disabled bool) *cobra.Command {
		var proto, members string
		var random uint32
		c := &cobra.Command{
			Use: use + " NAME", Short: short, Args: cobra.ExactArgs(1),
			Example: fmt.Sprintf("  clabgenctl group %[1]s ce\n  clabgenctl group %[1]s ce --protocol bgp --members 1-10\n  clabgenctl group %[1]s ce --random 5", use),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, cancel := ctx()
				defer cancel()
				r, err := client().SetGroupDisabled(c, connect.NewRequest(&v1.SetGroupDisabledRequest{Group: args[0], Protocol: proto,
					Members: members, Random: random, Disabled: disabled}))
				if err != nil {
					return err
				}
				failed := 0
				for _, x := range r.Msg.Results {
					if x.Error != "" {
						failed++
						fmt.Printf("%d %s: %s\n", x.Index, x.Protocol, x.Error)
					}
				}
				fmt.Printf("%sd %d instances: %d ok, %d failed\n", use, len(r.Msg.Results), len(r.Msg.Results)-failed, failed)
				if failed > 0 {
					return fmt.Errorf("%d instances failed", failed)
				}
				return nil
			}}
		c.Flags().StringVar(&proto, "protocol", "", "the group protocol (default all)")
		c.Flags().StringVar(&members, "members", "", "members by 1-based index, e.g. 1-10,15 (default all, which is kept in the group spec)")
		c.Flags().Uint32Var(&random, "random", 0, "only this many of the selected members, picked at random")
		return c
	}
	disable := toggle("disable", "Stop group protocols on members, keeping their config", true)
	enable := toggle("enable", "Start disabled group protocols on members again", false)

	cmd.AddCommand(add, set, show, disable, enable, del, run)
	return cmd
}
