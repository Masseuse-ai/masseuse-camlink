package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// ConnectorService is the window's side of the link to masseuse-camlink.
// Its exported methods are what the page can ask for; Wails generates the
// TypeScript for them (frontend/bindings). What the connector reports comes
// the other way, as "connector" events the page listens to; the shell's
// own words go the same way (the connector stopped, "blocked"; the
// system's standing on the camera and the microphone, "media").
//
// The service starts the connector as a child on ServiceStartup
// (cmd/masseuse-camlink -ipc, docs/DESKTOP.md section 3), relays the JSON
// lines it writes as events, sends the page's requests down its standard
// input, and ends it on ServiceShutdown. The last event of each kind is
// kept, so a page that mounts after the connector has spoken gets the
// state (Snapshot). The connector ending with the relaunch code after an
// update is the cue to start the whole program again (relaunch.go); any
// other end is reported to the page, which can start it again.
type ConnectorService struct {
	app      *application.App
	stateDir string
	log      *slog.Logger

	// The application menu's status lines (menu.go): the connector's
	// version and the state of updates, relabelled as the connector
	// reports; nil until the menu is built.
	menu        *application.Menu
	versionItem *application.MenuItem
	updateItem  *application.MenuItem

	mu       sync.Mutex
	proc     *connectorProcess
	last     map[string]map[string]any // the last event of each kind
	stopping bool
	// wired says the connector is running and its hello has arrived.
	wired bool
	// linkActive and armed are what shouldQuit asks about.
	linkActive bool
	armed      bool
	// forceQuit is set once the person has confirmed a quit during a
	// session; shouldQuit then answers yes.
	forceQuit atomic.Bool
	// relaunch is set when the connector ended with the relaunch code:
	// main starts the program again once the window has closed.
	relaunch atomic.Bool
	// mediaAsking is set while the system's prompts for the camera and
	// the microphone are up (RequestMediaAccess): one ask at a time.
	mediaAsking atomic.Bool
	// logFile is where the connector's standard error goes.
	logFile *logFile
	// args are what the connector is started with after the fixed flags:
	// what this program was started with, passed through.
	args []string
}

func newConnectorService() *ConnectorService {
	return &ConnectorService{
		stateDir: defaultStateDir(),
		log:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		last:     map[string]map[string]any{},
		args:     passthroughArgs(os.Args[1:]),
	}
}

// connectorEvent is the Wails event the connector's lines are relayed as.
const connectorEvent = "connector"

// snapshotKinds are the events kept for a page that mounts late, in the
// order they are replayed. "media" is the shell's own (permissions.go),
// said before the connector's first line.
var snapshotKinds = []string{"media", "hello", "update", "source", "online", "code", "paired", "link", "camera", "face", "units", "device", "blocked"}

func init() {
	application.RegisterEvent[map[string]any](connectorEvent)
}

// SourceChoice is the camera and microphone the person picked, or a camera
// on the network; the fields are the connector's own flags (its
// cmd/masseuse-camlink/source.go, sourceConfig), and a set_source changes
// only the part they speak of.
type SourceChoice struct {
	Camera      string `json:"camera,omitempty"`
	Mic         string `json:"mic,omitempty"`
	VideoSize   string `json:"videoSize,omitempty"`
	FPS         int    `json:"fps,omitempty"`
	Bitrate     string `json:"bitrate,omitempty"`
	Encoder     string `json:"encoder,omitempty"`
	URL         string `json:"url,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// FaceCamera is the camera that carries OBS's picture back to the room
	// as the person's face (the connector's -face-camera); "phone" offers
	// none, and the phone's own camera is the face view, straight to the
	// room.
	FaceCamera string `json:"faceCamera,omitempty"`
	// Share is whether the connector offers the phone's picture to this
	// computer for OBS (-share-phone on or off); nil leaves it as it is.
	Share *bool `json:"share,omitempty"`
}

// ShellInfo is what the window knows about itself.
type ShellInfo struct {
	// Version is the shell's own version; the connector's is in its hello.
	Version string `json:"version"`
	OS      string `json:"os"`
	// StateDir is where the connector keeps its identity, pairings and
	// choices; the Help menu opens it.
	StateDir string `json:"stateDir"`
	// Wired says whether the connector is running behind the window.
	Wired bool `json:"wired"`
	// LogFile is where the connector's log goes (Help, "Show the log").
	LogFile string `json:"logFile"`
}

// ServiceStartup opens the log and starts the connector.
func (s *ConnectorService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return err
	}
	lf, err := openLogFile(filepath.Join(s.stateDir, logFileName))
	if err != nil {
		return err
	}
	s.logFile = lf
	// The file first, and standard error behind a writer that never
	// fails: a program built for the Windows GUI subsystem has no standard
	// error when Explorer starts it (every write fails with an invalid
	// handle), and io.MultiWriter stops at the first writer that fails,
	// which left desktop.log without a single line of the shell's own.
	out := io.MultiWriter(lf, failsafe{os.Stderr})
	s.log = slog.New(slog.NewTextHandler(out, nil))
	// main's own lines (log.Printf, log.Fatal) go the same way.
	log.SetOutput(out)
	s.log.Info("shell: starting", "version", shellVersion(), "os", runtime.GOOS, "stateDir", s.stateDir)
	s.start()
	// The system's standing on the camera and the microphone, the shell's
	// own word, kept in the snapshot for the page and current while the
	// program runs (permissions.go, watchMedia).
	s.reportMedia(mediaAuthStatus())
	if mediaWatched {
		go s.watchMedia(ctx)
	}
	return nil
}

// ServiceShutdown ends the connector: quit on its standard input, which
// ends it cleanly (the camera off, the unit released), a bounded wait,
// then the process is killed.
func (s *ConnectorService) ServiceShutdown() error {
	s.mu.Lock()
	s.stopping = true
	p := s.proc
	s.mu.Unlock()
	if p != nil {
		p.stop(5 * time.Second)
	}
	if s.logFile != nil {
		s.logFile.Close()
	}
	return nil
}

// start runs the connector, once; a connector already running stays.
func (s *ConnectorService) start() {
	s.mu.Lock()
	if s.proc != nil && !s.proc.done() {
		s.mu.Unlock()
		return
	}
	s.stopping = false
	s.mu.Unlock()

	bin, root, err := locateConnector(s.stateDir, s.log)
	if err != nil {
		s.log.Error("shell: no connector to start", "err", err)
		s.report(map[string]any{"type": "blocked", "kind": "connector-stopped", "detail": err.Error()})
		return
	}
	args := []string{"-ipc", "-state-dir", s.stateDir}
	if root != "" {
		args = append(args, "-install-root", root)
	}
	args = append(args, s.args...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), relaunchEnv+"=1")
	hideWindow(cmd)
	p, err := startConnector(cmd, s.logFile)
	if err != nil {
		s.log.Error("shell: the connector would not start", "bin", bin, "err", err)
		s.report(map[string]any{"type": "blocked", "kind": "connector-stopped", "detail": "The connector could not be started: " + err.Error()})
		return
	}
	s.log.Info("shell: connector started", "bin", bin, "pid", cmd.Process.Pid, "root", root)
	s.mu.Lock()
	s.proc = p
	delete(s.last, "blocked")
	s.mu.Unlock()
	go s.relay(p)
}

// relay hands the connector's lines to the page and reads what the shell
// itself needs from them, then deals with the connector's end.
func (s *ConnectorService) relay(p *connectorProcess) {
	for line := range p.lines {
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			s.log.Warn("shell: not an event", "line", string(line))
			continue
		}
		s.report(ev)
	}
	code := p.wait()
	s.mu.Lock()
	stopping := s.stopping
	s.wired, s.linkActive, s.armed = false, false, false
	s.mu.Unlock()
	switch {
	case code == relaunchExitCode:
		// An update is in place: the whole program starts again
		// (main.go, after the window has closed).
		s.log.Info("shell: the connector asks for a relaunch; quitting to start the new version")
		s.relaunch.Store(true)
		s.forceQuit.Store(true)
		if s.app != nil {
			s.app.Quit()
		}
	case stopping:
		s.log.Info("shell: connector ended", "code", code)
	default:
		detail := fmt.Sprintf("The connector stopped (exit code %d).", code)
		if tail := p.tail(); tail != "" {
			detail += "\n" + tail
		}
		s.log.Error("shell: connector stopped", "code", code)
		s.report(map[string]any{"type": "blocked", "kind": "connector-stopped", "detail": detail})
	}
}

// take reads what the shell needs off an event: the connector's version
// and update line for the menu, the link and the unit for shouldQuit, and
// keeps the last of each kind for Snapshot.
func (s *ConnectorService) take(ev map[string]any) {
	kind, _ := ev["type"].(string)
	var version, update string
	s.mu.Lock()
	for _, k := range snapshotKinds {
		if k == kind {
			s.last[kind] = ev
			break
		}
	}
	switch kind {
	case "hello":
		s.wired = true
		if hello, ok := ev["hello"].(map[string]any); ok {
			version, _ = hello["version"].(string)
			if updates, _ := hello["updates"].(string); updates == "on" {
				update = "Looking for updates every six hours"
			}
		}
	case "update":
		update, _ = ev["text"].(string)
	case "link":
		state, _ := ev["state"].(string)
		s.linkActive = state == "active"
	case "device":
		s.armed = false
		if d, ok := ev["descriptor"].(map[string]any); ok {
			_, s.armed = d["armed"].(map[string]any)
		}
	}
	s.mu.Unlock()
	// The menu is native: relabelled outside the lock, since the main
	// thread it runs on may be asking shouldQuit.
	if version != "" || update != "" {
		s.setStatus(version, update)
	}
}

// report takes note of an event and sends it to the page.
func (s *ConnectorService) report(ev map[string]any) {
	s.take(ev)
	s.emit(ev)
}

// emit sends an event to the page.
func (s *ConnectorService) emit(ev map[string]any) {
	if s.app == nil {
		return
	}
	s.app.Event.Emit(connectorEvent, ev)
}

// Snapshot is the last event of each kind the connector has sent, in the
// order to replay them: a page that mounts after the connector spoke reads
// its state from these before the live events.
func (s *ConnectorService) Snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.last))
	for _, k := range snapshotKinds {
		if ev, ok := s.last[k]; ok {
			out = append(out, ev)
		}
	}
	return out
}

// Info describes the shell.
func (s *ConnectorService) Info() ShellInfo {
	s.mu.Lock()
	wired := s.wired
	s.mu.Unlock()
	return ShellInfo{Version: shellVersion(), OS: runtime.GOOS, StateDir: s.stateDir, Wired: wired, LogFile: filepath.Join(s.stateDir, logFileName)}
}

// setStatus relabels the menu's status lines. Wails rebuilds native menus
// on Update; without it a new label is not shown (Windows in particular).
func (s *ConnectorService) setStatus(version, update string) {
	if s.menu == nil || s.versionItem == nil || s.updateItem == nil {
		return
	}
	if version != "" {
		s.versionItem.SetLabel(appName + " " + version + " · masseuse-camlink")
	}
	if update != "" {
		s.updateItem.SetLabel(update)
	}
	s.menu.Update()
}

// send writes one command to the connector.
func (s *ConnectorService) send(cmd map[string]any) error {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p == nil || p.done() {
		return errNotRunning
	}
	return p.send(cmd)
}

// errNotRunning is the answer of every command while the connector is not
// running.
var errNotRunning = errors.New("the connector is not running")

// ListDevices asks the connector for the cameras and microphones it can
// open; the answer arrives as a "devices" event.
func (s *ConnectorService) ListDevices() error {
	return s.send(map[string]any{"type": "list_devices"})
}

// SetSource chooses the camera and microphone (or the network camera), the
// front-facing camera, or whether the phone's picture is wanted. The
// connector applies it while no session reads the camera and remembers
// it for the next start; the result comes back as a "source" event.
func (s *ConnectorService) SetSource(choice SourceChoice) error {
	return s.send(map[string]any{"type": "set_source", "choice": choice})
}

// SelectUnit serves the stimulation unit with this id from the list the
// connector reported; the unit let go is put to zero first. It is refused
// while a session has the unit armed.
func (s *ConnectorService) SelectUnit(id string) error {
	return s.send(map[string]any{"type": "select_unit", "id": id})
}

// UpdateNow has the connector look for a newer release at once instead of
// at its next check.
func (s *ConnectorService) UpdateNow() error {
	return s.send(map[string]any{"type": "update_now"})
}

// Restart starts the connector again after it stopped.
func (s *ConnectorService) Restart() error {
	s.mu.Lock()
	running := s.proc != nil && !s.proc.done()
	s.mu.Unlock()
	if running {
		return errors.New("the connector is running")
	}
	s.start()
	return nil
}

// RequestMediaAccess asks the system for the camera and the microphone:
// on a Mac the two prompts in turn, while the standing is not determined
// (a refusal is answered at once, without a prompt; System Settings is the
// way back, OpenPrivacySettings). It returns at once, and the answer is a
// "media" event once both are answered. One ask runs at a time: asked
// again while the prompts are up, nothing more happens, the answer to the
// first covers it. The page asks when the Cameras screen opens (or Ready,
// on a computer paired before) and on its Allow button; elsewhere than a
// Mac there is nothing to ask and the standing is authorized.
func (s *ConnectorService) RequestMediaAccess() error {
	if !s.mediaAsking.CompareAndSwap(false, true) {
		return nil
	}
	go func() {
		defer s.mediaAsking.Store(false)
		s.reportMedia(requestMediaAccess())
	}()
	return nil
}

// OpenPrivacySettings opens the system's settings where the camera and the
// microphone are allowed to this application: on a Mac, System Settings ›
// Privacy & Security, on the Camera pane while the camera is not allowed
// and on the Microphone pane otherwise. Nothing elsewhere.
func (s *ConnectorService) OpenPrivacySettings() error {
	return openPrivacySettings(mediaAuthStatus())
}

// reportMedia says the standing on the camera and the microphone to the
// page when it differs from what was last said; the first word always
// goes. Returns whether a word went.
func (s *ConnectorService) reportMedia(camera, mic string) bool {
	s.mu.Lock()
	last, said := s.last["media"]
	s.mu.Unlock()
	if said && last["camera"] == camera && last["mic"] == mic {
		return false
	}
	s.log.Info("shell: camera and microphone access", "camera", camera, "mic", mic)
	s.report(mediaEvent(camera, mic))
	return true
}

// mediaWatchInterval is how often watchMedia reads the standing.
const mediaWatchInterval = 3 * time.Second

// watchMedia keeps reading the standing while the program runs, so a
// switch made in System Settings shows in the window within seconds and
// without a restart. Each read is one cheap question to the system; only
// a change is said.
func (s *ConnectorService) watchMedia(ctx context.Context) {
	t := time.NewTicker(mediaWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reportMedia(mediaAuthStatus())
		}
	}
}

// Quit ends the program: the camera goes off and the unit is released on
// the way out, as with the window's close button.
func (s *ConnectorService) Quit() {
	if s.app != nil {
		s.app.Quit()
	}
}

// links are the pages the Help menu and the About dialog open, by name, so
// the page can only ever open these.
var links = map[string]string{
	"learn-more": "https://masseuse.ai/app",
	"privacy":    "https://github.com/FemLed/masseuse-camlink#how-it-stays-private",
	"verify":     "https://github.com/FemLed/masseuse-camlink/blob/main/VERIFY.md",
	"security":   "https://github.com/FemLed/masseuse-camlink/blob/main/SECURITY.md",
	"source":     "https://github.com/FemLed/masseuse-camlink",
	"releases":   "https://github.com/FemLed/masseuse-camlink/releases",
}

// OpenLink opens one of the known pages in the default browser.
func (s *ConnectorService) OpenLink(name string) error {
	url, ok := links[name]
	if !ok {
		return errors.New("no such link: " + name)
	}
	return s.app.Browser.OpenURL(url)
}

// RevealStateDir shows the state directory in the file manager.
func (s *ConnectorService) RevealStateDir() error {
	if err := os.MkdirAll(s.stateDir, 0o700); err != nil {
		return err
	}
	return s.app.Env.OpenFileManager(s.stateDir, false)
}

// ShowLog reveals the connector's log in the file manager.
func (s *ConnectorService) ShowLog() error {
	path := filepath.Join(s.stateDir, logFileName)
	if _, err := os.Stat(path); err != nil {
		return s.RevealStateDir()
	}
	return s.app.Env.OpenFileManager(path, true)
}

// shouldQuit is asked before the program ends. While a session has the
// camera or the unit is armed, the person is asked first: the quit is
// refused and a question shown; "Quit anyway" answers it by quitting
// again with forceQuit set.
func (s *ConnectorService) shouldQuit() bool {
	if s.forceQuit.Load() {
		return true
	}
	s.mu.Lock()
	busy := s.linkActive || s.armed
	armed := s.armed
	s.mu.Unlock()
	if !busy {
		return true
	}
	if s.app == nil {
		return false
	}
	message := "A session on your phone is using this computer's camera. Quitting ends it."
	if armed {
		message = "A session on your phone has the stimulation unit armed. Quitting puts the unit to zero and ends the session."
	}
	dialog := s.app.Dialog.Question().SetTitle("Quit " + appName + "?").SetMessage(message)
	dialog.AddButton("Keep running").SetAsCancel()
	dialog.AddButton("Quit").SetAsDefault().OnClick(func() {
		s.forceQuit.Store(true)
		s.app.Quit()
	})
	dialog.Show()
	return false
}

// shouldRelaunch says whether the program is to start again once the
// window has closed (an update is in place).
func (s *ConnectorService) shouldRelaunch() bool { return s.relaunch.Load() }

// passthroughArgs are this program's own arguments handed to the
// connector: -service, -log-level, -no-update and the rest of its flags,
// for people who start the window from a terminal. --version is the
// shell's own (main.go) and does not pass.
func passthroughArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--version" || a == "-version" {
			continue
		}
		out = append(out, a)
	}
	return out
}

// connectorProcess is one run of the connector.
type connectorProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// lines are the connector's standard output, one event each, closed
	// when it ends.
	lines chan []byte
	// tailBuf keeps the last lines of standard error for the page when
	// the connector stops on its own.
	tailBuf *tailBuffer

	wmu    sync.Mutex
	exited chan struct{}
	code   int
}

// startConnector starts cmd with its standard error going to the log and
// its standard output read line by line.
func startConnector(cmd *exec.Cmd, log io.Writer) (*connectorProcess, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tail := &tailBuffer{max: 40}
	if log == nil {
		log = io.Discard
	}
	cmd.Stderr = io.MultiWriter(log, tail)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &connectorProcess{cmd: cmd, stdin: stdin, lines: make(chan []byte, 64), tailBuf: tail, exited: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			p.lines <- []byte(line)
		}
		close(p.lines)
	}()
	go func() {
		err := cmd.Wait()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			code = -1
		}
		p.wmu.Lock()
		p.code = code
		p.wmu.Unlock()
		close(p.exited)
	}()
	return p, nil
}

func (p *connectorProcess) send(cmd map[string]any) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// done says whether the process has ended.
func (p *connectorProcess) done() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// wait blocks until the process has ended and returns its exit code.
func (p *connectorProcess) wait() int {
	<-p.exited
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return p.code
}

// stop asks the connector to quit and waits at most grace for it; then it
// is killed.
func (p *connectorProcess) stop(grace time.Duration) {
	_ = p.send(map[string]any{"type": "quit"})
	p.wmu.Lock()
	_ = p.stdin.Close()
	p.wmu.Unlock()
	select {
	case <-p.exited:
	case <-time.After(grace):
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
}

func (p *connectorProcess) tail() string { return p.tailBuf.String() }

// tailBuffer keeps the last max lines written to it.
type tailBuffer struct {
	mu    sync.Mutex
	max   int
	lines []string
	part  string
}

func (t *tailBuffer) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.part + string(b)
	parts := strings.Split(s, "\n")
	t.part = parts[len(parts)-1]
	for _, line := range parts[:len(parts)-1] {
		t.lines = append(t.lines, line)
	}
	if len(t.lines) > t.max {
		t.lines = t.lines[len(t.lines)-t.max:]
	}
	return len(b), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(strings.Join(t.lines, "\n"))
}

// failsafe writes to w and reports every write whole, whatever w says: for
// a standard error that may not exist (the Windows GUI subsystem), so the
// writers after it in an io.MultiWriter still get their turn.
type failsafe struct{ w io.Writer }

func (f failsafe) Write(b []byte) (int, error) {
	_, _ = f.w.Write(b)
	return len(b), nil
}

// defaultStateDir is the connector's (cmd/masseuse-camlink/main.go), so the
// Help menu opens the directory the connector uses.
func defaultStateDir() string {
	if v := os.Getenv("MASSEUSE_CAMLINK_STATE_DIR"); v != "" {
		return v
	}
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

// version is set by the release build (-ldflags "-X main.version=v0.1.0");
// a build from a working tree reports what the Go toolchain knows.
var version string

func shellVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
