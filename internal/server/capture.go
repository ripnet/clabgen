package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ripnet/clabgen/internal/core"
	"github.com/ripnet/clabgen/internal/pcapng"
	"github.com/ripnet/clabgen/protocols/packet"

	v1 "github.com/ripnet/clabgen/gen/clabgen/v1"
)

// Frames a capture buffers for a slow reader before dropping.
const captureBuffer = 4096

type captureSpec struct {
	core.CaptureFilter
	count   int
	seconds float64
}

func (s *Server) startCapture(c captureSpec) (*core.Capture, error) {
	for _, p := range c.Ports {
		if _, ok := s.ports.Get(p); !ok {
			return nil, fmt.Errorf("port %s: %w", p, core.ErrNotFound)
		}
	}
	return s.eng.StartCapture(c.CaptureFilter, captureBuffer)
}

// runCapture calls fn for each frame until the count or time is reached,
// ctx ends, or fn fails.
func runCapture(ctx context.Context, c *core.Capture, spec captureSpec, fn func(core.CapturedFrame) error) error {
	var end <-chan time.Time
	if spec.seconds > 0 {
		t := time.NewTimer(time.Duration(spec.seconds * float64(time.Second)))
		defer t.Stop()
		end = t.C
	}
	for n := 0; spec.count == 0 || n < spec.count; n++ {
		select {
		case <-ctx.Done():
			return nil
		case <-end:
			return nil
		case f, ok := <-c.C:
			if !ok {
				return nil
			}
			if err := fn(f); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) Capture(ctx context.Context, r *connect.Request[v1.CaptureRequest], st *connect.ServerStream[v1.CapturedFrame]) error {
	m := r.Msg
	spec := captureSpec{CaptureFilter: core.CaptureFilter{Ports: m.Ports, Device: m.Device, Direction: m.Direction, Snaplen: int(m.Snaplen)},
		count: int(m.Count), seconds: m.Seconds}
	for _, et := range m.Ethertypes {
		spec.EtherTypes = append(spec.EtherTypes, uint16(et))
	}
	c, err := s.startCapture(spec)
	if err != nil {
		return connectErr(err)
	}
	defer c.Close()
	// Headers now: the client knows the capture has started.
	if err := st.Send(nil); err != nil {
		return err
	}
	return runCapture(ctx, c, spec, func(f core.CapturedFrame) error {
		out := &v1.CapturedFrame{Time: timestamppb.New(f.Time), Port: f.Port, Direction: f.Dir, Data: f.Data,
			Length: uint32(f.Length), Dropped: c.Dropped()}
		if m.Summaries {
			out.Summary = packet.Inspect(f.Data).Summary
		}
		return st.Send(out)
	})
}

func (s *Server) DecodeFrame(ctx context.Context, r *connect.Request[v1.DecodeFrameRequest]) (*connect.Response[v1.DecodeFrameResponse], error) {
	st, err := toStruct(packet.Inspect(r.Msg.Data))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&v1.DecodeFrameResponse{Result: st}), nil
}

// CaptureHandler serves a live capture as a pcapng stream:
//
//	GET /capture.pcapng?port=eth1&port=eth2&device=d1&direction=rx&ethertype=0x0800&snaplen=128&count=100&seconds=10
//
// Every parameter is optional (no port: every port). The stream runs until
// the count or time is reached or the client goes away, e.g.
// curl -sSfN http://agent:8080/capture.pcapng?port=eth1 | wireshark -k -i -
func (s *Server) CaptureHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var spec captureSpec
		for _, p := range q["port"] {
			spec.Ports = append(spec.Ports, strings.Split(p, ",")...)
		}
		spec.Device, spec.Direction = q.Get("device"), q.Get("direction")
		for _, v := range q["ethertype"] {
			for _, e := range strings.Split(v, ",") {
				et, err := strconv.ParseUint(e, 0, 16)
				if err != nil {
					http.Error(w, fmt.Sprintf("ethertype %q: a number like 0x0800", e), http.StatusBadRequest)
					return
				}
				spec.EtherTypes = append(spec.EtherTypes, uint16(et))
			}
		}
		for k, dst := range map[string]*int{"snaplen": &spec.Snaplen, "count": &spec.count} {
			if v := q.Get(k); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 {
					http.Error(w, k+": a whole number", http.StatusBadRequest)
					return
				}
				*dst = n
			}
		}
		if v := q.Get("seconds"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				http.Error(w, "seconds: a number", http.StatusBadRequest)
				return
			}
			spec.seconds = f
		}
		c, err := s.startCapture(spec)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer c.Close()

		// One pcapng interface per port.
		var names []string
		for _, p := range s.ports.List() {
			if len(spec.Ports) == 0 || slices.Contains(spec.Ports, p.Name) {
				names = append(names, p.Name)
			}
		}
		slices.Sort(names)
		ifs := make([]pcapng.Interface, len(names))
		for i, n := range names {
			ifs[i] = pcapng.Interface{Name: n, Snaplen: spec.Snaplen}
		}
		host, _ := os.Hostname()
		w.Header().Set("Content-Type", "application/x-pcapng")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="clabgen-%s-%s.pcapng"`, host, time.Now().Format("20060102-150405")))
		w.Header().Set("Cache-Control", "no-store")
		pw, err := pcapng.NewWriter(w, ifs, "clabgen")
		if err != nil {
			return
		}
		runCapture(r.Context(), c, spec, func(f core.CapturedFrame) error {
			dir := pcapng.Inbound
			if f.Dir == "tx" {
				dir = pcapng.Outbound
			}
			return pw.WritePacket(slices.Index(names, f.Port), f.Time, f.Data, f.Length, dir)
		})
	})
}
