package native

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/pion/ice/v4"
)

// Factory pools the per-call inputs that don't change between Stacks:
// the DTLS cert pool, the ICE network types / interface filters, and
// an optional shared UDP mux.
//
// Goroutine budget at the Factory layer: 0. Goroutines are paid by the
// CertPool refill loop (1 — shared across N calls) and by each Stack
// (its ice.Agent + a single drain Read loop after Connect).
type Factory struct {
	udpMux          ice.UDPMux
	log             *slog.Logger
	certPool        *CertPool
	interfaceFilter func(name string) bool
	ipFilter        func(ip net.IP) bool
	networkTypes    []ice.NetworkType

	mu sync.Mutex

	// monotonic per-Factory SSRC source so successive calls don't collide
	// SSRCs within the same process. Audio uses ssrcCounter; video uses
	// ssrcCounter+1; the FID rtx slot we declare in the JOIN payload is
	// ssrcCounter+2.
	ssrcCounter atomic.Uint32

	closed bool
}

// FactoryOptions configures Factory construction. STUN/TURN servers
// and ICE timing knobs are not exposed — ntgcalls doesn't expose them
// either; host candidates + pion defaults cover every real deployment.
type FactoryOptions struct {
	UDPMux          ice.UDPMux
	Logger          *slog.Logger
	InterfaceFilter func(name string) bool
	IPFilter        func(ip net.IP) bool
	NetworkTypes    []ice.NetworkType
	CertPoolSize    int
}

// NewFactory constructs a Factory and starts its cert-pool refill.
func NewFactory(opts FactoryOptions) (*Factory, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.CertPoolSize == 0 {
		opts.CertPoolSize = 8
	}
	networkTypes := opts.NetworkTypes
	if len(networkTypes) == 0 {
		networkTypes = []ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6}
	}
	f := &Factory{
		log:             log,
		certPool:        NewCertPool(opts.CertPoolSize, log),
		networkTypes:    networkTypes,
		interfaceFilter: opts.InterfaceFilter,
		ipFilter:        opts.IPFilter,
		udpMux:          opts.UDPMux,
	}
	// Seed the SSRC counter randomly so a process restart doesn't collide
	// with the previous run's calls if Telegram still has them cached.
	var seed [4]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	f.ssrcCounter.Store(binary.BigEndian.Uint32(seed[:]) | 1)
	return f, nil
}

// NewStack constructs a per-call Stack from the factory-shared resources.
// audioOnly callers pass false to skip video SSRC allocation; the
// returned Stack's VideoTrack() will be nil.
func (f *Factory) NewStack(ctx context.Context, withVideo bool) (*Stack, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, ErrClosed
	}
	f.mu.Unlock()

	cert, err := f.certPool.Take(ctx)
	if err != nil {
		return nil, err
	}

	audioSSRC := f.allocateSSRC()
	var videoSSRC uint32
	if withVideo {
		videoSSRC = f.allocateSSRC()
		// Skip the slot videoSSRC+1 so the FID rtx SSRC we declare in the
		// JOIN payload doesn't collide with the next call's audio SSRC.
		_ = f.allocateSSRC()
	}

	return NewStack(Options{
		Logger:          f.log,
		Cert:            cert,
		NetworkTypes:    f.networkTypes,
		UDPMux:          f.udpMux,
		InterfaceFilter: f.interfaceFilter,
		IPFilter:        f.ipFilter,
		AudioSSRC:       audioSSRC,
		VideoSSRC:       videoSSRC,
	})
}

// Close stops the cert-pool refill loop. Existing Stacks keep running
// until their own Close.
func (f *Factory) Close() error {
	f.mu.Lock()
	closed := f.closed
	f.closed = true
	f.mu.Unlock()
	if closed {
		return nil
	}
	if f.certPool != nil {
		f.certPool.Close()
	}
	return nil
}

func (f *Factory) allocateSSRC() uint32 {
	for {
		v := f.ssrcCounter.Add(1)
		// SSRC=0 is reserved by RTP; skip it on the wraparound.
		if v != 0 {
			return v
		}
	}
}

// ErrClosed is returned by Factory operations after Close.
var ErrClosed = errClosed{}

type errClosed struct{}

func (errClosed) Error() string { return "native: factory closed" }
