package native

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
	dtlsnet "github.com/pion/dtls/v3/pkg/net"
	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
	"github.com/pion/stun/v3"

	"github.com/annihilatorrrr/gotgcall/models"
)

// ConnStateFn is invoked when the stack's high-level connection state
// transitions. Replaces pion/webrtc's OnConnectionStateChange.
type ConnStateFn func(models.ConnState)

// Stack drives one Telegram group-call connection end-to-end. It owns
// the ICE agent (role CONTROLLED — pion's Accept path), the DTLS server
// connection (role SSL_SERVER), the SRTP encryption context (send-only),
// and the audio + video Tracks that feed RTP packets through.
//
// Lifecycle:
//
//  1. NewStack — generates per-call ufrag/pwd, takes a cert from the
//     factory's pool, instantiates the ICE agent.
//  2. LocalParams — returns the JoinGroupCall blob (no SDP detour).
//  3. Connect(remoteJSON) — adds Telegram's candidates, runs ICE Accept
//     synchronously, then DTLS handshake, then derives SRTP keys, then
//     starts a single drain goroutine on the ICE conn.
//  4. AudioTrack / VideoTrack expose Tracks the caller pipes media into.
//  5. Close — tears down SRTP, DTLS, ICE in reverse order; cancels the
//     drain goroutine.
type Stack struct {
	drainCtx context.Context
	log      *slog.Logger

	cert *tls.Certificate

	audio *Track
	video *Track

	agent    *ice.Agent
	iceConn  *ice.Conn
	dtlsConn *dtls.Conn
	srtpCtx  *srtp.Context

	onStateChange ConnStateFn
	drainCancel   context.CancelFunc
	drainDone     chan struct{}

	ufrag string
	pwd   string

	// reusable per-packet encryption buffer; resized as needed and
	// shared between audio + video writes under srtpWriteMu.
	encryptBuf []byte

	// connectDelay matches the factory option of the same name — a short
	// sleep between the JoinGroupCall response landing and ICE Accept,
	// giving Telegram's SFU time to register our ufrag/pwd so the first
	// inbound STUN binding from us isn't rejected as auth-fail.
	connectDelay time.Duration

	onStateChangeMu sync.RWMutex

	closeOnce sync.Once

	// srtpWriteMu serialises raw writes onto iceConn so concurrent audio
	// and video send paths do not interleave encrypted SRTP bytes within
	// a UDP datagram.
	srtpWriteMu sync.Mutex

	audioSSRC uint32
	videoSSRC uint32

	state atomic.Int32

	closed atomic.Bool
}

// Options collects construction-time inputs for a Stack. NetworkTypes,
// ICEServers, and the cert pool are factory-shared; the per-call SSRCs
// and connect-delay are caller-specified.
type Options struct {
	UDPMux          ice.UDPMux
	Logger          *slog.Logger
	Cert            *tls.Certificate
	InterfaceFilter func(name string) bool
	IPFilter        func(ip net.IP) bool
	NetworkTypes    []ice.NetworkType
	ICEServers      []*stun.URI
	ConnectDelay    time.Duration

	AudioSSRC uint32
	VideoSSRC uint32
}

// NewStack creates a Stack ready to emit a JoinGroupCall blob. The ICE
// agent starts gathering host candidates eagerly so they are populated
// by the time Connect runs.
func NewStack(opts Options) (*Stack, error) {
	if opts.Cert == nil {
		return nil, fmt.Errorf("native: cert is required")
	}
	if opts.AudioSSRC == 0 {
		return nil, fmt.Errorf("native: audioSSRC is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	var agentOpts []ice.AgentOption

	if len(opts.NetworkTypes) > 0 {
		agentOpts = append(agentOpts, ice.WithNetworkTypes(opts.NetworkTypes))
	} else {
		agentOpts = append(agentOpts, ice.WithNetworkTypes([]ice.NetworkType{
			ice.NetworkTypeUDP4, ice.NetworkTypeUDP6,
		}))
	}
	if len(opts.ICEServers) > 0 {
		agentOpts = append(agentOpts, ice.WithUrls(opts.ICEServers))
	}
	if opts.UDPMux != nil {
		agentOpts = append(agentOpts, ice.WithUDPMux(opts.UDPMux))
	}
	if opts.InterfaceFilter != nil {
		agentOpts = append(agentOpts, ice.WithInterfaceFilter(opts.InterfaceFilter))
	}
	if opts.IPFilter != nil {
		agentOpts = append(agentOpts, ice.WithIPFilter(opts.IPFilter))
	}

	agent, err := ice.NewAgentWithOptions(agentOpts...)
	if err != nil {
		return nil, fmt.Errorf("native: new agent: %w", err)
	}
	// Pion auto-generates ufrag/pwd inside NewAgent; read them back so we
	// can emit them in LocalParams. There's no way to inject our own
	// credentials short of an ICE restart, which isn't worth the
	// complexity for our case.
	ufrag, pwd, err := agent.GetLocalUserCredentials()
	if err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("native: get local creds: %w", err)
	}

	audioTrack := NewTrack(KindAudio, opts.AudioSSRC)
	var videoTrack *Track
	if opts.VideoSSRC != 0 {
		videoTrack = NewTrack(KindVideo, opts.VideoSSRC)
	}

	s := &Stack{
		log:          log.With(slog.String("comp", "native")),
		cert:         opts.Cert,
		ufrag:        ufrag,
		pwd:          pwd,
		audioSSRC:    opts.AudioSSRC,
		videoSSRC:    opts.VideoSSRC,
		audio:        audioTrack,
		video:        videoTrack,
		agent:        agent,
		connectDelay: opts.ConnectDelay,
		drainDone:    make(chan struct{}),
		encryptBuf:   make([]byte, 0, 1500),
	}
	s.state.Store(int32(models.Connecting))

	if err = agent.OnConnectionStateChange(s.onAgentState); err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("native: hook agent state: %w", err)
	}
	if err = agent.OnCandidate(func(c ice.Candidate) {
		if c == nil {
			s.log.Debug("ICE gather complete")
			return
		}
		s.log.Debug("ICE candidate gathered",
			slog.String("typ", c.Type().String()),
			slog.String("network", c.NetworkType().String()),
			slog.String("addr", c.Address()),
			slog.Int("port", c.Port()))
	}); err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("native: hook agent candidate: %w", err)
	}
	if err = agent.GatherCandidates(); err != nil {
		_ = agent.Close()
		return nil, fmt.Errorf("native: gather: %w", err)
	}
	return s, nil
}

// LocalParams returns the JoinGroupCall blob carrying our ufrag/pwd,
// DTLS fingerprint, SSRCs, codecs, and header extensions. No
// SetLocalDescription-style state transition exists here — Telegram
// learns our candidates peer-reflexively from the first STUN binding
// we emit during Accept.
func (s *Stack) LocalParams() (string, error) {
	if s.closed.Load() {
		return "", models.ErrClosed
	}
	fp, err := CertFingerprintSHA256(s.cert)
	if err != nil {
		return "", fmt.Errorf("native: fingerprint: %w", err)
	}
	js, err := buildLocalParamsJSON(s.ufrag, s.pwd, fp, s.audioSSRC, s.videoSSRC)
	if err != nil {
		return "", err
	}
	s.log.Debug("LocalParams emitted",
		slog.String("ufrag", s.ufrag),
		slog.Uint64("audioSSRC", uint64(s.audioSSRC)),
		slog.Uint64("videoSSRC", uint64(s.videoSSRC)))
	return js, nil
}

// Connect parses Telegram's response, registers remote candidates, runs
// ICE-Accept (we are the CONTROLLED peer), performs the DTLS handshake
// as SERVER, derives SRTP keys, and starts the inbound-drain goroutine.
// The audio and video tracks become live the moment SRTP is wired in.
func (s *Stack) Connect(ctx context.Context, remoteJSON string) error {
	if s.closed.Load() {
		return models.ErrClosed
	}
	rp, err := parseRemoteJSON(remoteJSON)
	if err != nil {
		return fmt.Errorf("%w: %v", models.ErrInvalidParams, err)
	}

	for i, c := range rp.candidates {
		cand, addErr := buildICECandidate(c)
		if addErr != nil {
			s.log.Warn("skip malformed remote candidate", slog.Int("i", i), slog.Any("err", addErr))
			continue
		}
		if addErr = s.agent.AddRemoteCandidate(cand); addErr != nil {
			s.log.Warn("AddRemoteCandidate failed", slog.Int("i", i), slog.Any("err", addErr))
		}
	}

	if s.connectDelay > 0 {
		s.log.Debug("Connect: pre-Accept delay", slog.Duration("delay", s.connectDelay))
		select {
		case <-time.After(s.connectDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Accept = ICE-CONTROLLED. Pion sends checks outbound from our host
	// candidates; Telegram learns our post-NAT source peer-reflexively
	// and nominates the pair. Blocks until Connected (or failure).
	iceConn, err := s.agent.Accept(ctx, rp.ufrag, rp.pwd)
	if err != nil {
		return fmt.Errorf("%w: ice accept: %v", models.ErrConnectionFailed, err)
	}
	s.iceConn = iceConn

	// DTLS handshake. We are the SERVER (setup=passive in the JoinGroupCall
	// payload). Telegram's SFU is DTLS-active and sends ClientHello.
	packetConn := dtlsnet.PacketConnFromConn(iceConn)
	dtlsConn, err := dtls.ServerWithOptions(
		packetConn,
		iceConn.RemoteAddr(),
		dtls.WithCertificates(*s.cert),
		dtls.WithSRTPProtectionProfiles(
			dtls.SRTP_AEAD_AES_128_GCM,
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
		),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		// Telegram's fingerprint validation rides on the JoinGroupCall blob
		// the caller already trusts (MTProto-authenticated). Cert-chain
		// validation would fail against self-signed peers anyway.
		dtls.WithInsecureSkipVerify(true),
	)
	if err != nil {
		return fmt.Errorf("%w: dtls server: %v", models.ErrConnectionFailed, err)
	}
	s.dtlsConn = dtlsConn

	hsCtx, hsCancel := context.WithTimeout(ctx, 10*time.Second)
	if err = dtlsConn.HandshakeContext(hsCtx); err != nil {
		hsCancel()
		return fmt.Errorf("%w: dtls handshake: %v", models.ErrConnectionFailed, err)
	}
	hsCancel()

	srtpCtx, _, err := deriveSRTPContext(dtlsConn)
	if err != nil {
		return fmt.Errorf("%w: srtp keys: %v", models.ErrConnectionFailed, err)
	}
	s.srtpCtx = srtpCtx

	// Wire the SRTP write path into both tracks. We pass a shim that
	// adapts pion/srtp's Context (encrypt-only) to the WriteStreamSRTP
	// interface our Tracks expect.
	writeStream := &srtpWriter{stack: s}
	s.audio.AttachWriteStream(writeStream)
	if s.video != nil {
		s.video.AttachWriteStream(writeStream)
	}

	// Single drain goroutine: prevents the ICE Agent's internal receive
	// buffer from filling with packets we'll never decrypt (we are
	// send-only). Cheap — one Read loop on a hot socket.
	s.drainCtx, s.drainCancel = context.WithCancel(context.Background())
	go s.drainInbound()

	s.transitionTo(models.Connected)
	return nil
}

// AudioTrack returns the audio Track (Opus PT=111). Caller writes
// media.Sample frames via Track.WriteSample.
func (s *Stack) AudioTrack() *Track { return s.audio }

// VideoTrack returns the video Track (VP8 PT=100), or nil for audio-only calls.
func (s *Stack) VideoTrack() *Track { return s.video }

// AudioSSRC returns the audio synchronization source. Callers use this
// when sending the JoinGroupCall MTProto request — Telegram requires the
// participant SSRC to match the one in our LocalParams blob.
func (s *Stack) AudioSSRC() uint32 { return s.audioSSRC }

// VideoSSRC returns the video synchronization source.
func (s *Stack) VideoSSRC() uint32 { return s.videoSSRC }

// OnConnectionStateChange registers fn for high-level state transitions.
// The callback fires synchronously from inside the stack goroutine that
// drove the transition (the ice.Agent's, or this stack's Close).
func (s *Stack) OnConnectionStateChange(fn ConnStateFn) {
	s.onStateChangeMu.Lock()
	s.onStateChange = fn
	s.onStateChangeMu.Unlock()
}

// State returns the current high-level connection state.
func (s *Stack) State() models.ConnState {
	return models.ConnState(s.state.Load())
}

// Close tears the stack down idempotently. Drain goroutine exits when
// the iceConn closes from underneath it.
func (s *Stack) Close() error {
	var firstErr error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.drainCancel != nil {
			s.drainCancel()
		}
		if s.dtlsConn != nil {
			if err := s.dtlsConn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if s.iceConn != nil {
			if err := s.iceConn.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if s.agent != nil {
			if err := s.agent.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		s.transitionTo(models.Closed)
	})
	return firstErr
}

// drainInbound is a single per-stack goroutine that reads + discards
// every byte coming up from the ICE conn after DTLS handshake. Without
// this the ice.Agent's internal receive buffer fills with SRTCP /
// re-handshake / rekey traffic we have no use for and back-pressure
// halts the agent's UDP read loop.
func (s *Stack) drainInbound() {
	defer close(s.drainDone)
	buf := make([]byte, 1500)
	for {
		select {
		case <-s.drainCtx.Done():
			return
		default:
		}
		if s.iceConn == nil {
			return
		}
		if err := s.iceConn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			return
		}
		_, err := s.iceConn.Read(buf)
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			continue
		}
		// Anything else (Close, EOF, agent-shutdown) is terminal.
		return
	}
}

func (s *Stack) onAgentState(state ice.ConnectionState) {
	s.log.Debug("ICE agent state", slog.String("state", state.String()))
	switch state {
	case ice.ConnectionStateConnected, ice.ConnectionStateCompleted:
		// Connected at ICE level is necessary but not sufficient — we
		// transition to models.Connected only once DTLS handshake +
		// SRTP key derivation succeed (inside Connect). Suppress here.
	case ice.ConnectionStateDisconnected:
		s.transitionTo(models.Disconnected)
	case ice.ConnectionStateFailed:
		s.transitionTo(models.Failed)
	case ice.ConnectionStateClosed:
		s.transitionTo(models.Closed)
	}
}

func (s *Stack) transitionTo(next models.ConnState) {
	prev := models.ConnState(s.state.Swap(int32(next)))
	if prev == next {
		return
	}
	s.onStateChangeMu.RLock()
	fn := s.onStateChange
	s.onStateChangeMu.RUnlock()
	if fn != nil {
		fn(next)
	}
}

// srtpWriter adapts the encrypt-only srtp.Context to the
// srtp.WriteStreamSRTP interface our Tracks consume. The session-level
// type pulls in a read loop + goroutine we have no use for; this shim
// stays synchronous and shares the encryptBuf scratch across packets.
type srtpWriter struct {
	stack *Stack
}

// WriteRTP marshals a single RTP packet, encrypts it via the SRTP
// context, and writes the ciphertext to the underlying ICE conn.
//
// Concurrency: Tracks may call WriteRTP from independent goroutines
// (audio streamer + video keepalive); srtpWriteMu serialises both the
// scratch-buffer reuse and the iceConn.Write call so a single UDP
// datagram is never partially-interleaved.
func (w *srtpWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	s := w.stack
	if s.iceConn == nil || s.srtpCtx == nil {
		return 0, fmt.Errorf("srtp not ready")
	}

	pkt := &rtp.Packet{Header: *header, Payload: payload}
	plaintext, err := pkt.Marshal()
	if err != nil {
		return 0, err
	}

	s.srtpWriteMu.Lock()
	cipher, err := s.srtpCtx.EncryptRTP(s.encryptBuf[:0], plaintext, header)
	if err != nil {
		s.srtpWriteMu.Unlock()
		return 0, err
	}
	s.encryptBuf = cipher
	n, err := s.iceConn.Write(cipher)
	s.srtpWriteMu.Unlock()
	return n, err
}
