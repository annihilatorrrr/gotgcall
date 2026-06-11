package wrtc

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/pion/ice/v4"

	"github.com/annihilatorrrr/gotgcall/models"
	"github.com/annihilatorrrr/gotgcall/wrtc/native"
)

// NetworkType is the candidate network-type tag the legacy API exposed.
// Values are the same constants pion uses internally so callers don't
// have to import pion/ice directly for this enum.
type NetworkType int

const (
	NetworkTypeUDP4 NetworkType = 1
	NetworkTypeUDP6 NetworkType = 2
	NetworkTypeTCP4 NetworkType = 3
	NetworkTypeTCP6 NetworkType = 4
)

// Factory hosts the per-process configuration the native Stack draws
// from on each NewPeerConnection. A single Factory is shared across
// every concurrent call.
type Factory struct {
	inner   *native.Factory
	monitor *FactoryMonitor
	log     *slog.Logger
	// mux is the shared UDP mux when SharedUDPMux is enabled, nil
	// otherwise. Its Close closes the underlying socket via pion's
	// own sync.Once, so no separate handle to that socket is kept.
	mux    ice.UDPMux
	mu     sync.Mutex
	closed bool
}

// FactoryOptions configures the per-process Factory. ICE timing,
// per-pair binding budgets, and STUN/TURN servers are handled
// internally — no caller knob, matching ntgcalls.
type FactoryOptions struct {
	Logger *slog.Logger
	// NetworkTypes whitelists the candidate network types. Empty = UDP4+UDP6.
	NetworkTypes []NetworkType
	CertPoolSize int
	SharedUDPMux bool
	// PionTraceAsDebug and LogICECandidates are surfaced by the native
	// logger plumbing where applicable.
	PionTraceAsDebug bool
	LogICECandidates bool
}

// NewFactory configures and constructs a Factory.
func NewFactory(opts FactoryOptions) (*Factory, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	netTypes := translateNetworkTypes(opts.NetworkTypes)

	var mux ice.UDPMux
	if opts.SharedUDPMux {
		muxConn, lerr := net.ListenPacket("udp4", ":0")
		if lerr != nil {
			return nil, lerr
		}
		// UDPMuxDefault.Close closes the wrapped conn — no extra handle.
		mux = ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: muxConn})
	}

	innerOpts := native.FactoryOptions{
		Logger:          log,
		CertPoolSize:    opts.CertPoolSize,
		NetworkTypes:    netTypes,
		InterfaceFilter: defaultInterfaceFilter,
		IPFilter:        defaultIPFilter,
		UDPMux:          mux,
	}
	inner, err := native.NewFactory(innerOpts)
	if err != nil {
		return nil, err
	}
	monitor := NewFactoryMonitor(log)
	monitor.Start()
	return &Factory{
		inner:   inner,
		monitor: monitor,
		log:     log,
		mux:     mux,
	}, nil
}

// Monitor returns the per-Factory shared keepalive + watchdog monitor.
// Exposed so PeerConnection.NewPeerConnection can self-register.
func (f *Factory) Monitor() *FactoryMonitor { return f.monitor }

// newStack hands out a fresh native.Stack from the factory's pool.
// Audio + video are always wired (caller's source decides whether the
// video Streamer actually writes anything).
func (f *Factory) newStack(ctx context.Context) (*native.Stack, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, models.ErrClosed
	}
	f.mu.Unlock()
	return f.inner.NewStack(ctx, true)
}

// Close shuts the factory + monitor down + releases the shared UDP
// socket (if any).
func (f *Factory) Close() error {
	f.mu.Lock()
	closed := f.closed
	f.closed = true
	f.mu.Unlock()
	if closed {
		return nil
	}
	if f.monitor != nil {
		f.monitor.Stop()
	}
	var firstErr error
	if f.inner != nil {
		if err := f.inner.Close(); err != nil {
			firstErr = err
		}
	}
	if f.mux != nil {
		if err := f.mux.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func translateNetworkTypes(in []NetworkType) []ice.NetworkType {
	if len(in) == 0 {
		return nil
	}
	out := make([]ice.NetworkType, 0, len(in))
	for _, n := range in {
		out = append(out, ice.NetworkType(n))
	}
	return out
}

// defaultInterfaceFilter skips virtual / VPN / container interfaces
// that produce unreachable ICE candidates and slow gathering.
func defaultInterfaceFilter(name string) bool {
	lower := strings.ToLower(name)
	for _, skip := range skipInterfaceSubstrings {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	return true
}

// defaultIPFilter excludes IPs from subnets that produce unreachable ICE
// candidates. Windows ICS (192.168.137.0/24) is the primary one.
func defaultIPFilter(ip net.IP) bool {
	icsNet := net.IPNet{IP: net.IP{192, 168, 137, 0}, Mask: net.CIDRMask(24, 32)}
	return !icsNet.Contains(ip)
}

var skipInterfaceSubstrings = [...]string{
	"vethernet", "vmware", "virtualbox", "vbox", "hyper-v",
	"loopback", "teredo", "isatap", "tap-",
	"docker", "wsl", "tailscale", "zerotier", "openvpn",
}
