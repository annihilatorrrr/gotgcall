package wrtc

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/annihilatorrrr/gotgcall/models"
)

// Tunables for the FactoryMonitor. The keepalive cadence is short enough
// that Telegram's SFU never sees a long enough "no-video" gap to GC the
// SSRC binding, and long enough that the bandwidth overhead is negligible
// (one padding packet per ~60 real VP8 frames at 30 fps).
const (
	keepaliveTickInterval = 2 * time.Second
	monitorPollInterval   = time.Second
	// iceCheckingTimeout — how long a PC that has NEVER reached Connected
	// may stay in setup before the monitor force-closes it. Must exceed
	// both the SetSource gate (10s default) and pion's failedTimeout (30s
	// after NewStack's tuning) so the caller's own error path and pion's
	// internal Failed transition both fire first with cleaner signals.
	iceCheckingTimeout = 35 * time.Second
)

// FactoryMonitor is a SINGLE per-Factory goroutine that
//  1. Generates a VP8 padding packet every keepaliveTickInterval on
//     every registered PC's video Track, keeping Telegram's SFU video
//     SSRC binding warm.
//  2. Force-closes PCs stuck in Connecting beyond iceCheckingTimeout —
//     a safety net for callers that ignore SetSource errors and would
//     otherwise leak the underlying ICE agent + UDP socket.
//
// Goroutine budget: 1 goroutine per Factory, regardless of concurrent
// call count. Per-PC work is dispatched from the tick loop synchronously.
type FactoryMonitor struct {
	log     *slog.Logger
	ctx     context.Context
	cancel  context.CancelFunc
	entries map[*PeerConnection]*pcMonitorEntry
	mu      sync.RWMutex
	started atomic.Bool
	stopped atomic.Bool
}

// NewFactoryMonitor constructs a stopped monitor. Call Start exactly
// once (typically from NewFactory).
func NewFactoryMonitor(log *slog.Logger) *FactoryMonitor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &FactoryMonitor{
		log:     log,
		ctx:     ctx,
		cancel:  cancel,
		entries: make(map[*PeerConnection]*pcMonitorEntry),
	}
}

func (m *FactoryMonitor) Start() {
	if m == nil {
		return
	}
	if m.started.CompareAndSwap(false, true) {
		go m.run()
	}
}

func (m *FactoryMonitor) Stop() {
	if m == nil {
		return
	}
	if m.stopped.CompareAndSwap(false, true) {
		m.cancel()
	}
}

// Register adds pc to the monitor's working set. Called from
// NewPeerConnection.
func (m *FactoryMonitor) Register(pc *PeerConnection) {
	if m == nil || pc == nil {
		return
	}
	entry := &pcMonitorEntry{pc: pc, log: pc.log}
	m.mu.Lock()
	m.entries[pc] = entry
	m.mu.Unlock()
}

// Unregister removes pc from the monitor's working set. Called from
// PeerConnection.Close — both user-initiated closes and the monitor's
// own force-close path go through it, so the entry is always reaped.
func (m *FactoryMonitor) Unregister(pc *PeerConnection) {
	if m == nil || pc == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, pc)
	m.mu.Unlock()
}

func (m *FactoryMonitor) run() {
	t := time.NewTicker(monitorPollInterval)
	defer t.Stop()
	var tick uint64
	var snapshot []*pcMonitorEntry
	keepaliveEvery := uint64(keepaliveTickInterval / monitorPollInterval)
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			tick++
			doKeepalive := tick%keepaliveEvery == 0
			m.mu.RLock()
			if cap(snapshot) < len(m.entries) {
				snapshot = make([]*pcMonitorEntry, 0, len(m.entries))
			}
			snapshot = snapshot[:0]
			for _, e := range m.entries {
				snapshot = append(snapshot, e)
			}
			m.mu.RUnlock()
			for _, e := range snapshot {
				e.tick(doKeepalive)
			}
		}
	}
}

// pcMonitorEntry is one PC's per-tick state. checkingNs is the monotonic
// UnixNano when the PC was first observed outside Connected; it resets
// once the PC reaches Connected. everConnected is sticky once true and
// suppresses the force-close path so pion can recover transient mid-call
// disconnects on its own (failedTimeout governs the terminal verdict).
// Atomics avoid a per-entry mutex.
type pcMonitorEntry struct {
	pc            *PeerConnection
	log           *slog.Logger
	checkingNs    atomic.Int64
	everConnected atomic.Bool
}

func (e *pcMonitorEntry) tick(doKeepalive bool) {
	state := e.pc.State()

	if state != models.Connected {
		// Once a PC has reached Connected, a later Disconnected /
		// Connecting / Failed is pion's to resolve within its own
		// failedTimeout. The watchdog's job is the leak case: a PC
		// that never connects and whose caller forgot to Close. Skip
		// after first connect so recovery isn't preempted.
		if e.everConnected.Load() {
			return
		}
		now := time.Now().UnixNano()
		if start := e.checkingNs.Load(); start == 0 {
			e.checkingNs.Store(now)
		} else if time.Duration(now-start) > iceCheckingTimeout {
			e.log.Warn("PC stuck out of Connected, forcing close",
				slog.String("state", state.String()),
				slog.Duration("timeout", iceCheckingTimeout))
			// pc.Close → monitor.Unregister, so this entry is gone
			// from the map before the next tick — no re-fire guard
			// needed.
			_ = e.pc.Close()
		}
		return
	}
	e.everConnected.Store(true)
	e.checkingNs.Store(0)

	if doKeepalive {
		if v := e.pc.VideoTrack(); v != nil {
			if err := v.GeneratePadding(1); err != nil {
				e.log.Debug("video keepalive padding", slog.Any("err", err))
			}
		}
	}
}
