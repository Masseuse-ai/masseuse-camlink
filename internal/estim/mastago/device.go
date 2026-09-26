package mastago

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/ble"
	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// Timings, as the unit's own controller app uses them.
const (
	// CommandGap is the least time between two writes.
	CommandGap = 100 * time.Millisecond
	// ReplyTimeout is how long a command may go unanswered.
	ReplyTimeout = 3 * time.Second
	// RampStep is the time between two intensity steps of a ramp.
	RampStep = 400 * time.Millisecond
	// FragmentFlush is how long a partial line is held for its remainder.
	FragmentFlush = 250 * time.Millisecond
)

// ErrLinkLost says the Bluetooth link to the unit ended.
var ErrLinkLost = errors.New("mastago: the unit's Bluetooth link ended")

// ErrTimeout says the unit did not answer a command in time.
var ErrTimeout = errors.New("mastago: the unit did not answer")

// A UnitError is a command the unit refused.
type UnitError struct {
	Command string
	Code    int
	Line    string
}

func (e *UnitError) Error() string {
	switch e.Code {
	case ErrCodeNoLoad:
		return fmt.Sprintf("mastago: %s refused: the electrodes are not on the skin", e.Command)
	case ErrCodeUnsupported:
		return fmt.Sprintf("mastago: %s refused: not supported by this unit", e.Command)
	}
	return fmt.Sprintf("mastago: %s refused: %s", e.Command, e.Line)
}

// IsNoLoad says whether err is the unit refusing an intensity because the
// electrodes are not on the skin.
func IsNoLoad(err error) bool {
	var ue *UnitError
	return errors.As(err, &ue) && ue.Code == ErrCodeNoLoad
}

// State is what the Device has learned about the unit from replies and
// from the unit's own reports. Nil pointers are unknowns.
type State struct {
	Mode            *int
	Level           *int
	Outputting      *bool
	TimerRemainingS *int
	timerReadAt     time.Time
	BatteryVolts    *float64
	LoadDetected    *bool
	// ShutdownReason is set when the unit announced it is switching
	// itself off (+QPOWD:<reason>).
	ShutdownReason *int
	// DeviceLevelChanges counts intensity changes made at the unit's own
	// buttons (+CSTR:1,<n> pushes).
	DeviceLevelChanges int
	LastHeartbeat      time.Time
}

// Device speaks the AT protocol to one unit over a ble.Conn: one command
// at a time, CommandGap between writes, replies matched to the command
// waiting for them, and everything else folded into the State.
type Device struct {
	conn ble.Conn
	log  *slog.Logger

	// Timing knobs; the constants above unless a test shortens them.
	Gap          time.Duration
	Timeout      time.Duration
	Step         time.Duration
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	rampObserver func(level int)

	cmdMu     sync.Mutex // one exchange at a time
	lastWrite time.Time

	mu       sync.Mutex // guards the fields below
	st       State
	waiter   *waiter
	rx       []byte
	rxTimer  *time.Timer
	closed   bool
	linkErr  error
	linkDone chan struct{}
}

type waiter struct {
	name  string
	query bool
	ch    chan Reply
}

// accept says whether a decoded line answers the waiting command: a named
// acknowledgment or value under the command's name, or any error. Bare OK
// lines (the tail of a query answer) and the unit's own reports are not
// answers.
func (w *waiter) accept(r Reply) bool {
	switch r.Kind {
	case ReplyError:
		return r.Name == "" || r.Name == w.name
	case ReplyAck:
		return !w.query && r.Name == w.name
	case ReplyValue:
		return w.query && r.Name == w.name && !r.Unsolicited
	}
	return false
}

// New wraps a connected unit. Listen must be called before any command.
func New(conn ble.Conn, log *slog.Logger) *Device {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Device{
		conn: conn, log: log,
		Gap: CommandGap, Timeout: ReplyTimeout, Step: RampStep,
		Now:      time.Now,
		Sleep:    sleepCtx,
		linkDone: make(chan struct{}),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ID is the system's identifier for the unit.
func (d *Device) ID() string { return d.conn.ID() }

// Name is the unit's advertised name ("MASTOGO G-12AB").
func (d *Device) Name() string { return d.conn.Name() }

// Listen subscribes to the unit's replies and watches the link.
func (d *Device) Listen(ctx context.Context) error {
	if err := d.conn.Subscribe(ctx, ServiceUUID, NotifyUUID, d.onNotify); err != nil {
		return fmt.Errorf("mastago: subscribing to replies: %w", err)
	}
	go d.watchLink()
	return nil
}

func (d *Device) watchLink() {
	<-d.conn.Disconnected()
	d.mu.Lock()
	d.closed = true
	d.linkErr = d.linkErrLocked()
	w := d.waiter
	d.waiter = nil
	d.mu.Unlock()
	if w != nil {
		close(w.ch)
	}
	close(d.linkDone)
}

// linkErrLocked is why the link ended: the unit's own shutdown report when
// it sent one before dropping the link, a bare ErrLinkLost otherwise. The
// caller holds mu.
func (d *Device) linkErrLocked() error {
	if d.linkErr != nil {
		return d.linkErr
	}
	if d.st.ShutdownReason != nil {
		code := *d.st.ShutdownReason
		return &estim.LossError{
			Reason: LossReason(code),
			Err:    fmt.Errorf("%w: the unit switched itself off (%s)", ErrLinkLost, ShutdownReason(code)),
		}
	}
	return &estim.LossError{Reason: estim.ReasonLinkLost, Err: ErrLinkLost}
}

// LinkError is why the link ended, or nil while it is up.
func (d *Device) LinkError() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.linkErr
}

// State is a snapshot of what is known about the unit.
func (d *Device) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st
}

// -- receive ---------------------------------------------------------------------

func (d *Device) onNotify(b []byte) {
	d.mu.Lock()
	d.rx = append(d.rx, b...)
	var lines [][]byte
	for {
		i := indexByte(d.rx, '\n')
		if i < 0 {
			break
		}
		lines = append(lines, append([]byte(nil), d.rx[:i]...))
		d.rx = d.rx[i+1:]
	}
	if d.rxTimer != nil {
		d.rxTimer.Stop()
		d.rxTimer = nil
	}
	if len(d.rx) > 0 {
		d.rxTimer = time.AfterFunc(FragmentFlush, d.flushFragment)
	}
	d.mu.Unlock()
	for _, l := range lines {
		d.handleLine(l)
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (d *Device) flushFragment() {
	d.mu.Lock()
	frag := d.rx
	d.rx = nil
	d.rxTimer = nil
	d.mu.Unlock()
	if len(frag) > 0 {
		d.handleLine(frag)
	}
}

func (d *Device) handleLine(line []byte) {
	r := DecodeReply(line)
	if r.Raw == "" {
		return
	}
	d.log.Debug("mastago: <<", "line", r.Raw)
	d.mu.Lock()
	d.fold(r)
	w := d.waiter
	if w != nil && w.accept(r) {
		d.waiter = nil
	} else {
		w = nil
	}
	d.mu.Unlock()
	if w != nil {
		w.ch <- r
	}
}

// fold updates the State from any line, answer or report; the caller
// holds mu.
func (d *Device) fold(r Reply) {
	switch r.Kind {
	case ReplyHeartbeat:
		d.st.LastHeartbeat = d.Now()
		return
	case ReplyError:
		if r.Code == ErrCodeNoLoad {
			d.st.LoadDetected = estim.Bool(false)
		}
		return
	case ReplyValue:
	default:
		return
	}
	switch r.Name {
	case nameLevel:
		if n, ok := LastInt(r.Value); ok {
			if len(Ints(r.Value)) > 1 || r.Unsolicited {
				// "+CSTR:1,<n>": the unit's own buttons.
				d.st.DeviceLevelChanges++
			}
			d.st.Level = estim.Int(n)
		}
	case nameMode:
		if n, ok := LastInt(r.Value); ok {
			d.st.Mode = estim.Int(n)
		}
	case nameTimer:
		if s, ok := ParseHHMMSS(r.Value); ok {
			d.st.TimerRemainingS = estim.Int(s)
			d.st.timerReadAt = d.Now()
		}
	case namePaused:
		// "+QPOWP:0" outputting, "+QPOWP:1" paused.
		if n, ok := LastInt(r.Value); ok {
			d.st.Outputting = estim.Bool(n == 0)
		}
	case nameStarted:
		if n, ok := LastInt(r.Value); ok {
			d.st.Outputting = estim.Bool(n != 0)
		}
	case nameBattery:
		if v, ok := ParseVolts(r.Value); ok {
			d.st.BatteryVolts = &v
		}
	case nameLoad:
		if n, ok := LastInt(r.Value); ok {
			d.st.LoadDetected = estim.Bool(n != 0)
		}
	case nameTimerEnd:
		d.st.TimerRemainingS = estim.Int(0)
		d.st.timerReadAt = d.Now()
		d.st.Outputting = estim.Bool(false)
	case nameShutdown:
		code := -1
		if n, ok := LastInt(r.Value); ok {
			code = n
		}
		d.st.ShutdownReason = estim.Int(code)
	}
}

// -- send -------------------------------------------------------------------------

// exchange writes one command and waits for its answer. query says an
// answer carries a value rather than an acknowledgment.
func (d *Device) exchange(ctx context.Context, cmd string, query bool) (Reply, error) {
	d.cmdMu.Lock()
	defer d.cmdMu.Unlock()
	d.mu.Lock()
	if d.closed {
		err := d.linkErrLocked()
		d.mu.Unlock()
		return Reply{}, err
	}
	w := &waiter{name: CommandName(cmd), query: query, ch: make(chan Reply, 1)}
	d.waiter = w
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.waiter == w {
			d.waiter = nil
		}
		d.mu.Unlock()
	}()
	if gap := d.Gap - d.Now().Sub(d.lastWrite); gap > 0 {
		if err := d.Sleep(ctx, gap); err != nil {
			return Reply{}, err
		}
	}
	d.log.Debug("mastago: >>", "cmd", cmd)
	err := d.conn.Write(ctx, ServiceUUID, WriteUUID, Encode(cmd), true)
	d.lastWrite = d.Now()
	if err != nil {
		if errors.Is(err, ble.ErrDisconnected) {
			return Reply{}, d.lost()
		}
		return Reply{}, fmt.Errorf("mastago: writing %s: %w", cmd, err)
	}
	timeout := time.NewTimer(d.Timeout)
	defer timeout.Stop()
	select {
	case r, ok := <-w.ch:
		if !ok {
			return Reply{}, d.lost()
		}
		if r.Kind == ReplyError {
			return r, &UnitError{Command: cmd, Code: r.Code, Line: r.Raw}
		}
		return r, nil
	case <-d.linkDone:
		return Reply{}, d.lost()
	case <-timeout.C:
		return Reply{}, fmt.Errorf("%w: %s", ErrTimeout, cmd)
	case <-ctx.Done():
		return Reply{}, ctx.Err()
	}
}

func (d *Device) lost() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.linkErrLocked()
}

func (d *Device) set(ctx context.Context, cmd string) error {
	_, err := d.exchange(ctx, cmd, false)
	return err
}

func (d *Device) queryInt(ctx context.Context, cmd string) (int, error) {
	r, err := d.exchange(ctx, cmd, true)
	if err != nil {
		return 0, err
	}
	n, ok := LastInt(r.Value)
	if !ok {
		return 0, fmt.Errorf("mastago: %s answered %q", cmd, r.Raw)
	}
	return n, nil
}

// -- queries ------------------------------------------------------------------

// Mode is the selected program.
func (d *Device) Mode(ctx context.Context) (int, error) { return d.queryInt(ctx, QueryMode) }

// Level is the intensity.
func (d *Device) Level(ctx context.Context) (int, error) { return d.queryInt(ctx, QueryLevel) }

// Outputting says whether the unit is delivering its program (false:
// paused).
func (d *Device) Outputting(ctx context.Context) (bool, error) {
	n, err := d.queryInt(ctx, QueryPaused)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// Timer is what is left of the countdown, in seconds.
func (d *Device) Timer(ctx context.Context) (int, error) {
	r, err := d.exchange(ctx, QueryTimer, true)
	if err != nil {
		return 0, err
	}
	s, ok := ParseHHMMSS(r.Value)
	if !ok {
		return 0, fmt.Errorf("mastago: %s answered %q", QueryTimer, r.Raw)
	}
	return s, nil
}

// Battery is the cell voltage.
func (d *Device) Battery(ctx context.Context) (float64, error) {
	r, err := d.exchange(ctx, QueryBattery, true)
	if err != nil {
		return 0, err
	}
	v, ok := ParseVolts(r.Value)
	if !ok {
		return 0, fmt.Errorf("mastago: %s answered %q", QueryBattery, r.Raw)
	}
	return v, nil
}

// Load says whether the electrodes are on the skin.
func (d *Device) Load(ctx context.Context) (bool, error) {
	n, err := d.queryInt(ctx, QueryLoad)
	if err != nil {
		return false, err
	}
	return n != 0, nil
}

// -- settings -----------------------------------------------------------------

// SetMode selects program m. Output is paused first, as the unit's own
// controller does; the caller brings the intensity back.
func (d *Device) SetMode(ctx context.Context, m int) error {
	if !ModeAllowed(m) {
		return fmt.Errorf("mastago: program must be 0..%d", ModeMax)
	}
	if err := d.Pause(ctx); err != nil {
		return err
	}
	if err := d.set(ctx, CmdSetMode(m)); err != nil {
		return err
	}
	d.mu.Lock()
	d.st.Mode = estim.Int(m)
	d.mu.Unlock()
	return nil
}

// SetLevelRaw writes one intensity without ramping. The unit refuses it
// (IsNoLoad) when the electrodes are not on the skin, and keeps its
// previous intensity then.
func (d *Device) SetLevelRaw(ctx context.Context, n int) error {
	if n < 0 || n > LevelMax {
		return fmt.Errorf("mastago: intensity must be 0..%d", LevelMax)
	}
	if err := d.set(ctx, CmdSetLevel(n)); err != nil {
		return err
	}
	d.mu.Lock()
	d.st.Level = estim.Int(n)
	d.mu.Unlock()
	return nil
}

// SetTimer arms the countdown for seconds (clamped to the unit's range).
// The countdown runs whether or not the unit is outputting; at zero the
// unit stops its output and reports +CLKEND.
func (d *Device) SetTimer(ctx context.Context, seconds int) error {
	if err := d.set(ctx, CmdSetTimer(seconds)); err != nil {
		return err
	}
	d.mu.Lock()
	d.st.TimerRemainingS = estim.Int(max(TimerMinS, min(TimerMaxS, seconds)))
	d.st.timerReadAt = d.Now()
	d.mu.Unlock()
	return nil
}

// Resume starts output. Acknowledged even with no load; the unit stays
// paused until an intensity above zero is accepted.
func (d *Device) Resume(ctx context.Context) error {
	if err := d.set(ctx, CmdStart); err != nil {
		return err
	}
	d.mu.Lock()
	d.st.Outputting = estim.Bool(true)
	d.mu.Unlock()
	return nil
}

// Pause stops output; the unit keeps its program and intensity.
func (d *Device) Pause(ctx context.Context) error {
	if err := d.set(ctx, CmdPause); err != nil {
		return err
	}
	d.mu.Lock()
	d.st.Outputting = estim.Bool(false)
	d.mu.Unlock()
	return nil
}

// -- compound operations --------------------------------------------------------

// Ramp takes the intensity to target and leaves the unit outputting at it
// (or paused at zero): upward one step per Step with a read-back of each,
// output started before the first step above zero; downward in one write.
// A paused unit holding the target already (after a program change, or
// its own countdown ending) is resumed. It returns the intensity the unit
// ended at. A refusal for missing electrode contact (IsNoLoad) stops the
// ramp where it is; cancelled, polled between steps, preempts it with
// estim.ErrCancelled.
func (d *Device) Ramp(ctx context.Context, target int, cancelled func() bool) (int, error) {
	if target < 0 || target > LevelMax {
		return 0, fmt.Errorf("mastago: intensity must be 0..%d", LevelMax)
	}
	current, err := d.Level(ctx)
	if err != nil {
		return 0, err
	}
	switch {
	case target == current:
		if target == 0 {
			return 0, d.Pause(ctx)
		}
		return current, d.ensureOutputting(ctx)
	case target < current:
		if target == 0 {
			return 0, d.zero(ctx)
		}
		if err := d.SetLevelRaw(ctx, target); err != nil {
			if IsNoLoad(err) {
				// Nothing is being delivered without contact; stop output
				// so nothing resumes at the old intensity when contact
				// returns.
				if perr := d.Pause(ctx); perr != nil {
					return current, perr
				}
			}
			return current, err
		}
		got, err := d.readBack(ctx, target)
		if err != nil {
			return got, err
		}
		return got, d.ensureOutputting(ctx)
	}
	if cancelled != nil && cancelled() {
		return current, estim.ErrCancelled
	}
	if err := d.ensureOutputting(ctx); err != nil {
		return current, err
	}
	applied := current
	for n := current + 1; n <= target; n++ {
		if n > current+1 {
			if err := d.Sleep(ctx, d.Step); err != nil {
				return applied, err
			}
		}
		if cancelled != nil && cancelled() {
			return applied, estim.ErrCancelled
		}
		if err := d.SetLevelRaw(ctx, n); err != nil {
			return applied, err
		}
		got, err := d.readBack(ctx, n)
		if err != nil {
			return applied, err
		}
		applied = got
		if d.rampObserver != nil {
			d.rampObserver(applied)
		}
	}
	return applied, nil
}

// ensureOutputting resumes a paused unit.
func (d *Device) ensureOutputting(ctx context.Context) error {
	outputting, err := d.Outputting(ctx)
	if err != nil {
		return err
	}
	if outputting {
		return nil
	}
	return d.Resume(ctx)
}

// readBack confirms the intensity the unit holds after a write.
func (d *Device) readBack(ctx context.Context, want int) (int, error) {
	got, err := d.Level(ctx)
	if err != nil {
		return 0, err
	}
	if got != want {
		return got, fmt.Errorf("mastago: intensity read back as %d after setting %d", got, want)
	}
	return got, nil
}

// zero pauses output and sets the intensity to zero. Without electrode
// contact the unit refuses even zero; that is accepted once it confirms
// it is paused, since a paused unit delivers nothing.
func (d *Device) zero(ctx context.Context) error {
	if err := d.Pause(ctx); err != nil {
		return err
	}
	err := d.SetLevelRaw(ctx, 0)
	if err == nil {
		return nil
	}
	if !IsNoLoad(err) {
		return err
	}
	outputting, qerr := d.Outputting(ctx)
	if qerr != nil {
		return qerr
	}
	if outputting {
		return fmt.Errorf("mastago: the unit refused intensity zero and is still outputting: %w", err)
	}
	d.log.Debug("mastago: intensity zero refused without electrode contact; the unit is paused")
	return nil
}

// Release fails closed: output paused, intensity zero. It reports what it
// could confirm.
func (d *Device) Release(ctx context.Context) error {
	if err := d.zero(ctx); err != nil {
		return err
	}
	outputting, err := d.Outputting(ctx)
	if err != nil {
		return err
	}
	if outputting {
		return errors.New("mastago: the unit is still outputting after a release")
	}
	return nil
}

// Status reads the unit in full: program, intensity, output state,
// countdown, battery, electrode contact.
func (d *Device) Status(ctx context.Context) (estim.Status, error) {
	mode, err := d.Mode(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	level, err := d.Level(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	outputting, err := d.Outputting(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	timer, err := d.Timer(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	volts, err := d.Battery(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	load, err := d.Load(ctx)
	if err != nil {
		return estim.Status{}, err
	}
	s := estim.Status{
		Connected:       true,
		Port:            estim.String(d.ID()),
		Mode:            estim.Int(mode),
		LevelA:          estim.Int(level),
		LevelB:          estim.Int(0),
		Power:           estim.String(estim.PowerModeNormal),
		BatteryPercent:  estim.Int(BatteryPercent(volts)),
		ADCOverride:     estim.Bool(false),
		MAPotOverride:   estim.Bool(false),
		Outputting:      estim.Bool(outputting),
		LoadDetected:    estim.Bool(load),
		TimerRemainingS: estim.Int(timer),
	}
	return s, nil
}

// Telemetry is a compact sample: intensity and output state read fresh,
// the program, electrode contact and countdown from the last full reading
// (the countdown counted down locally while output runs).
func (d *Device) Telemetry(ctx context.Context) (estim.Frame, error) {
	level, err := d.Level(ctx)
	if err != nil {
		return estim.Frame{}, err
	}
	outputting, err := d.Outputting(ctx)
	if err != nil {
		return estim.Frame{}, err
	}
	d.mu.Lock()
	st := d.st
	now := d.Now()
	d.mu.Unlock()
	f := estim.Frame{
		LevelA:       estim.Int(level),
		Outputting:   estim.Bool(outputting),
		LoadDetected: st.LoadDetected,
	}
	if st.Mode != nil {
		f.Mode = estim.Int(*st.Mode)
	}
	if st.TimerRemainingS != nil {
		remaining := *st.TimerRemainingS
		if !st.timerReadAt.IsZero() {
			remaining = max(0, remaining-int(now.Sub(st.timerReadAt).Seconds()))
		}
		f.TimerRemainingS = estim.Int(remaining)
	}
	return f, nil
}

// Close releases the unit when restore is set and ends the link.
func (d *Device) Close(ctx context.Context, restore bool) error {
	var err error
	if restore && d.LinkError() == nil {
		err = d.Release(ctx)
	}
	if cerr := d.conn.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
