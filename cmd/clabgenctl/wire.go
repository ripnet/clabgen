package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
	"github.com/ripnet/clabgen/internal/wire"
)

// wireFlow is a flow judged against the lab's monitors.
type wireFlow struct {
	Agent   string       `json:"agent"`
	Name    string       `json:"name"`
	FlowID  string       `json:"flow_id"`
	Expect  *wire.Expect `json:"expect,omitempty"`
	Verdict wire.Verdict `json:"result"`
}

func wireCmd() *cobra.Command {
	var services, clearAll bool
	cmd := &cobra.Command{
		Use:   "wire [FLOW...]",
		Short: "VLAN tags of flows and their OAM on monitored links, checked against the flows' wire expectations",
		Long: `Collects what every monitor instance saw (with --lab, on every agent of the lab)
and checks each flow with a wire expectation, or seen by a monitor: per
direction, the VLAN tags its frames carried inside the service, and those of the
CFM/Y.1731 frames in the same service. --services lists everything each
monitor saw instead, flows or not. --clear clears every monitor, for a fresh
verdict after a change.`,
		Example: `  clabgenctl --lab cc wire
  clabgenctl --lab cc wire ping-2101 -o json
  clabgenctl --lab cc wire --services
  clabgenctl --lab cc wire --clear`,
		RunE: func(cmd *cobra.Command, args []string) error {
			agents, err := convergenceAgents()
			if err != nil {
				return err
			}
			var mons []wire.Monitor
			type flowRef struct {
				agent string
				p     *v1.Protocol
				f     wire.Flow
				has   bool
			}
			var flows []flowRef
			cleared := 0
			for _, a := range agents {
				c, cancel := ctx()
				r, err := a.c.ListProtocols(c, connect.NewRequest(&v1.ListProtocolsRequest{WithStatus: true, Brief: true}))
				if err != nil {
					cancel()
					return fmt.Errorf("%s: %w", a.name, err)
				}
				devs, err := a.c.ListDevices(c, connect.NewRequest(&v1.ListDevicesRequest{}))
				cancel()
				if err != nil {
					return fmt.Errorf("%s: %w", a.name, err)
				}
				macs := map[string]string{}
				for _, d := range devs.Msg.Devices {
					macs[d.Name] = d.Mac
				}
				for _, p := range r.Msg.Protocols {
					if p.Status == nil {
						continue
					}
					st := p.Status.AsMap()
					if p.Type == "monitor" && clearAll {
						c, cancel := ctx()
						_, err := a.c.RunProtocolAction(c, connect.NewRequest(&v1.RunProtocolActionRequest{Name: p.Name, Action: "clear"}))
						cancel()
						if err != nil {
							return fmt.Errorf("%s/%s: %w", a.name, p.Name, err)
						}
						fmt.Fprintf(cmd.OutOrStdout(), "cleared %s/%s\n", a.name, p.Name)
						cleared++
						continue
					}
					if p.Type == "monitor" {
						var ms wire.Status
						b, _ := json.Marshal(st)
						if json.Unmarshal(b, &ms) == nil {
							mons = append(mons, wire.Monitor{Name: a.name + "/" + p.Name, Status: ms})
						}
						continue
					}
					id, _ := st["flow_id"].(string)
					if id == "" {
						continue
					}
					f := wire.Flow{FlowID: id, MAC: macs[p.Device]}
					if src, _ := st["source"].(string); src != "" {
						if ip, err := netip.ParseAddr(src); err == nil {
							f.IP = ip.String()
						}
					}
					has := false
					if p.Config != nil {
						if w, ok := p.Config.AsMap()["wire"]; ok && w != nil {
							b, _ := json.Marshal(w)
							if json.Unmarshal(b, &f.Expect) == nil {
								has = true
							}
						}
					}
					flows = append(flows, flowRef{agent: a.name, p: p, f: f, has: has})
				}
			}
			if clearAll {
				if cleared == 0 {
					return fmt.Errorf("no monitor to clear")
				}
				return nil
			}
			if services {
				return printWireServices(cmd, mons)
			}
			want := map[string]bool{}
			for _, a := range args {
				want[a] = true
			}
			var out []wireFlow
			for _, fr := range flows {
				if len(want) > 0 && !want[fr.p.Name] {
					continue
				}
				v := wire.Judge(fr.f, mons)
				if !fr.has && len(seenRows(v)) == 0 {
					continue // no expectation and not seen: not about the wire
				}
				w := wireFlow{Agent: fr.agent, Name: fr.p.Name, FlowID: fr.f.FlowID, Verdict: v}
				if fr.has {
					e := fr.f.Expect
					w.Expect = &e
				}
				out = append(out, w)
			}
			sort.Slice(out, func(i, j int) bool {
				if out[i].Agent != out[j].Agent {
					return out[i].Agent < out[j].Agent
				}
				return out[i].Name < out[j].Name
			})
			if f := cmd.Flag("output"); f != nil && f.Changed {
				return printJSONOrYAML(out)
			}
			if len(mons) == 0 {
				fmt.Println("no monitor instances found (with --lab, on any agent of the lab)")
			}
			w := table()
			fmt.Fprintln(w, "FLOW\tVERDICT\tDIRECTION\tMONITOR\tLABEL\tDATA\tOAM")
			for _, f := range out {
				name := f.Agent + "/" + f.Name
				verdict := f.Verdict.Verdict
				if verdict == "" {
					verdict = "-"
				}
				for i, r := range f.Verdict.Rows {
					if i > 0 {
						name, verdict = "", ""
					}
					label := "-"
					if r.Label != nil {
						label = fmt.Sprint(*r.Label)
					}
					mon := r.Monitor
					if mon == "" {
						mon = "(not seen)"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", name, verdict, r.Direction, mon, label, classText(r.Data), classText(r.OAM))
				}
			}
			return w.Flush()
		}}
	cmd.Flags().BoolVar(&services, "services", false, "list every service the monitors saw instead")
	cmd.Flags().BoolVar(&clearAll, "clear", false, "clear every monitor (what they saw so far) instead")
	return cmd
}

func seenRows(v wire.Verdict) []wire.Row {
	var out []wire.Row
	for _, r := range v.Rows {
		if r.Monitor != "" {
			out = append(out, r)
		}
	}
	return out
}

// classText: the stacks seen, and against the expectation.
func classText(c wire.Class) string {
	var seen []string
	for _, s := range c.Seen {
		mark := ""
		switch {
		case s.Stale:
			mark = fmt.Sprintf(" (old: %.0f s ago, not judged)", s.LastS)
		case c.Expect != "":
			mark = " ✓"
			if s.Tags != c.Expect {
				mark = " ✗"
			}
		}
		seen = append(seen, fmt.Sprintf("%s ×%d%s", s.Tags, s.Count, mark))
	}
	out := strings.Join(seen, " ")
	switch {
	case c.Verdict == wire.Fail:
		out += " (want " + c.Expect + ")"
	case c.Verdict == wire.Pending:
		out = "none yet (want " + c.Expect + ")"
	case out == "":
		out = "-"
	}
	return out
}

func printWireServices(cmd *cobra.Command, mons []wire.Monitor) error {
	if f := cmd.Flag("output"); f != nil && f.Changed {
		return printJSONOrYAML(mons)
	}
	w := table()
	fmt.Fprintln(w, "MONITOR\tLABEL\tFROM\tTO\tFRAMES\tFLOWS\tOAM\tOTHER")
	for _, m := range mons {
		for _, s := range m.Status.Services {
			label, from, to := "-", s.Src, s.Dst
			if s.Label != nil {
				label = fmt.Sprint(*s.Label)
			} else {
				from, to = "(plain)", s.OuterTags
				if to == "" {
					to = wire.Untagged
				}
			}
			var data, oam, other []string
			for _, d := range s.Data {
				r := ""
				if d.Reply {
					r = " reply"
				}
				data = append(data, fmt.Sprintf("%s%s [%s]×%d", d.FlowID, r, d.Tags, d.Count))
			}
			for _, o := range s.OAM {
				oam = append(oam, fmt.Sprintf("%s %s [%s]×%d", o.Kind, o.MA, o.Tags, o.Count))
			}
			for _, o := range s.Other {
				other = append(other, fmt.Sprintf("%s×%d", o.Kind, o.Count))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", m.Name, label, from, to, s.Frames,
				orDash(strings.Join(data, " ")), orDash(strings.Join(oam, " ")), orDash(strings.Join(other, " ")))
		}
		if m.Status.Truncated {
			fmt.Fprintf(w, "%s\t(more not kept)\t\t\t\t\t\t\n", m.Name)
		}
	}
	return w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printJSONOrYAML(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return printJSON(b)
}
