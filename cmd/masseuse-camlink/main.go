// masseuse-camlink runs on a computer at home and sends its camera and
// microphone, or a camera on the home network, to the one attested enclave
// a session names (docs/PROTOCOL.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/buildinfo"
	"github.com/FemLed/masseuse-camlink/internal/capture"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/mux"
	"github.com/FemLed/masseuse-camlink/internal/oci"
	"github.com/FemLed/masseuse-camlink/internal/provenance"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
	"github.com/FemLed/masseuse-camlink/internal/serve"
	"github.com/FemLed/masseuse-camlink/internal/tunnel"
)

func main() {
	var (
		service   = flag.String("service", envOr("MASSEUSE_CAMLINK_SERVICE", "https://masseuse.ai"), "the masseuse.ai service")
		stateDir  = flag.String("state-dir", envOr("MASSEUSE_CAMLINK_STATE_DIR", defaultStateDir()), "where the identity key, pairings and camera choice live")
		logLevel  = flag.String("log-level", "info", "debug, info, warn or error")
		version   = flag.Bool("version", false, "print the version and exit")
		estimPort = flag.String("estim-port", envOr("MASSEUSE_CAMLINK_ESTIM_PORT", ""), "the serial port of the stimulation device, if the scan picks the wrong one (default: scan the USB serial adapters)")
		sf        sourceFlags
	)
	flag.StringVar(&sf.camera, "camera", "", "the computer's camera to send: its number in the devices listing, or (part of) its name; default the first")
	flag.StringVar(&sf.mic, "mic", "", "the microphone to send with it: number or name; none for video only; default the first")
	flag.StringVar(&sf.videoSize, "video-size", "", "capture size WxH (default "+capture.Defaults.VideoSize+")")
	flag.IntVar(&sf.fps, "fps", 0, fmt.Sprintf("capture rate (default %d)", capture.Defaults.FPS))
	flag.StringVar(&sf.bitrate, "bitrate", "", "video bit rate (default "+capture.Defaults.Bitrate+")")
	flag.StringVar(&sf.encoder, "encoder", "", "auto, h264_videotoolbox, h264_mf or libx264 (default auto: the hardware encoder, then libx264)")
	flag.StringVar(&sf.ffmpeg, "ffmpeg", "", "the ffmpeg executable (default: found on PATH)")
	flag.StringVar(&sf.cameraURL, "camera-url", "", "send a camera on your network instead: rtsps://user:password@host:port/path")
	flag.StringVar(&sf.cameraFingerprint, "camera-fingerprint", "", "that camera's certificate SHA-256, if you have it; otherwise it is trusted on first use")
	flag.Usage = usage
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.Version(), buildinfo.GoVersion())
		return
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "bad -log-level:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if !strings.HasPrefix(*service, "https://") {
		fmt.Fprintln(os.Stderr, "-service must be an https:// URL")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch flag.Arg(0) {
	case "":
	case "devices":
		os.Exit(listDevices(ctx, sf.ffmpeg))
	case "estim":
		if flag.Arg(1) != "probe" {
			fmt.Fprintf(os.Stderr, "unknown estim command %q (the one command is: estim probe)\n", flag.Arg(1))
			os.Exit(2)
		}
		os.Exit(probeEstim(ctx, *stateDir, *estimPort, logger))
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (the commands are: devices, estim probe)\n", flag.Arg(0))
		os.Exit(2)
	}

	id, err := identity.Load(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "identity:", err)
		os.Exit(1)
	}
	fmt.Printf("masseuse-camlink %s\n", buildinfo.Version())
	fmt.Printf("Identity %s… (state in %s)\n", id.PublicKeyString()[:8], *stateDir)

	// The connector's own stream and the camera behind it.
	sink, err := serve.New(serve.Config{StateDir: *stateDir, Logger: logger})
	if err != nil {
		fmt.Fprintln(os.Stderr, "camera stream:", err)
		os.Exit(1)
	}
	defer sink.Close()
	cfg, err := resolveSourceConfig(*stateDir, sf)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	off, err := buildOffer(ctx, cfg, *stateDir, sink, logger, !sf.any())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, capture.ErrNoDevice) {
			fmt.Fprintln(os.Stderr, "Run `masseuse-camlink devices` to see what is connected.")
		}
		os.Exit(2)
	}
	off.describe()
	if sf.any() {
		if err := saveSourceConfig(*stateDir, off.save); err != nil {
			logger.Warn("could not remember the camera choice", "err", err)
		}
	}
	cam := &camControl{sink: sink, offer: off, log: logger}
	if n := len(id.PairedHashes()); n > 0 {
		fmt.Printf("Paired with %d phone(s). Sessions that use this camera connect automatically.\n", n)
	}

	httpClient := &http.Client{}
	mgr := &manager{
		id:    id,
		log:   logger,
		cam:   cam,
		estim: newEstimLink(*stateDir, *estimPort, logger, func(format string, args ...any) { fmt.Printf(format, args...) }),
		dialer: &tunnel.Dialer{
			Identity: id,
			Attester: &policyAttester{
				service: *service,
				client:  httpClient,
				log:     logger,
				floors:  attest.Production,
				provenance: &provenance.Verifier{
					Registry: &oci.Client{HTTP: &http.Client{Timeout: 30 * time.Second}},
					CacheDir: *stateDir,
					Logger:   logger,
				},
			},
			Local:  map[string]func() (net.Conn, error){serve.Target: cam.dialLocal},
			Logger: logger,
		},
		tunnels: map[string]*active{},
	}
	client := &rendezvous.Client{Service: *service, Identity: id, Version: buildinfo.Version(), HTTP: httpClient, Logger: logger}
	mgr.estim.client = client
	// The device link runs beside the rendezvous client and outlives its
	// context slightly: on the way out it releases the device and tells the
	// service.
	estimCtx, stopEstim := context.WithCancel(context.Background())
	estimDone := make(chan struct{})
	go func() { defer close(estimDone); mgr.estim.run(estimCtx) }()
	err = client.Run(ctx, mgr)
	mgr.closeAll("shutting down")
	cam.off(false)
	stopEstim()
	<-estimDone
	if err != nil && ctx.Err() == nil {
		logger.Error("rendezvous stopped", "err", err)
		os.Exit(1)
	}
	fmt.Println("\nStopped.")
}

func usage() {
	w := flag.CommandLine.Output()
	fmt.Fprintf(w, `masseuse-camlink sends this computer's camera and microphone, or a camera
on your network, to the enclave of a masseuse.ai session.

  masseuse-camlink                    run with the remembered (or first) camera and microphone
  masseuse-camlink devices            list cameras and microphones
  masseuse-camlink estim probe        find the stimulation device on USB serial and print its status
  masseuse-camlink -camera 1 -mic 0   choose by number or by (part of) the name; remembered
  masseuse-camlink -camera-url rtsps://user:password@192.168.1.20:322/live
                                      send a camera on your network instead

Flags:
`)
	flag.PrintDefaults()
}

// policyAttester fetches the service's policy (cached briefly), tightens it
// to the floors this build carries, verifies an enclave against it, and
// then checks the attested image's provenance against the public registry
// and the Sigstore log.
type policyAttester struct {
	service string
	client  *http.Client
	log     *slog.Logger
	// floors is what the served policy may only tighten (internal/attest,
	// Floors); the served policy is refused when it contradicts them.
	floors attest.Floors
	// provenance ties the attested digest to the release workflow's logged
	// signature and the builder's SLSA provenance (internal/provenance).
	// nil skips the check; the connector never leaves it nil.
	provenance *provenance.Verifier

	mu      sync.Mutex
	policy  *attest.Policy
	fetched time.Time
	keys    attest.KeySource
}

func (a *policyAttester) Verify(ctx context.Context, origin string) (*attest.Result, error) {
	a.mu.Lock()
	if a.policy == nil || time.Since(a.fetched) > 5*time.Minute {
		p, err := a.fetchPolicy(ctx)
		if err != nil {
			a.mu.Unlock()
			return nil, err
		}
		if a.policy == nil || a.policy.JWKSURL != p.JWKSURL {
			a.keys = attest.NewRemoteJWKS(p.JWKSURL, a.client)
		}
		a.policy, a.fetched = p, time.Now()
	}
	v := &attest.Verifier{Policy: a.policy, Keys: a.keys}
	a.mu.Unlock()
	res, err := v.Verify(ctx, origin)
	if err != nil {
		return nil, err
	}
	attrs := []any{"origin", origin, "image", res.ImageDigest, "signer", res.SignerKeyID, "instance", res.InstanceID, "dbgstat", res.DbgStat}
	if res.Release != nil {
		// The release tag and source commit the image was built from, read
		// off the attested container environment.
		attrs = append(attrs, "release", res.Release.Version, "commit", res.Release.Commit)
	}
	a.log.Info("enclave verified", attrs...)
	// Where the attested image was built from, and the one command that
	// checks it (VERIFY.md, "The enclave your camera streams to").
	if res.Source != nil {
		a.log.Info("enclave source", "image", res.ImageDigest, "source", res.Source.String(), "registry", res.Source.Repo, "verify", res.Source.VerifyCommand(res.ImageDigest))
	} else {
		a.log.Warn("enclave source unpublished", "image", res.ImageDigest, "note", "the policy names no source repository for its images; the attestation holds but the source cannot be checked")
	}
	if a.provenance != nil {
		if err := a.checkProvenance(ctx, res); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// checkProvenance runs the provenance check the attestation makes
// possible: the digest the launcher attested must carry, in the public
// registry, a logged signature by the enclave repository's release
// workflow at the release the image is stamped with, and logged SLSA
// provenance for that release and commit. Without a stamp or a source
// there is nothing to check against, and the enclave is refused.
func (a *policyAttester) checkProvenance(ctx context.Context, res *attest.Result) error {
	if res.Release == nil {
		return fmt.Errorf("enclave provenance: image %s carries no release stamp to check against", res.ImageDigest)
	}
	if res.Source == nil {
		return fmt.Errorf("enclave provenance: the policy names no source repository for image %s", res.ImageDigest)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	p, err := a.provenance.Verify(ctx, provenance.Expect{
		Digest:    res.ImageDigest,
		Release:   res.Release.Version,
		Commit:    res.Release.Commit,
		Repo:      res.Source.Repo,
		SourceURI: res.Source.SourceURI,
	})
	if err != nil {
		return fmt.Errorf("enclave provenance: %w", err)
	}
	a.log.Info("enclave provenance",
		"image", p.Digest, "release", p.Release, "commit", p.Commit,
		"signed_by", p.SignatureIdentity, "signature_log_index", p.SignatureLogIndex,
		"builder", p.Builder, "provenance_log_index", p.ProvenanceLogIndex,
		"cached", p.Cached)
	if !p.Cached {
		fmt.Printf("Enclave image %s… is %s %s (commit %.7s): signature and build provenance verified in the public registry and the Sigstore log.\n",
			strings.TrimPrefix(p.Digest, "sha256:")[:12], p.SourceURI, p.Release, p.Commit)
	}
	return nil
}

// fetchPolicy downloads the served policy with its own deadline and applies
// the floors: the result is at least as strict as both.
func (a *policyAttester) fetchPolicy(ctx context.Context) (*attest.Policy, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p, err := attest.FetchPolicy(ctx, a.client, a.service)
	if err != nil {
		return nil, err
	}
	if err := a.floors.Apply(p); err != nil {
		return nil, fmt.Errorf("%s/api/tee-policy: %w", a.service, err)
	}
	a.log.Debug("enclave policy", "signers", p.ImageSignatures, "minRelease", p.MinRelease, "hosts", p.TeeSlotHostSuffixes, "project", p.ProjectID, "imageRef", p.ImageReferencePrefix, "source", p.SourceURI)
	return p, nil
}

// manager holds at most one tunnel per session and reacts to rendezvous
// events.
type manager struct {
	id     *identity.Identity
	log    *slog.Logger
	dialer *tunnel.Dialer
	cam    *camControl
	// estim serves the stimulation device, if one is plugged in.
	estim *estimLink
	// out is the console; nil means standard output.
	out io.Writer

	mu       sync.Mutex
	tunnels  map[string]*active // by session id
	lastCode string             // the code last shown, so a re-send is not printed twice
}

func (m *manager) printf(format string, args ...any) {
	w := m.out
	if w == nil {
		w = os.Stdout
	}
	fmt.Fprintf(w, format, args...)
}

// CurrentSource tells the service which camera this connector offers.
func (m *manager) CurrentSource() (rendezvous.Source, bool) {
	if m.cam == nil || m.cam.offer == nil {
		return rendezvous.Source{}, false
	}
	return m.cam.offer.source(), true
}

type active struct {
	dial   rendezvous.Dial
	cancel context.CancelFunc
	// done is closed when run has given up on this dial (the enclave
	// refused the ticket or closed the link); a fresh dial then replaces it.
	done chan struct{}
	// ended is set before cancel when the session is over (cleared, or the
	// connector shutting down) rather than the tunnel being replaced: the
	// camera then goes off at once instead of waiting for the next tunnel.
	ended atomic.Bool
}

func (a *active) finished() bool {
	select {
	case <-a.done:
		return true
	default:
		return false
	}
}

// OnCode shows a pairing code. The service sends the current code with
// hello and again at the head of every event stream (so a reconnect shows
// it), which is the same code twice on a normal start: print it once.
func (m *manager) OnCode(code string, expiresAt time.Time) {
	m.mu.Lock()
	same := code == m.lastCode
	m.lastCode = code
	m.mu.Unlock()
	if same {
		return
	}
	m.printf("\nPairing code: %s\n", code)
	m.printf("Enter it in the masseuse.ai app: Camera > Computer or home camera.\n")
	if !expiresAt.IsZero() && expiresAt.Year() > 2000 {
		m.printf("(valid until %s; a new one appears here when it expires)\n\n", expiresAt.Local().Format("15:04"))
	}
}

func (m *manager) OnPaired(hash string) {
	if err := m.id.AddPaired(hash); err != nil {
		m.log.Error("could not save pairing", "err", err)
		return
	}
	m.printf("Paired with a phone. Sessions that use this camera connect automatically.\n")
}

func (m *manager) OnOnline(online bool) {
	if online {
		m.log.Info("connected to the service")
		if m.estim != nil {
			m.estim.reportDevice()
		}
	} else {
		m.log.Warn("disconnected from the service; reconnecting")
	}
}

// OnEstim hands a device link message to the device session.
func (m *manager) OnEstim(sessionID string, message json.RawMessage) {
	if m.estim != nil {
		m.estim.OnEstim(sessionID, message)
	}
}

func (m *manager) OnDial(d rendezvous.Dial) {
	m.mu.Lock()
	if cur, ok := m.tunnels[d.SessionID]; ok {
		if cur.dial.Origin == d.Origin && cur.dial.TicketHash == d.TicketHash && !cur.finished() {
			m.mu.Unlock()
			return // idempotent
		}
		cur.cancel()
	}
	// One session at a time: a new session replaces any other.
	for id, other := range m.tunnels {
		if id != d.SessionID {
			other.cancel()
			delete(m.tunnels, id)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &active{dial: d, cancel: cancel, done: make(chan struct{})}
	m.tunnels[d.SessionID] = a
	m.mu.Unlock()
	go m.run(ctx, a)
}

func (m *manager) OnClear(sessionID, reason string) {
	m.mu.Lock()
	cur, ok := m.tunnels[sessionID]
	if ok {
		delete(m.tunnels, sessionID)
	}
	m.mu.Unlock()
	if ok {
		cur.ended.Store(true)
		cur.cancel()
		m.log.Info("session ended", "session", sessionID, "reason", reason)
		m.printf("Camera link closed.\n")
	}
	if m.estim != nil {
		m.estim.sessionCleared(sessionID)
	}
}

func (m *manager) closeAll(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, a := range m.tunnels {
		a.ended.Store(true)
		a.cancel()
		delete(m.tunnels, id)
	}
	_ = reason
}

// run keeps a tunnel up for the session until it is cleared, re-dialing
// after network failures with backoff. The camera, if the session turned
// it on, stays on for a while when the tunnel goes down (cameraGrace), so
// that the next tunnel's first stream finds it publishing, and goes off
// when that grace passes with no stream, or at once when the session ends.
//
// Two endings are final for this ticket and are not retried: the enclave
// answering that it is not expecting the connector (the ticket is spent or
// forgotten, as after the phone removed the camera), and the enclave
// closing the link itself. Either way the service sends a new dial when a
// session wants the camera again; the session entry stays so that a clear
// is still reported and the same ticket is not dialed twice.
func (m *manager) run(ctx context.Context, a *active) {
	d := a.dial
	defer close(a.done)
	defer func() { m.cam.off(!a.ended.Load()) }()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		t, err := m.dialer.Dial(ctx, d.Origin, d.Ticket, d.TicketHash)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var se *tunnel.StatusError
			if errors.As(err, &se) && se.Refused() {
				m.log.Warn("the enclave is not expecting this connector; waiting for the service", "status", se.Status, "session", d.SessionID)
				m.printf("Camera link on hold: the enclave is not expecting this connector; waiting for the service.\n")
				return
			}
			m.log.Warn("could not reach the enclave", "err", err, "retryIn", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		m.printf("Camera link active: connected to the verified enclave.\n")
		m.cam.attach(t.Backlog)
		err = t.Serve(ctx)
		m.cam.attach(nil)
		m.cam.off(!a.ended.Load())
		if ctx.Err() != nil {
			return
		}
		var re *mux.ResetError
		if errors.As(err, &re) {
			m.log.Info("the enclave closed the camera link; waiting for the service", "reason", re.Reason, "session", d.SessionID)
			m.printf("The enclave closed the camera link (%s); waiting for the service.\n", re.Reason)
			return
		}
		m.log.Warn("tunnel ended; reconnecting", "err", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func defaultStateDir() string {
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "masseuse-camlink")
		}
	case "windows":
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, "masseuse-camlink")
		}
	default:
		if v := os.Getenv("XDG_STATE_HOME"); v != "" {
			return filepath.Join(v, "masseuse-camlink")
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".local", "state", "masseuse-camlink")
		}
	}
	return "masseuse-camlink-state"
}
