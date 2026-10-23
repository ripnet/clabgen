// Package server exposes the engine over Connect/gRPC.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"sigs.k8s.io/yaml"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/events"
	"github.com/ripnet/clabgen/internal/ipstack"
	"github.com/ripnet/clabgen/internal/pkt"
	"github.com/ripnet/clabgen/internal/port"
	"github.com/ripnet/clabgen/internal/protocol"
	"github.com/ripnet/clabgen/internal/version"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
	"github.com/ripnet/clabgen/gen/clabgen/v1/clabgenv1connect"
)

// Ports is the subset of the port manager the API needs.
type Ports interface {
	List() []port.Info
	Get(name string) (port.Info, bool)
	SetAdmin(name string, up bool) error
}

type Server struct {
	clabgenv1connect.UnimplementedAgentServiceHandler
	eng   *core.Engine
	ports Ports
	start time.Time
}

func New(eng *core.Engine, ports Ports) *Server {
	return &Server{eng: eng, ports: ports, start: time.Now()}
}

func connectErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, core.ErrNotFound), errors.Is(err, port.ErrNoPort):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, core.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}

func toStruct(v any) (*structpb.Struct, error) {
	if v == nil {
		return nil, nil
	}
	var b []byte
	switch x := v.(type) {
	case json.RawMessage:
		b = x
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			return nil, err
		}
	}
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(b, s); err != nil {
		return nil, err
	}
	return s, nil
}

func fromStruct(s *structpb.Struct) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	return protojson.Marshal(s)
}

func portPB(i port.Info) *v1.Port {
	return &v1.Port{Name: i.Name, Index: int32(i.Index), Mac: i.MAC.String(), AdminUp: i.AdminUp, OperUp: i.OperUp,
		Mtu: int32(i.MTU), Counters: &v1.PortCounters{RxFrames: i.RxFrames, RxBytes: i.RxBytes, TxFrames: i.TxFrames,
			TxBytes: i.TxBytes, TxErrors: i.TxErrors, RxUnmatched: i.RxUnmatched}}
}

func (s *Server) GetInfo(ctx context.Context, _ *connect.Request[v1.GetInfoRequest]) (*connect.Response[v1.GetInfoResponse], error) {
	h, _ := os.Hostname()
	return connect.NewResponse(&v1.GetInfoResponse{Hostname: h, Version: version.Version, StartTime: timestamppb.New(s.start)}), nil
}

func (s *Server) ListPorts(ctx context.Context, _ *connect.Request[v1.ListPortsRequest]) (*connect.Response[v1.ListPortsResponse], error) {
	out := &v1.ListPortsResponse{}
	for _, p := range s.ports.List() {
		out.Ports = append(out.Ports, portPB(p))
	}
	return connect.NewResponse(out), nil
}

func (s *Server) SetPortAdmin(ctx context.Context, r *connect.Request[v1.SetPortAdminRequest]) (*connect.Response[v1.SetPortAdminResponse], error) {
	if err := s.ports.SetAdmin(r.Msg.Name, r.Msg.Up); err != nil {
		return nil, connectErr(err)
	}
	// netlink reports the change asynchronously; give it a moment so the
	// returned state is current.
	var info port.Info
	for i := 0; i < 20; i++ {
		var ok bool
		if info, ok = s.ports.Get(r.Msg.Name); !ok || info.AdminUp == r.Msg.Up {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp := &v1.SetPortAdminResponse{}
	if info.Name != "" {
		resp.Port = portPB(info)
	}
	return connect.NewResponse(resp), nil
}

func devicePB(d core.DeviceSpec) *v1.Device {
	out := &v1.Device{Name: d.Name, Port: d.Port, Mac: d.MAC, MacDerived: d.MACDerived, Group: d.Group,
		Addresses: d.Addresses, Gateway4: d.Gateway4, Gateway6: d.Gateway6}
	for _, t := range d.VLANs {
		out.Vlans = append(out.Vlans, &v1.VlanTag{Tpid: uint32(t.TPID), Vid: uint32(t.VID), Pcp: uint32(t.PCP)})
	}
	return out
}

func (s *Server) ListDevices(ctx context.Context, _ *connect.Request[v1.ListDevicesRequest]) (*connect.Response[v1.ListDevicesResponse], error) {
	out := &v1.ListDevicesResponse{}
	for _, d := range s.eng.Devices() {
		out.Devices = append(out.Devices, devicePB(d))
	}
	return connect.NewResponse(out), nil
}

func deviceSpec(d *v1.Device) (core.DeviceSpec, error) {
	if d == nil {
		return core.DeviceSpec{}, connect.NewError(connect.CodeInvalidArgument, errors.New("device is required"))
	}
	spec := core.DeviceSpec{Name: d.Name, Port: d.Port, MAC: d.Mac, Addresses: d.Addresses, Gateway4: d.Gateway4, Gateway6: d.Gateway6}
	for _, t := range d.Vlans {
		if t.Tpid > 0xffff || t.Vid > 4095 || t.Pcp > 7 {
			return spec, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid VLAN tag"))
		}
		spec.VLANs = append(spec.VLANs, pkt.VLANTag{TPID: uint16(t.Tpid), VID: uint16(t.Vid), PCP: uint8(t.Pcp)})
	}
	return spec, nil
}

func (s *Server) currentDevice(name string) *v1.Device {
	for _, x := range s.eng.Devices() {
		if x.Name == name {
			return devicePB(x)
		}
	}
	return nil
}

func (s *Server) CreateDevice(ctx context.Context, r *connect.Request[v1.CreateDeviceRequest]) (*connect.Response[v1.CreateDeviceResponse], error) {
	spec, err := deviceSpec(r.Msg.Device)
	if err != nil {
		return nil, err
	}
	if _, err := s.eng.CreateDevice(spec); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&v1.CreateDeviceResponse{Device: s.currentDevice(spec.Name)}), nil
}

func (s *Server) UpdateDevice(ctx context.Context, r *connect.Request[v1.UpdateDeviceRequest]) (*connect.Response[v1.UpdateDeviceResponse], error) {
	spec, err := deviceSpec(r.Msg.Device)
	if err != nil {
		return nil, err
	}
	if _, err := s.eng.UpdateDevice(spec); err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&v1.UpdateDeviceResponse{Device: s.currentDevice(spec.Name)}), nil
}

func (s *Server) DeleteDevice(ctx context.Context, r *connect.Request[v1.DeleteDeviceRequest]) (*connect.Response[v1.DeleteDeviceResponse], error) {
	return connect.NewResponse(&v1.DeleteDeviceResponse{}), connectErr(s.eng.DeleteDevice(r.Msg.Name))
}

func (s *Server) GetDeviceIP(ctx context.Context, r *connect.Request[v1.GetDeviceIPRequest]) (*connect.Response[v1.GetDeviceIPResponse], error) {
	st, err := s.eng.DeviceIP(r.Msg.Name)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &v1.GetDeviceIPResponse{Addresses: st.Addresses, Gateway4: st.Gateway4, Gateway6: st.Gateway6, Counters: st.Counters}
	for _, n := range st.Neighbors {
		out.Neighbors = append(out.Neighbors, &v1.Neighbor{Ip: n.IP, Mac: n.MAC, State: n.State, AgeMs: n.Age.Milliseconds()})
	}
	return connect.NewResponse(out), nil
}

func (s *Server) ResolveNeighbor(ctx context.Context, r *connect.Request[v1.ResolveNeighborRequest]) (*connect.Response[v1.ResolveNeighborResponse], error) {
	ip, err := netip.ParseAddr(r.Msg.Ip)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid IP %q", r.Msg.Ip))
	}
	start := time.Now()
	nh, mac, cached, err := s.eng.ResolveNeighbor(ctx, r.Msg.Device, ip)
	if err != nil {
		if errors.Is(err, ipstack.ErrUnresolved) {
			return nil, connect.NewError(connect.CodeDeadlineExceeded, err)
		}
		return nil, connectErr(err)
	}
	return connect.NewResponse(&v1.ResolveNeighborResponse{NextHop: nh.String(), Mac: mac.String(), Cached: cached,
		RttUs: time.Since(start).Microseconds()}), nil
}

func (s *Server) ClearNeighbors(ctx context.Context, r *connect.Request[v1.ClearNeighborsRequest]) (*connect.Response[v1.ClearNeighborsResponse], error) {
	return connect.NewResponse(&v1.ClearNeighborsResponse{}), connectErr(s.eng.ClearNeighbors(r.Msg.Device))
}

func (s *Server) ListReceivedFlows(ctx context.Context, r *connect.Request[v1.ListReceivedFlowsRequest]) (*connect.Response[v1.ListReceivedFlowsResponse], error) {
	list, err := s.eng.ReceivedFlows(r.Msg.Device)
	if err != nil {
		return nil, connectErr(err)
	}
	out := &v1.ListReceivedFlowsResponse{}
	for _, f := range list {
		st, err := toStruct(f.Flow)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out.Flows = append(out.Flows, &v1.ReceivedFlow{Device: f.Device, Stats: st})
	}
	return connect.NewResponse(out), nil
}

func (s *Server) ClearReceivedFlows(ctx context.Context, r *connect.Request[v1.ClearReceivedFlowsRequest]) (*connect.Response[v1.ClearReceivedFlowsResponse], error) {
	var id *uint32
	if r.Msg.FlowId != "" {
		v, err := strconv.ParseUint(r.Msg.FlowId, 16, 32)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid flow_id %q (8 hex digits)", r.Msg.FlowId))
		}
		x := uint32(v)
		id = &x
	}
	return connect.NewResponse(&v1.ClearReceivedFlowsResponse{}), connectErr(s.eng.ClearReceivedFlows(r.Msg.Device, id))
}

func (s *Server) ListProtocolTypes(ctx context.Context, _ *connect.Request[v1.ListProtocolTypesRequest]) (*connect.Response[v1.ListProtocolTypesResponse], error) {
	out := &v1.ListProtocolTypesResponse{}
	for _, t := range protocol.Types() {
		pt := &v1.ProtocolType{Name: t.Name(), Description: t.Description(), ConfigSchema: protocol.Schema(t.NewConfig()),
			Category: protocol.CategoryOf(t)}
		for _, a := range t.Actions() {
			at := &v1.ActionType{Name: a.Name, Description: a.Description, ArgsSchema: "{}"}
			if a.NewArgs != nil {
				at.ArgsSchema = protocol.Schema(a.NewArgs())
			}
			pt.Actions = append(pt.Actions, at)
		}
		out.Types = append(out.Types, pt)
	}
	return connect.NewResponse(out), nil
}

func protocolPB(v core.ProtocolView) (*v1.Protocol, error) {
	cfg, err := toStruct(v.Config)
	if err != nil {
		return nil, err
	}
	st, err := toStruct(v.Status)
	if err != nil {
		return nil, err
	}
	return &v1.Protocol{Name: v.Name, Type: v.Type, Device: v.Device, Config: cfg, State: v.State, Error: v.Error, Status: st, Group: v.Group, Disabled: v.Disabled}, nil
}

func (s *Server) ListProtocols(ctx context.Context, r *connect.Request[v1.ListProtocolsRequest]) (*connect.Response[v1.ListProtocolsResponse], error) {
	out := &v1.ListProtocolsResponse{}
	for _, v := range s.eng.ProtocolList(r.Msg.WithStatus, r.Msg.Brief, r.Msg.NoGroupMembers) {
		p, err := protocolPB(v)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out.Protocols = append(out.Protocols, p)
	}
	return connect.NewResponse(out), nil
}

func (s *Server) GetProtocol(ctx context.Context, r *connect.Request[v1.GetProtocolRequest]) (*connect.Response[v1.GetProtocolResponse], error) {
	v, err := s.eng.GetProtocol(r.Msg.Name)
	if err != nil {
		return nil, connectErr(err)
	}
	p, err := protocolPB(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.GetProtocolResponse{Protocol: p}), nil
}

func (s *Server) CreateProtocol(ctx context.Context, r *connect.Request[v1.CreateProtocolRequest]) (*connect.Response[v1.CreateProtocolResponse], error) {
	p := r.Msg.Protocol
	if p == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("protocol is required"))
	}
	raw, err := fromStruct(p.Config)
	if err != nil {
		return nil, connectErr(err)
	}
	v, err := s.eng.CreateProtocol(core.ProtocolSpec{Name: p.Name, Type: p.Type, Device: p.Device, Config: raw, Disabled: p.Disabled})
	if err != nil {
		return nil, connectErr(err)
	}
	out, err := protocolPB(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.CreateProtocolResponse{Protocol: out}), nil
}

func (s *Server) UpdateProtocol(ctx context.Context, r *connect.Request[v1.UpdateProtocolRequest]) (*connect.Response[v1.UpdateProtocolResponse], error) {
	raw, err := fromStruct(r.Msg.Config)
	if err != nil {
		return nil, connectErr(err)
	}
	v, err := s.eng.UpdateProtocol(r.Msg.Name, raw)
	if err != nil {
		return nil, connectErr(err)
	}
	out, err := protocolPB(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.UpdateProtocolResponse{Protocol: out}), nil
}

func (s *Server) DeleteProtocol(ctx context.Context, r *connect.Request[v1.DeleteProtocolRequest]) (*connect.Response[v1.DeleteProtocolResponse], error) {
	return connect.NewResponse(&v1.DeleteProtocolResponse{}), connectErr(s.eng.DeleteProtocol(r.Msg.Name))
}

func (s *Server) SetProtocolDisabled(ctx context.Context, r *connect.Request[v1.SetProtocolDisabledRequest]) (*connect.Response[v1.SetProtocolDisabledResponse], error) {
	v, err := s.eng.SetProtocolDisabled(r.Msg.Name, r.Msg.Disabled)
	if err != nil {
		return nil, connectErr(err)
	}
	out, err := protocolPB(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.SetProtocolDisabledResponse{Protocol: out}), nil
}

func (s *Server) RunProtocolAction(ctx context.Context, r *connect.Request[v1.RunProtocolActionRequest]) (*connect.Response[v1.RunProtocolActionResponse], error) {
	raw, err := fromStruct(r.Msg.Args)
	if err != nil {
		return nil, connectErr(err)
	}
	res, err := s.eng.RunAction(ctx, r.Msg.Name, r.Msg.Action, raw)
	if err != nil {
		return nil, connectErr(err)
	}
	st, err := toStruct(res)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.RunProtocolActionResponse{Result: st}), nil
}

func eventPB(e events.Event) *v1.Event {
	d, _ := toStruct(e.Data)
	return &v1.Event{Time: timestamppb.New(e.Time), Source: e.Source, Kind: e.Kind, Message: e.Message, Data: d}
}

func (s *Server) WatchEvents(ctx context.Context, r *connect.Request[v1.WatchEventsRequest], st *connect.ServerStream[v1.Event]) error {
	hist, ch, cancel := s.eng.Events().Subscribe(r.Msg.Replay)
	defer cancel()
	// Headers now: a client knows it's subscribed before the first event.
	if err := st.Send(nil); err != nil {
		return err
	}
	for _, e := range hist {
		if err := st.Send(eventPB(e)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-ch:
			if err := st.Send(eventPB(e)); err != nil {
				return err
			}
		}
	}
}

func (s *Server) GetScenario(ctx context.Context, _ *connect.Request[v1.GetScenarioRequest]) (*connect.Response[v1.GetScenarioResponse], error) {
	y, err := s.eng.Export().YAML()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.GetScenarioResponse{Yaml: string(y)}), nil
}

func (s *Server) ApplyScenario(ctx context.Context, r *connect.Request[v1.ApplyScenarioRequest]) (*connect.Response[v1.ApplyScenarioResponse], error) {
	sc, err := core.ParseScenario([]byte(r.Msg.Yaml))
	if err != nil {
		return nil, connectErr(err)
	}
	return connect.NewResponse(&v1.ApplyScenarioResponse{}), connectErr(s.eng.Apply(sc, r.Msg.Replace))
}

func groupPB(v core.GroupView) (*v1.DeviceGroup, error) {
	spec, err := toStruct(v.Spec)
	if err != nil {
		return nil, err
	}
	st, err := toStruct(v)
	if err != nil {
		return nil, err
	}
	y, err := yaml.Marshal(v.Spec)
	if err != nil {
		return nil, err
	}
	return &v1.DeviceGroup{Name: v.Spec.Name, Spec: spec, Status: st, Yaml: string(y)}, nil
}

func groupSpec(g *v1.DeviceGroup) (core.GroupSpec, error) {
	var spec core.GroupSpec
	if g != nil && g.Spec == nil && g.Yaml != "" {
		if err := yaml.UnmarshalStrict([]byte(g.Yaml), &spec); err != nil {
			return spec, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("group spec: %w", err))
		}
		if spec.Name == "" {
			spec.Name = g.Name
		}
		return spec, nil
	}
	if g == nil || g.Spec == nil {
		return spec, connect.NewError(connect.CodeInvalidArgument, errors.New("group spec is required"))
	}
	raw, err := fromStruct(g.Spec)
	if err != nil {
		return spec, connectErr(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("group spec: %w", err))
	}
	if spec.Name == "" {
		spec.Name = g.Name
	}
	return spec, nil
}

// groupReply converts a create/update result; a partly failed change still
// returns the error.
func groupReply(v core.GroupView, err error) (*v1.DeviceGroup, error) {
	if err != nil {
		return nil, connectErr(err)
	}
	out, err := groupPB(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return out, nil
}

func (s *Server) ListDeviceGroups(ctx context.Context, _ *connect.Request[v1.ListDeviceGroupsRequest]) (*connect.Response[v1.ListDeviceGroupsResponse], error) {
	out := &v1.ListDeviceGroupsResponse{}
	for _, v := range s.eng.Groups() {
		g, err := groupPB(v)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out.Groups = append(out.Groups, g)
	}
	return connect.NewResponse(out), nil
}

func (s *Server) GetDeviceGroup(ctx context.Context, r *connect.Request[v1.GetDeviceGroupRequest]) (*connect.Response[v1.GetDeviceGroupResponse], error) {
	v, err := s.eng.GetGroup(r.Msg.Name, !r.Msg.SummaryOnly)
	if err != nil {
		return nil, connectErr(err)
	}
	g, err := groupReply(v, nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetDeviceGroupResponse{Group: g}), nil
}

func (s *Server) CreateDeviceGroup(ctx context.Context, r *connect.Request[v1.CreateDeviceGroupRequest]) (*connect.Response[v1.CreateDeviceGroupResponse], error) {
	spec, err := groupSpec(r.Msg.Group)
	if err != nil {
		return nil, err
	}
	g, err := groupReply(s.eng.CreateGroup(spec))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CreateDeviceGroupResponse{Group: g}), nil
}

func (s *Server) UpdateDeviceGroup(ctx context.Context, r *connect.Request[v1.UpdateDeviceGroupRequest]) (*connect.Response[v1.UpdateDeviceGroupResponse], error) {
	spec, err := groupSpec(r.Msg.Group)
	if err != nil {
		return nil, err
	}
	g, err := groupReply(s.eng.UpdateGroup(spec))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.UpdateDeviceGroupResponse{Group: g}), nil
}

func (s *Server) DeleteDeviceGroup(ctx context.Context, r *connect.Request[v1.DeleteDeviceGroupRequest]) (*connect.Response[v1.DeleteDeviceGroupResponse], error) {
	return connect.NewResponse(&v1.DeleteDeviceGroupResponse{}), connectErr(s.eng.DeleteGroup(r.Msg.Name))
}

func (s *Server) SetGroupDisabled(ctx context.Context, r *connect.Request[v1.SetGroupDisabledRequest]) (*connect.Response[v1.SetGroupDisabledResponse], error) {
	res, err := s.eng.SetGroupDisabled(core.GroupDisableRequest{Group: r.Msg.Group, Protocol: r.Msg.Protocol, Members: r.Msg.Members,
		Random: int(r.Msg.Random), Disabled: r.Msg.Disabled})
	if err != nil {
		return nil, connectErr(err)
	}
	out := &v1.SetGroupDisabledResponse{}
	for _, x := range res {
		out.Results = append(out.Results, &v1.GroupActionResult{Index: uint32(x.Index), Protocol: x.Protocol, Error: x.Error})
	}
	return connect.NewResponse(out), nil
}

func (s *Server) RunGroupAction(ctx context.Context, r *connect.Request[v1.RunGroupActionRequest]) (*connect.Response[v1.RunGroupActionResponse], error) {
	raw, err := fromStruct(r.Msg.Args)
	if err != nil {
		return nil, connectErr(err)
	}
	res, err := s.eng.RunGroupAction(ctx, core.GroupActionRequest{Group: r.Msg.Group, Protocol: r.Msg.Protocol, Action: r.Msg.Action,
		Args: raw, Members: r.Msg.Members, Random: int(r.Msg.Random)})
	if err != nil {
		return nil, connectErr(err)
	}
	out := &v1.RunGroupActionResponse{}
	for _, x := range res {
		st, err := toStruct(x.Result)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		out.Results = append(out.Results, &v1.GroupActionResult{Index: uint32(x.Index), Protocol: x.Protocol, Error: x.Error, Result: st})
	}
	return connect.NewResponse(out), nil
}
