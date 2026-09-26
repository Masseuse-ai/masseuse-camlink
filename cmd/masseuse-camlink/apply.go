package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/share"
)

// The sources at run time. At startup main.go builds the offers from the
// flags and the remembered choice; the desktop window may change them
// while the program runs (set_source, ipc.go), which comes here: the
// choice is merged into the configuration in force the way flags are
// (each part replaced only when spoken of), the offers rebuilt for the
// parts that changed, the phone's picture started or stopped, the result
// remembered in source.json and reported to the service, then said.

// pendingSource is a set_source that waits for the camera to be let go.
type pendingSource struct {
	cfg   sourceConfig
	flags sourceFlags
}

// applySource is the window's set_source. The camera cannot change while
// a session reads it, nor the front-facing camera while the enclave shows
// it: such a change waits, and is applied once the camera goes off
// (camControl.onOff, applyPending).
func (m *manager) applySource(ctx context.Context, choice sourceChoice) error {
	flags := choice.flags()
	if !flags.any() {
		return errors.New("the choice names nothing to change")
	}
	m.srcMu.Lock()
	defer m.srcMu.Unlock()
	cfg, err := mergeSourceConfig(m.cfg, flags)
	if err != nil {
		return err
	}
	cameraOn, faceOn := m.cam.busy()
	if (flags.bodyAny() && cameraOn) || (flags.faceAny() && faceOn) {
		m.pending = &pendingSource{cfg: cfg, flags: flags}
		m.reporter().Notice(noticeWarn, "A session is using the camera; the change is applied when it lets go.")
		return nil
	}
	m.pending = nil
	return m.applyLocked(ctx, cfg, flags)
}

// applyPending applies the change that waited, now that the camera is off.
func (m *manager) applyPending() {
	m.srcMu.Lock()
	defer m.srcMu.Unlock()
	p := m.pending
	if p == nil {
		return
	}
	if cameraOn, faceOn := m.cam.busy(); cameraOn || faceOn {
		return
	}
	m.pending = nil
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.applyLocked(ctx, p.cfg, p.flags); err != nil {
		m.reporter().Notice(noticeError, firstLine(err))
	}
}

// applyLocked rebuilds what the flags speak of; the caller holds srcMu.
func (m *manager) applyLocked(ctx context.Context, cfg sourceConfig, flags sourceFlags) error {
	body, face := m.cam.current()
	if flags.bodyAny() {
		o, err := buildOffer(ctx, cfg, m.stateDir, m.sink, m.log, m.reporter(), false)
		if err != nil {
			return err
		}
		if err := m.cam.setOffer(o); err != nil {
			return err
		}
		body = o
	}
	if flags.faceAny() {
		o, err := buildFaceOffer(ctx, cfg, m.sink.Face(), m.sink.Face().Stats, m.log, m.reporter(), false)
		if err != nil {
			return err
		}
		if err := m.cam.setFace(o); err != nil {
			return err
		}
		face = o
	}
	if flags.shareAny() {
		if err := m.setShareLocked(&cfg); err != nil {
			return err
		}
	}
	m.cfg = rememberedConfig(cfg, body, face)
	if err := saveSourceConfig(m.stateDir, m.cfg); err != nil {
		m.log.Warn("could not remember the camera choice", "err", err)
	}
	if m.client != nil {
		if src, ok := m.currentSourceLocked(); ok {
			rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			if err := m.client.ReportSource(rctx, src); err != nil {
				m.log.Warn("rendezvous: source report failed", "err", err)
			}
			cancel()
		}
	}
	m.reporter().Source(sourceReportOf(body, face, m.shared))
	return nil
}

// setShareLocked starts or stops the phone's picture server as cfg says,
// keeping the one running when nothing about it changed; the caller holds
// srcMu. A new secret is made for a first start and kept in cfg.
func (m *manager) setShareLocked(cfg *sourceConfig) error {
	if !cfg.SharePhone {
		if m.shared != nil {
			m.sink.SetReceiver(nil)
			m.shared.Close()
			m.shared = nil
		}
		return nil
	}
	port := cfg.SharePort
	if port == 0 {
		port = share.DefaultPort
	}
	if m.shared != nil && m.shared.Port() == port {
		return nil
	}
	if m.shared != nil {
		m.sink.SetReceiver(nil)
		m.shared.Close()
		m.shared = nil
	}
	s, err := startShare(cfg, m.log, m.reporter())
	if err != nil {
		return err
	}
	m.shared = s
	m.sink.SetReceiver(s)
	return nil
}

// startShare serves the phone's picture on loopback when cfg asks for it
// (nil otherwise): a secret is made when cfg has none and kept in it, so a
// program set up to read the address once keeps working.
func startShare(cfg *sourceConfig, log *slog.Logger, ui reporter) (*share.Server, error) {
	if !cfg.SharePhone {
		return nil, nil
	}
	if cfg.ShareSecret == "" {
		secret, err := share.NewSecret()
		if err != nil {
			return nil, err
		}
		cfg.ShareSecret = secret
	}
	var s *share.Server
	s, err := share.New(share.Config{
		Port: cfg.SharePort, Secret: cfg.ShareSecret, Logger: log,
		OnChange: func(arriving bool) { ui.Share(arriving, s.URL()) },
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// rememberedConfig is what source.json keeps: cfg with the devices by the
// names the offers resolved them to, so a later start finds them even if
// their numbering changed.
func rememberedConfig(cfg sourceConfig, body, face *offer) sourceConfig {
	save := cfg
	if body != nil {
		save.Kind = body.save.Kind
		save.Camera, save.Mic = body.save.Camera, body.save.Mic
		save.URL, save.Fingerprint = body.save.URL, body.save.Fingerprint
	}
	if face != nil {
		save.FaceCamera = face.save.FaceCamera
	}
	return save
}

// sourceReportOf is the offers and the phone's picture as the reporter
// takes them.
func sourceReportOf(body, face *offer, shared *share.Server) sourceReport {
	var s sourceReport
	if body != nil {
		s = body.report()
	}
	s.Face = face.faceReport()
	if shared != nil {
		s.Share = &shareReport{Ready: true, Address: shared.URL(), Receiving: shared.Arriving()}
	}
	return s
}
