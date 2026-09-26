package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/capture"
	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// The desktop window (docs/DESKTOP.md) runs this program with -ipc and
// talks to it over its standard streams: one JSON object per line, events
// out on standard output, commands in on standard input, the log on
// standard error as always. The events are the facts the console prints
// (report.go), in the shapes the window's page types
// (desktop/frontend/src/bridge/types.ts); the commands are the flags and
// the console picker, as requests. Standard input reaching its end is a
// shutdown: a window that was killed never leaves a connector behind.

// ipcMode is the -ipc flag: the program runs for the desktop window.
var ipcMode = flag.Bool("ipc", false, "run for the desktop window: events as JSON lines on standard output, commands on standard input, which ending stops the program")

// ipcHello is the connector's first word: what the console's first lines
// say, gathered.
type ipcHello struct {
	Version     string `json:"version"`
	Identity    string `json:"identity"`
	StateDir    string `json:"stateDir"`
	Updates     string `json:"updates"` // "on" or "off"
	UpdatesNote string `json:"updatesNote,omitempty"`
	Awake       bool   `json:"awake"`
	AwakeNote   string `json:"awakeNote,omitempty"`
	Drivers     string `json:"drivers"`
	Phones      int    `json:"phones"`
}

type ipcShare struct {
	Ready     bool   `json:"ready"`
	Address   string `json:"address,omitempty"`
	Receiving bool   `json:"receiving"`
}

type ipcFace struct {
	Label  string `json:"label"`
	Camera string `json:"camera"`
	Ready  bool   `json:"ready"`
	Note   string `json:"note,omitempty"`
}

// ipcSource is the "source" event: the connector's offer.
type ipcSource struct {
	Type   string    `json:"type"`
	Kind   string    `json:"kind"`
	Label  string    `json:"label"`
	Ready  bool      `json:"ready"`
	Note   string    `json:"note,omitempty"`
	Shape  string    `json:"shape,omitempty"`
	Camera string    `json:"camera,omitempty"`
	Mic    string    `json:"mic,omitempty"`
	URL    string    `json:"url,omitempty"`
	Share  *ipcShare `json:"share,omitempty"`
	// Face is null for the phone's own camera; the key is always there,
	// so the page tells "the phone" from "an older connector".
	Face *ipcFace `json:"face"`
}

type ipcDevice struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

type ipcSubstitution struct {
	Kind   string `json:"kind"`
	Wanted string `json:"wanted"`
	Using  string `json:"using"`
}

type ipcEnclave struct {
	Image    string `json:"image"`
	Release  string `json:"release"`
	Commit   string `json:"commit"`
	Source   string `json:"source"`
	Registry string `json:"registry"`
	SignedBy string `json:"signedBy"`
	Cached   bool   `json:"cached"`
}

type ipcStats struct {
	VideoBps  float64 `json:"videoBps"`
	AudioBps  float64 `json:"audioBps"`
	Congested bool    `json:"congested"`
	BacklogS  float64 `json:"backlogS"`
	// Reason is why the camera is on but nothing is being sent, when the
	// source can say (capture.Source.Trouble: the camera delivered no
	// picture, ffmpeg could not open it, ...), for the page to show beside
	// the idle meters; empty while sending or while it is too soon to say.
	Reason string `json:"reason,omitempty"`
}

// ipcDescriptor is the served unit for the page: the connector's
// Descriptor with what it last reported and whether a session has it armed.
type ipcDescriptor struct {
	estim.Descriptor
	Status *ipcStatus `json:"status,omitempty"`
	Armed  *ipcArmed  `json:"armed,omitempty"`
}

type ipcStatus struct {
	BatteryPercent *int    `json:"batteryPercent,omitempty"`
	Power          *string `json:"power,omitempty"`
	Mode           *int    `json:"mode,omitempty"`
	LevelA         *int    `json:"levelA,omitempty"`
	LevelB         *int    `json:"levelB,omitempty"`
	Outputting     *bool   `json:"outputting,omitempty"`
}

type ipcArmed struct {
	LevelBound int `json:"levelBound"`
}

// ipcReporter is the reporter behind -ipc: every fact a JSON line. Until
// Ready, startup facts are gathered into the hello and everything else
// waits behind it, so the window's first line is always the hello.
type ipcReporter struct {
	mu      sync.Mutex
	w       io.Writer
	ready   bool
	pending []any
	hello   ipcHello
	source  *sourceReport
	enclave *ipcEnclave
	// notSending is the reason last given for a camera that is on but not
	// sending (NotSending), so the notice about it is raised once, not at
	// every report; sending, or the camera going off, forgets it.
	notSending string
	// cameraOff and faceOff are the window's switches as last said
	// (CameraEnabled, FaceEnabled), stamped on every camera and face event
	// so the last of each kind, which the shell keeps for a page that
	// mounts late, carries the standing. Zero is on.
	cameraOff, faceOff bool
	log                *slog.Logger
}

func newIPCReporter(w io.Writer, log *slog.Logger) *ipcReporter {
	return &ipcReporter{w: w, log: log, hello: ipcHello{Updates: "on"}}
}

// emit writes one event; before Ready it is kept for after the hello.
func (r *ipcReporter) emit(v any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready {
		r.pending = append(r.pending, v)
		return
	}
	r.write(v)
}

func (r *ipcReporter) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		r.log.Error("ipc: could not encode an event", "err", err)
		return
	}
	if _, err := r.w.Write(append(b, '\n')); err != nil {
		r.log.Error("ipc: could not write an event", "err", err)
	}
}

func (r *ipcReporter) Banner(version, identity, stateDir string) {
	r.mu.Lock()
	r.hello.Version, r.hello.Identity, r.hello.StateDir = version, identity, stateDir
	r.mu.Unlock()
}

func (r *ipcReporter) Drivers(line string) {
	r.mu.Lock()
	r.hello.Drivers = line
	r.mu.Unlock()
}

func (r *ipcReporter) PairedCount(n int) {
	r.mu.Lock()
	r.hello.Phones = n
	r.mu.Unlock()
}

func (r *ipcReporter) Awake(state awakeState, note string) {
	r.mu.Lock()
	r.hello.Awake = state == awakeHeld
	switch state {
	case awakeAllowed:
		r.hello.AwakeNote = "This computer may go to sleep on its own (-allow-sleep)."
	case awakeFailed:
		r.hello.AwakeNote = "Could not keep this computer from sleeping (" + note + ")."
	default:
		r.hello.AwakeNote = ""
	}
	r.mu.Unlock()
}

func (r *ipcReporter) Ready() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready {
		return
	}
	r.ready = true
	r.write(struct {
		Type  string   `json:"type"`
		Hello ipcHello `json:"hello"`
	}{"hello", r.hello})
	if r.source != nil {
		r.write(r.sourceEvent(*r.source))
	}
	// The switches' standing, both cameras off and allowed: the page shows
	// a switch only once a connector has spoken of `enabled`, so an older
	// connector, which never does, shows none.
	r.write(ipcCamera{Type: "camera", Enabled: !r.cameraOff})
	r.write(ipcCamera{Type: "face", Enabled: !r.faceOff})
	for _, v := range r.pending {
		r.write(v)
	}
	r.pending = nil
}

func (r *ipcReporter) Stopped() {}

func (r *ipcReporter) Online(online bool) {
	r.emit(struct {
		Type   string `json:"type"`
		Online bool   `json:"online"`
	}{"online", online})
}

func (r *ipcReporter) Code(code string, expiresAt time.Time) {
	ev := struct {
		Type      string `json:"type"`
		Code      string `json:"code"`
		ExpiresAt string `json:"expiresAt,omitempty"`
	}{Type: "code", Code: code}
	if !expiresAt.IsZero() && expiresAt.Year() > 2000 {
		ev.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	}
	r.emit(ev)
}

func (r *ipcReporter) Paired(n int) {
	r.mu.Lock()
	r.hello.Phones = n
	r.mu.Unlock()
	r.emit(struct {
		Type   string `json:"type"`
		Phones int    `json:"phones"`
	}{"paired", n})
}

func (r *ipcReporter) Source(s sourceReport) {
	r.mu.Lock()
	r.source = &s
	if !r.ready {
		r.mu.Unlock()
		return
	}
	r.write(r.sourceEvent(s))
	r.mu.Unlock()
}

func (r *ipcReporter) sourceEvent(s sourceReport) ipcSource {
	ev := ipcSource{Type: "source", Kind: s.Kind, Label: s.Label, Ready: s.Ready, Note: s.Note, Shape: s.Shape,
		Camera: s.Camera, Mic: s.Mic, URL: s.URL}
	if s.Share != nil {
		ev.Share = &ipcShare{Ready: s.Share.Ready, Address: s.Share.Address, Receiving: s.Share.Receiving}
	}
	if s.Face != nil {
		ev.Face = &ipcFace{Label: s.Face.Label, Camera: s.Face.Camera, Ready: s.Face.Ready, Note: s.Face.Note}
	}
	return ev
}

// Devices is the answer to list_devices.
func (r *ipcReporter) Devices(devs []capture.Device, subs []capture.Substitution, err error) {
	ev := struct {
		Type          string            `json:"type"`
		Cameras       []ipcDevice       `json:"cameras"`
		Mics          []ipcDevice       `json:"mics"`
		Substitutions []ipcSubstitution `json:"substitutions"`
		Error         string            `json:"error,omitempty"`
	}{Type: "devices", Cameras: []ipcDevice{}, Mics: []ipcDevice{}, Substitutions: []ipcSubstitution{}}
	for _, d := range devs {
		item := ipcDevice{Kind: string(d.Kind), ID: d.ID, Name: d.Name}
		if d.Kind == capture.Video {
			ev.Cameras = append(ev.Cameras, item)
		} else {
			ev.Mics = append(ev.Mics, item)
		}
	}
	for _, s := range subs {
		ev.Substitutions = append(ev.Substitutions, ipcSubstitution{Kind: string(s.Kind), Wanted: s.Wanted, Using: s.Using.Name})
	}
	if err != nil {
		ev.Error = err.Error()
	}
	r.emit(ev)
}

func (r *ipcReporter) Link(state linkState, reason string) {
	ev := struct {
		Type    string      `json:"type"`
		State   string      `json:"state"`
		Reason  string      `json:"reason,omitempty"`
		Enclave *ipcEnclave `json:"enclave,omitempty"`
	}{Type: "link", State: string(state), Reason: reason}
	if state == linkReset {
		ev.State = string(linkClosed)
	}
	if state == linkActive {
		r.mu.Lock()
		ev.Enclave = r.enclave
		r.mu.Unlock()
	}
	r.emit(ev)
}

func (r *ipcReporter) Enclave(p enclaveProof) {
	r.mu.Lock()
	r.enclave = &ipcEnclave{Image: p.Image, Release: p.Release, Commit: p.Commit, Source: p.Source,
		Registry: p.Registry, SignedBy: p.SignedBy, Cached: p.Cached}
	r.mu.Unlock()
}

// ipcCamera is the "camera" event, and the "face" event in the same shape:
// whether the capture is on, its rates while sending, and the standing of
// the window's switch on it (Enabled), which a current connector always
// says.
type ipcCamera struct {
	Type    string    `json:"type"`
	On      bool      `json:"on"`
	Stats   *ipcStats `json:"stats,omitempty"`
	Enabled bool      `json:"enabled"`
}

// camera is a camera event with the switch's standing stamped on.
func (r *ipcReporter) camera(on bool, stats *ipcStats) ipcCamera {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ipcCamera{Type: "camera", On: on, Stats: stats, Enabled: !r.cameraOff}
}

// face is a face event with the switch's standing stamped on.
func (r *ipcReporter) face(on bool, stats *ipcStats) ipcCamera {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ipcCamera{Type: "face", On: on, Stats: stats, Enabled: !r.faceOff}
}

func (r *ipcReporter) CameraOn(string) {
	r.forgetNotSending()
	r.emit(r.camera(true, nil))
}

func (r *ipcReporter) CameraOff() {
	r.forgetNotSending()
	r.emit(r.camera(false, nil))
}

func (r *ipcReporter) Sending(_ string, videoBps, audioBps float64, congested bool, backlog time.Duration) {
	r.forgetNotSending()
	r.emit(r.camera(true, &ipcStats{VideoBps: videoBps, AudioBps: audioBps, Congested: congested, BacklogS: backlog.Seconds()}))
}

// NotSending is the camera on with nothing sent, every report: the idle
// meters carry the reason, when there is one, for the page to show for as
// long as it holds, and a notice says it the first time it is given (a
// reason that stands for a whole session would otherwise be raised every
// ten seconds).
func (r *ipcReporter) NotSending(reason string) {
	r.emit(r.camera(true, &ipcStats{Reason: reason}))
	if reason == "" {
		return
	}
	r.mu.Lock()
	said := r.notSending == reason
	r.notSending = reason
	r.mu.Unlock()
	if !said {
		r.Notice(noticeWarn, "Camera on but not sending yet: "+reason+".")
	}
}

func (r *ipcReporter) forgetNotSending() {
	r.mu.Lock()
	r.notSending = ""
	r.mu.Unlock()
}

func (r *ipcReporter) FaceOn(string) { r.emit(r.face(true, nil)) }
func (r *ipcReporter) FaceOff()      { r.emit(r.face(false, nil)) }

// CameraEnabled is the window's switch on the camera: the standing is kept
// for every camera event from here on and said at once, the camera off
// (switched off, it is being stopped; switched on, nothing starts until a
// session reads it).
func (r *ipcReporter) CameraEnabled(enabled bool) {
	r.mu.Lock()
	r.cameraOff = !enabled
	r.mu.Unlock()
	r.forgetNotSending()
	r.emit(r.camera(false, nil))
}

// FaceEnabled is the window's switch on the front-facing camera; as
// CameraEnabled.
func (r *ipcReporter) FaceEnabled(enabled bool) {
	r.mu.Lock()
	r.faceOff = !enabled
	r.mu.Unlock()
	r.emit(r.face(false, nil))
}

func (r *ipcReporter) Share(receiving bool, url string) {
	r.mu.Lock()
	if r.source != nil && r.source.Share != nil {
		s := *r.source
		sh := *s.Share
		sh.Receiving = receiving
		if url != "" {
			sh.Address = url
		}
		s.Share = &sh
		r.source = &s
		if r.ready {
			r.write(r.sourceEvent(s))
		}
	}
	r.mu.Unlock()
}

func (r *ipcReporter) Units(units []estim.Unit, _ *estim.Descriptor, _ bool) {
	if units == nil {
		units = []estim.Unit{}
	}
	r.emit(struct {
		Type     string       `json:"type"`
		Units    []estim.Unit `json:"units"`
		Scanning bool         `json:"scanning"`
	}{"units", units, false})
}

func (r *ipcReporter) Device(d estim.Descriptor, _ bool) {
	r.emit(struct {
		Type       string        `json:"type"`
		Descriptor ipcDescriptor `json:"descriptor"`
	}{"device", ipcDescriptor{Descriptor: d}})
}

func (r *ipcReporter) DeviceState(d estim.Descriptor, st estim.Status, armed bool, bound int) {
	desc := ipcDescriptor{Descriptor: d}
	if d.Connected {
		desc.Status = &ipcStatus{BatteryPercent: st.BatteryPercent, Power: st.Power, Mode: st.Mode, LevelA: st.LevelA, LevelB: st.LevelB, Outputting: st.Outputting}
	}
	if armed {
		desc.Armed = &ipcArmed{LevelBound: bound}
	}
	r.emit(struct {
		Type       string        `json:"type"`
		Descriptor ipcDescriptor `json:"descriptor"`
	}{"device", desc})
}

type ipcUpdate struct {
	Type  string `json:"type"`
	State string `json:"state"`
	Tag   string `json:"tag,omitempty"`
	Text  string `json:"text"`
}

func (r *ipcReporter) Update(state updateState, tag, text string) {
	if state == updateOff {
		r.mu.Lock()
		r.hello.Updates = "off"
		r.hello.UpdatesNote = text
		r.mu.Unlock()
	}
	r.emit(ipcUpdate{Type: "update", State: string(state), Tag: tag, Text: text})
}

func (r *ipcReporter) UpdateQuiet(state updateState, tag, text string) {
	r.emit(ipcUpdate{Type: "update", State: string(state), Tag: tag, Text: text})
}

func (r *ipcReporter) Notice(level noticeLevel, text string) {
	r.emit(struct {
		Type  string `json:"type"`
		Level string `json:"level"`
		Text  string `json:"text"`
	}{"notice", string(level), strings.TrimRight(text, "\n")})
}

// Blocked goes out at once, hello or not: the program is about to end.
func (r *ipcReporter) Blocked(kind blockedKind, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write(struct {
		Type   string `json:"type"`
		Kind   string `json:"kind"`
		Detail string `json:"detail,omitempty"`
	}{"blocked", string(kind), detail})
}

// ipcCommand is one line of standard input.
type ipcCommand struct {
	Type   string        `json:"type"`
	Choice *sourceChoice `json:"choice,omitempty"`
	ID     string        `json:"id,omitempty"`
	// View and Enabled are a set_camera: the window's switch on the camera
	// ("body") or the front-facing camera ("face"), off or on again.
	View    string `json:"view,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// sourceChoice is a set_source: the source flags as fields, each part
// (the camera, the front-facing camera, the phone's picture) changed only
// when spoken of, as on the command line (resolveSourceConfig).
type sourceChoice struct {
	Camera      string `json:"camera,omitempty"`
	Mic         string `json:"mic,omitempty"`
	VideoSize   string `json:"videoSize,omitempty"`
	FPS         int    `json:"fps,omitempty"`
	Bitrate     string `json:"bitrate,omitempty"`
	Encoder     string `json:"encoder,omitempty"`
	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// FaceCamera names the front-facing camera; "phone" (or "none") puts
	// the phone's own camera back.
	FaceCamera string `json:"faceCamera,omitempty"`
	// Share is whether the phone's picture is wanted on this computer;
	// nil leaves it as it is. SharePort is the loopback port it is served
	// on; 0 leaves it as it is.
	Share     *bool `json:"share,omitempty"`
	SharePort int   `json:"sharePort,omitempty"`
}

// flags turns the choice into the command line's terms.
func (c sourceChoice) flags() sourceFlags {
	f := sourceFlags{camera: c.Camera, mic: c.Mic, videoSize: c.VideoSize, fps: c.FPS, bitrate: c.Bitrate,
		encoder: c.Encoder, cameraURL: c.URL, cameraFingerprint: c.Fingerprint, sharePort: c.SharePort}
	switch strings.ToLower(strings.TrimSpace(c.FaceCamera)) {
	case "":
	case "phone", "none":
		f.faceCamera = "none"
	default:
		f.faceCamera = c.FaceCamera
	}
	if c.Share != nil {
		if *c.Share {
			f.sharePhone = "on"
		} else {
			f.sharePhone = "off"
		}
	}
	return f
}

// ipcMaxLine bounds one command line.
const ipcMaxLine = 1 << 20

// ipcCommands answers the window's requests.
type ipcCommands struct {
	ui  *ipcReporter
	mgr *manager
	upd *updater
	// ffmpeg is the executable named by the flags ("" for the one found).
	ffmpeg string
	// stop ends the program (quit, or standard input ending).
	stop func()
	log  *slog.Logger
}

// run reads commands until in ends, then stops the program.
func (c *ipcCommands) run(ctx context.Context, in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), ipcMaxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var cmd ipcCommand
		if err := json.Unmarshal([]byte(line), &cmd); err != nil {
			c.log.Warn("ipc: not a command", "err", err)
			continue
		}
		if c.handle(ctx, cmd) {
			return
		}
	}
	if err := sc.Err(); err != nil {
		c.log.Warn("ipc: standard input", "err", err)
	}
	c.log.Info("ipc: standard input ended; stopping")
	c.stop()
}

// handle runs one command; true when the program is to stop.
func (c *ipcCommands) handle(ctx context.Context, cmd ipcCommand) bool {
	switch cmd.Type {
	case "quit":
		c.log.Info("ipc: quit")
		c.stop()
		return true
	case "list_devices":
		devs, err := deviceListing(ctx, c.ffmpeg)
		var subs []capture.Substitution
		if c.mgr != nil && c.mgr.cam != nil {
			subs = c.mgr.cam.substitutions()
		}
		c.ui.Devices(devs, subs, err)
	case "set_source":
		if cmd.Choice == nil {
			c.ui.Notice(noticeError, "set_source needs a choice.")
			return false
		}
		if err := c.mgr.applySource(ctx, *cmd.Choice); err != nil {
			c.ui.Notice(noticeError, firstLine(err))
		}
	case "set_camera":
		// The switch on Ready: off stops the capture now and keeps it off
		// for every session until on again (camControl.setEnabled); the
		// standing comes back as a camera or face event.
		if cmd.Enabled == nil {
			c.ui.Notice(noticeError, "set_camera needs enabled, true or false.")
			return false
		}
		if err := c.mgr.cam.setEnabled(cmd.View, *cmd.Enabled); err != nil {
			c.ui.Notice(noticeError, firstLine(err))
		}
	case "select_unit":
		if c.mgr.estim == nil {
			c.ui.Notice(noticeWarn, "No stimulation device is served: every device family is switched off by the flags.")
			return false
		}
		label := cmd.ID
		for _, u := range c.mgr.estim.rt.Units() {
			if u.ID == cmd.ID {
				label = u.Label
			}
		}
		c.mgr.estim.choose(ctx, cmd.ID, label)
	case "update_now":
		if c.upd == nil {
			c.ui.UpdateQuiet(updateOff, "", "Updates are off for this install.")
			return false
		}
		c.upd.checkNow()
	default:
		c.log.Warn("ipc: unknown command", "type", cmd.Type)
		c.ui.Notice(noticeWarn, fmt.Sprintf("Unknown command %q.", cmd.Type))
	}
	return false
}

// watchDevice tells the window the served unit's state as it changes
// (armed, the levels, the battery), which the console never prints: the
// runtime is read every second while a unit is connected.
func (c *ipcCommands) watchDevice(ctx context.Context) {
	if c.mgr.estim == nil {
		return
	}
	rt := c.mgr.estim.rt
	type snapshot struct {
		connected, armed bool
		bound            int
		levelA, levelB   int
		battery, mode    int
		power            string
		outputting       bool
	}
	take := func() (snapshot, estim.Descriptor, estim.Status) {
		d, st := rt.Descriptor(), rt.LastStatus()
		s := snapshot{connected: d.Connected, armed: rt.Armed(), bound: rt.Settings().LevelMax}
		if st.LevelA != nil {
			s.levelA = *st.LevelA
		}
		if st.LevelB != nil {
			s.levelB = *st.LevelB
		}
		if st.BatteryPercent != nil {
			s.battery = *st.BatteryPercent
		}
		if st.Mode != nil {
			s.mode = *st.Mode
		}
		if st.Power != nil {
			s.power = *st.Power
		}
		if st.Outputting != nil {
			s.outputting = *st.Outputting
		}
		return s, d, st
	}
	last, _, _ := take()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s, d, st := take()
		if s == last || !s.connected {
			last = s
			continue
		}
		last = s
		c.ui.DeviceState(d, st, s.armed, s.bound)
	}
}

// deviceListing is what the computer can capture: the cameras and
// microphones ffmpeg lists (the `devices` command and list_devices).
func deviceListing(ctx context.Context, ffmpegPath string) ([]capture.Device, error) {
	ffmpeg, err := capture.FindFFmpeg(ffmpegPath)
	if err != nil {
		return nil, fmt.Errorf("%v; %s", err, capture.InstallHint())
	}
	return capture.Devices(ctx, ffmpeg)
}

// errCameraBusy is applySource's answer while a session reads the camera.
var errCameraBusy = errors.New("a session is reading the camera; the change is applied when it lets go")
