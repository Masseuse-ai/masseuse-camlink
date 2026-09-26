// masseuse-camlink runs at home, next to the camera, and relays its
// encrypted stream to the one attested enclave a session names
// (docs/PROTOCOL.md).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/buildinfo"
	"github.com/FemLed/masseuse-camlink/internal/identity"
	"github.com/FemLed/masseuse-camlink/internal/rendezvous"
	"github.com/FemLed/masseuse-camlink/internal/tunnel"
)

func main() {
	var (
		service  = flag.String("service", envOr("MASSEUSE_CAMLINK_SERVICE", "https://masseuse.ai"), "the masseuse.ai service")
		stateDir = flag.String("state-dir", envOr("MASSEUSE_CAMLINK_STATE_DIR", defaultStateDir()), "where the identity key and pairings live")
		logLevel = flag.String("log-level", "info", "debug, info, warn or error")
		version  = flag.Bool("version", false, "print the version and exit")
	)
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

	id, err := identity.Load(*stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "identity:", err)
		os.Exit(1)
	}
	fmt.Printf("masseuse-camlink %s\n", buildinfo.Version())
	fmt.Printf("Identity %s… (state in %s)\n", id.PublicKeyString()[:8], *stateDir)
	if n := len(id.PairedHashes()); n > 0 {
		fmt.Printf("Paired with %d phone(s). Sessions that use your home camera will connect automatically.\n", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{}
	mgr := &manager{
		id:  id,
		log: logger,
		dialer: &tunnel.Dialer{
			Identity: id,
			Attester: &policyAttester{service: *service, client: httpClient, log: logger},
			Logger:   logger,
		},
		tunnels: map[string]*active{},
	}
	client := &rendezvous.Client{Service: *service, Identity: id, Version: buildinfo.Version(), HTTP: httpClient, Logger: logger}
	if err := client.Run(ctx, mgr); err != nil && ctx.Err() == nil {
		logger.Error("rendezvous stopped", "err", err)
		os.Exit(1)
	}
	mgr.closeAll("shutting down")
	fmt.Println("\nStopped.")
}

// policyAttester fetches the service's policy (cached briefly) and verifies
// an enclave against it.
type policyAttester struct {
	service string
	client  *http.Client
	log     *slog.Logger

	mu      sync.Mutex
	policy  *attest.Policy
	fetched time.Time
	keys    attest.KeySource
}

func (a *policyAttester) Verify(ctx context.Context, origin string) (*attest.Result, error) {
	a.mu.Lock()
	if a.policy == nil || time.Since(a.fetched) > 5*time.Minute {
		p, err := attest.FetchPolicy(ctx, a.client, a.service)
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
	attrs := []any{"origin", origin, "image", res.ImageDigest, "instance", res.InstanceID, "dbgstat", res.DbgStat}
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
	return res, nil
}

// manager holds at most one tunnel per session and reacts to rendezvous
// events.
type manager struct {
	id     *identity.Identity
	log    *slog.Logger
	dialer *tunnel.Dialer

	mu       sync.Mutex
	tunnels  map[string]*active // by session id
	lastCode string             // the code last shown, so a re-send is not printed twice
}

type active struct {
	dial   rendezvous.Dial
	cancel context.CancelFunc
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
	fmt.Printf("\nPairing code: %s\n", code)
	fmt.Println("Enter it in the masseuse.ai app: Camera > Home network camera.")
	if !expiresAt.IsZero() && expiresAt.Year() > 2000 {
		fmt.Printf("(valid until %s; a new one appears here when it expires)\n\n", expiresAt.Local().Format("15:04"))
	}
}

func (m *manager) OnPaired(hash string) {
	if err := m.id.AddPaired(hash); err != nil {
		m.log.Error("could not save pairing", "err", err)
		return
	}
	fmt.Println("Paired with a phone. Sessions that use your home camera will connect automatically.")
}

func (m *manager) OnOnline(online bool) {
	if online {
		m.log.Info("connected to the service")
	} else {
		m.log.Warn("disconnected from the service; reconnecting")
	}
}

func (m *manager) OnDial(d rendezvous.Dial) {
	m.mu.Lock()
	if cur, ok := m.tunnels[d.SessionID]; ok {
		if cur.dial.Origin == d.Origin && cur.dial.TicketHash == d.TicketHash {
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
	m.tunnels[d.SessionID] = &active{dial: d, cancel: cancel}
	m.mu.Unlock()
	go m.run(ctx, d)
}

func (m *manager) OnClear(sessionID, reason string) {
	m.mu.Lock()
	cur, ok := m.tunnels[sessionID]
	if ok {
		delete(m.tunnels, sessionID)
	}
	m.mu.Unlock()
	if ok {
		cur.cancel()
		m.log.Info("session ended", "session", sessionID, "reason", reason)
		fmt.Println("Camera link closed.")
	}
}

func (m *manager) closeAll(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, a := range m.tunnels {
		a.cancel()
		delete(m.tunnels, id)
	}
	_ = reason
}

// run keeps a tunnel up for the session until it is cleared, re-dialing
// after failures with backoff.
func (m *manager) run(ctx context.Context, d rendezvous.Dial) {
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
		fmt.Println("Camera link active: relaying to the verified enclave.")
		err = t.Serve(ctx)
		if ctx.Err() != nil {
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
