package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// How long the Host gives a helper to answer `hello` after starting it.
const helloTimeout = 10 * time.Second

// How often a running execute looks at the Runtime's cancellation latch.
const cancelPoll = 50 * time.Millisecond

// A Host runs one helper program and presents it as a device family: an
// estim.Finder that is also a Lister and a Selector. The process is
// started on first use and again after it exits; a call outstanding when
// it exits fails with ErrExited, which a Driver call reports as a loss.
type Host struct {
	// Path is the helper program.
	Path string
	// StateDir is passed to the helper as --state-dir: where it may keep
	// what it must remember (a pairing key, a remembered port).
	StateDir string
	// Log receives the helper's stderr, line by line, and the Host's own
	// notes. Nil discards.
	Log *slog.Logger
	// Start replaces exec.Command for tests: it must return a running
	// process's pipes and a wait function.
	Start func(ctx context.Context, path string, args []string) (Process, error)

	mu      sync.Mutex
	proc    Process
	hello   *Hello
	nextID  uint64
	pending map[uint64]chan envelope
	writeMu sync.Mutex
	writer  io.Writer
	exited  chan struct{}
	// selection is the last Select, repeated to a restarted helper.
	selection *string
}

// A Process is a running helper: its stdin, its stdout, its stderr, and a
// way to wait for and end it.
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
	// Wait blocks until the process ends.
	Wait func() error
	// Kill ends the process.
	Kill func() error
}

// New is a Host for the helper program at path.
func New(path, stateDir string, log *slog.Logger) *Host {
	return &Host{Path: path, StateDir: stateDir, Log: log}
}

func (h *Host) log() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

// Name is the family's name from `hello` ("" before the helper has said it).
func (h *Host) Name() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hello == nil {
		return ""
	}
	return h.hello.Name
}

// Kinds are what the helper's driver may report ("" before hello).
func (h *Host) Kinds() []estim.Kind {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hello == nil {
		return nil
	}
	return append([]estim.Kind(nil), h.hello.Kinds...)
}

// Hello starts the helper if it is not running and returns what it said
// about itself.
func (h *Host) Hello(ctx context.Context) (Hello, error) {
	if err := h.ensure(ctx); err != nil {
		return Hello{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.hello, nil
}

// How long Close gives a helper to end on its own once its stdin is closed.
const closeGrace = 2 * time.Second

// Close ends the helper process, if any: its stdin is closed, which a
// helper takes as the connector going away (Serve returns, closing the
// device), and one that is still running after a moment is killed.
func (h *Host) Close() error {
	h.mu.Lock()
	proc := h.proc
	exited := h.exited
	h.proc = Process{}
	h.mu.Unlock()
	if proc.Wait == nil && proc.Kill == nil {
		return nil
	}
	if proc.Stdin != nil {
		_ = proc.Stdin.Close()
	}
	if exited != nil {
		select {
		case <-exited:
			return nil
		case <-time.After(closeGrace):
		}
	}
	if proc.Kill == nil {
		return nil
	}
	err := proc.Kill()
	if exited != nil {
		select {
		case <-exited:
			return nil // gone on its own meanwhile; the kill's error is moot
		default:
		}
	}
	return err
}

// ensure has the helper running and greeted.
func (h *Host) ensure(ctx context.Context) error {
	h.mu.Lock()
	running := h.proc.Wait != nil && h.exited != nil
	if running {
		select {
		case <-h.exited:
			running = false
		default:
		}
	}
	if running {
		h.mu.Unlock()
		return nil
	}
	h.mu.Unlock()

	start := h.Start
	if start == nil {
		start = startCommand
	}
	args := []string{}
	if h.StateDir != "" {
		args = append(args, "--state-dir", h.StateDir)
	}
	proc, err := start(ctx, h.Path, args)
	if err != nil {
		return fmt.Errorf("helper: starting %s: %w", h.Path, err)
	}
	exited := make(chan struct{})
	h.mu.Lock()
	h.proc = proc
	h.exited = exited
	h.pending = map[uint64]chan envelope{}
	h.hello = nil
	h.writeMu.Lock()
	h.writer = proc.Stdin
	h.writeMu.Unlock()
	h.mu.Unlock()
	go h.readStdout(proc.Stdout, exited)
	go h.readStderr(proc.Stderr)
	go func() {
		err := proc.Wait()
		h.log().Info("helper: program ended", "path", h.Path, "err", err)
	}()

	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	var hello Hello
	if err := h.call(hctx, MethodHello, nil, &hello); err != nil {
		_ = proc.Kill()
		return fmt.Errorf("helper: %s did not say hello: %w", h.Path, err)
	}
	if hello.Protocol != Protocol {
		_ = proc.Kill()
		return fmt.Errorf("helper: %s speaks protocol %d, this connector %d", h.Path, hello.Protocol, Protocol)
	}
	if hello.Name == "" {
		_ = proc.Kill()
		return fmt.Errorf("helper: %s gave no name", h.Path)
	}
	h.mu.Lock()
	h.hello = &hello
	selection := h.selection
	h.mu.Unlock()
	if selection != nil {
		if err := h.call(ctx, MethodSelect, selectParams{Unit: *selection}, nil); err != nil {
			h.log().Warn("helper: the remembered selection was not taken", "helper", hello.Name, "err", err)
		}
	}
	return nil
}

// startCommand is the real Start: the program with its stdio piped. The
// process outlives the call that started it (ctx is the caller's, not the
// helper's lifetime); Close ends it.
func startCommand(_ context.Context, path string, args []string) (Process, error) {
	cmd := exec.Command(path, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Process{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Process{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Process{}, err
	}
	if err := cmd.Start(); err != nil {
		return Process{}, err
	}
	return Process{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Wait:   cmd.Wait,
		Kill: func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Kill()
		},
	}, nil
}

// readStdout delivers answers to their callers; when the pipe ends, every
// caller still waiting hears ErrExited.
func (h *Host) readStdout(r io.Reader, exited chan struct{}) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			h.log().Warn("helper: unreadable line from the program", "path", h.Path, "err", err)
			continue
		}
		if env.ID == nil {
			// A notification from the helper: none defined yet; logged.
			h.log().Debug("helper: notification", "method", env.Method)
			continue
		}
		h.mu.Lock()
		ch := h.pending[*env.ID]
		delete(h.pending, *env.ID)
		h.mu.Unlock()
		if ch != nil {
			ch <- env
		}
	}
	h.mu.Lock()
	pending := h.pending
	h.pending = map[uint64]chan envelope{}
	h.mu.Unlock()
	close(exited)
	for _, ch := range pending {
		ch <- envelope{Error: &wireError{Code: CodeLoss, Message: ErrExited.Error(), Reason: estim.ReasonStoppedAnswering}}
	}
}

// readStderr relays the helper's log.
func (h *Host) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	name := func() string {
		if n := h.Name(); n != "" {
			return n
		}
		return h.Path
	}
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			h.log().Info("helper: "+line, "helper", name())
		}
	}
}

// call sends one request and decodes its answer into out (nil to discard).
func (h *Host) call(ctx context.Context, method string, params any, out any) error {
	h.mu.Lock()
	if h.pending == nil {
		h.mu.Unlock()
		return ErrExited
	}
	h.nextID++
	id := h.nextID
	ch := make(chan envelope, 1)
	h.pending[id] = ch
	exited := h.exited
	h.mu.Unlock()

	if err := h.send(envelope{ID: &id, Method: method, Params: mustJSON(params)}); err != nil {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrExited, err)
	}
	select {
	case env := <-ch:
		if env.Error != nil {
			return fromWire(env.Error)
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("helper: %s answered something unreadable: %w", method, err)
			}
		}
		return nil
	case <-exited:
		return &estim.LossError{Reason: estim.ReasonStoppedAnswering, Err: ErrExited}
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return ctx.Err()
	}
}

// notify sends a notification (no answer expected).
func (h *Host) notify(method string, params any) error {
	return h.send(envelope{Method: method, Params: mustJSON(params)})
}

func (h *Host) send(env envelope) error {
	line, err := json.Marshal(env)
	if err != nil {
		return err
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.writer == nil {
		return ErrExited
	}
	_, err = h.writer.Write(append(line, '\n'))
	return err
}

func mustJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// -- estim.Finder, Lister, Selector -------------------------------------------------

// Find asks the helper for its device and returns a Driver that forwards
// every call to it.
func (h *Host) Find(ctx context.Context) (estim.Driver, error) {
	if err := h.ensure(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", estim.ErrNoDevice, err)
	}
	var found Found
	if err := h.call(ctx, MethodFind, nil, &found); err != nil {
		return nil, err
	}
	if found.Kind == "" {
		return nil, errors.New("helper: find answered without a kind")
	}
	return &proxyDriver{host: h, found: found}, nil
}

// Describe writes what the helper can see (`estim probe`), or why it could
// not be asked.
func (h *Host) Describe(ctx context.Context, out io.Writer) error {
	if err := h.ensure(ctx); err != nil {
		fmt.Fprintf(out, "Helper %s: could not be started (%v)\n", h.Path, err)
		return err
	}
	var res describeResult
	if err := h.call(ctx, MethodDescribe, nil, &res); err != nil {
		fmt.Fprintf(out, "Helper %s: %v\n", h.Name(), err)
		return err
	}
	_, err := io.WriteString(out, res.Text)
	return err
}

// List is the helper's units in reach.
func (h *Host) List(ctx context.Context) ([]estim.Unit, error) {
	if err := h.ensure(ctx); err != nil {
		return nil, err
	}
	var res listResult
	if err := h.call(ctx, MethodList, nil, &res); err != nil {
		return nil, err
	}
	return res.Units, nil
}

// Select restricts the helper's finder to one unit (estim.Selector). The
// helper hears it on its next start too: the Host remembers the last
// selection and repeats it after a restart.
func (h *Host) Select(unit string) {
	h.mu.Lock()
	h.selection = &unit
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.ensure(ctx); err != nil {
		return
	}
	if err := h.call(ctx, MethodSelect, selectParams{Unit: unit}, nil); err != nil {
		h.log().Warn("helper: select not taken", "helper", h.Name(), "err", err)
	}
}

// -- the Driver proxy --------------------------------------------------------------

type proxyDriver struct {
	host  *Host
	found Found
	// closed is set once Close ran; a later call is a programming error the
	// helper would refuse anyway.
	closed atomic.Bool
}

func (d *proxyDriver) Kind() estim.Kind                 { return d.found.Kind }
func (d *proxyDriver) Label() string                    { return d.found.Label }
func (d *proxyDriver) Port() string                     { return d.found.Port }
func (d *proxyDriver) Capabilities() estim.Capabilities { return d.found.Capabilities }
func (d *proxyDriver) Held() bool                       { return d.found.Held }

func (d *proxyDriver) Release(ctx context.Context) error {
	return d.host.call(ctx, MethodRelease, nil, nil)
}

func (d *proxyDriver) Arm(ctx context.Context, powerMode string) error {
	return d.host.call(ctx, MethodArm, armParams{PowerMode: powerMode}, nil)
}

// RenewArm forwards the countdown renewal; a helper without one (RenewsArm
// false) is not asked.
func (d *proxyDriver) RenewArm(ctx context.Context, until time.Time) error {
	if !d.found.RenewsArm {
		return nil
	}
	return d.host.call(ctx, MethodRenewArm, renewArmParams{Until: until.UTC().Format(time.RFC3339Nano)}, nil)
}

func (d *proxyDriver) Status(ctx context.Context) (estim.Status, error) {
	var res statusResult
	if err := d.host.call(ctx, MethodStatus, nil, &res); err != nil {
		return estim.Status{}, err
	}
	return res.Status, nil
}

func (d *proxyDriver) Telemetry(ctx context.Context) (estim.Frame, error) {
	var res telemetryResult
	if err := d.host.call(ctx, MethodTelemetry, nil, &res); err != nil {
		return estim.Frame{}, err
	}
	return res.Frame, nil
}

// Execute runs the command in the helper, watching the Runtime's
// cancellation latch meanwhile: when it trips, a `cancel` notification
// tells the helper to end the ramp where it is.
func (d *proxyDriver) Execute(ctx context.Context, cmd estim.Command, levelMax int, cancelled func() bool) (estim.Result, error) {
	h := d.host
	h.mu.Lock()
	if h.pending == nil {
		h.mu.Unlock()
		return estim.Result{}, &estim.LossError{Reason: estim.ReasonStoppedAnswering, Err: ErrExited}
	}
	h.nextID++
	id := h.nextID
	ch := make(chan envelope, 1)
	h.pending[id] = ch
	exited := h.exited
	h.mu.Unlock()
	if err := h.send(envelope{ID: &id, Method: MethodExecute, Params: mustJSON(executeParams{Command: cmd, LevelMax: levelMax})}); err != nil {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return estim.Result{}, &estim.LossError{Reason: estim.ReasonStoppedAnswering, Err: fmt.Errorf("%w: %v", ErrExited, err)}
	}
	ticker := time.NewTicker(cancelPoll)
	defer ticker.Stop()
	told := false
	for {
		select {
		case env := <-ch:
			if env.Error != nil {
				return estim.Result{}, fromWire(env.Error)
			}
			var res executeResult
			if err := json.Unmarshal(env.Result, &res); err != nil {
				return estim.Result{}, fmt.Errorf("helper: execute answered something unreadable: %w", err)
			}
			return res.Result, nil
		case <-exited:
			return estim.Result{}, &estim.LossError{Reason: estim.ReasonStoppedAnswering, Err: ErrExited}
		case <-ctx.Done():
			h.mu.Lock()
			delete(h.pending, id)
			h.mu.Unlock()
			return estim.Result{}, ctx.Err()
		case <-ticker.C:
			if !told && cancelled != nil && cancelled() {
				told = true
				_ = h.notify(MethodCancel, cancelParams{ID: id})
			}
		}
	}
}

func (d *proxyDriver) Close(ctx context.Context, restore bool) error {
	if d.closed.Swap(true) {
		return nil
	}
	return d.host.call(ctx, MethodClose, closeParams{Restore: restore}, nil)
}
