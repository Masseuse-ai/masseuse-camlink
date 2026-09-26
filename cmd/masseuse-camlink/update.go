package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/FemLed/masseuse-camlink/internal/attest"
	"github.com/FemLed/masseuse-camlink/internal/buildinfo"
	"github.com/FemLed/masseuse-camlink/internal/update"
)

// The program keeps itself current (internal/update, README "Updates"): a
// look at the latest release soon after it starts and every six hours
// after, the download verified the way VERIFY.md says (the release
// workflow's signature over the checksums, the artifact's hash, its
// provenance, and on a Mac the application's own signature and
// notarization), and the new version put in place and started when no
// session is using this computer. Nothing is asked; what happened is
// said in one line. -no-update turns it off, as does a build that is not
// a release, a location this user cannot write, or the container image.

var noUpdate = flag.Bool("no-update", strings.EqualFold(envOr("MASSEUSE_CAMLINK_UPDATE", ""), "off"),
	"do not look for or install newer versions of this program (MASSEUSE_CAMLINK_UPDATE=off does the same)")

// Where the releases are; a stand-in in tests and in the release
// workflow's own check.
var updateReleasesURL = envOr("MASSEUSE_CAMLINK_UPDATE_URL", update.DefaultReleasesURL)

// The version this program takes itself to be, when told (the release
// workflow's check and a person trying the update path run a program as
// an older release than it is: the latest release is then downloaded,
// verified and installed over it, the whole path exercised against real
// signatures). It changes nothing about what is trusted: only a verified
// release is ever installed.
var updateAs = os.Getenv("MASSEUSE_CAMLINK_UPDATE_AS")

const (
	updateFirstAfter = 15 * time.Second
	updateEvery      = 6 * time.Hour
	updateIdlePoll   = 30 * time.Second
	// How long the handoff before a restart may take in all (the unit
	// runtime's close alone allows itself 20 s): past it the restart goes
	// ahead with what is still held (updater.handOver).
	updateHandoffTimeout = 20 * time.Second
)

// updater runs the update loop beside the manager.
type updater struct {
	client *update.Client
	inst   *update.Installer
	state  *update.State
	log    *slog.Logger
	// out is the console.
	out func(format string, args ...any)
	// idle says whether an update may be applied now: no session is
	// using this computer.
	idle func() bool
	// handoff stops what the running program holds (the unit released and
	// its helpers closed, the camera off, the instance lock and the awake
	// hold let go) right before the new program takes over.
	handoff func()
	// args are what the new program is started with (this run's).
	args []string
	env  []string
	now  func() time.Time
	// restart hands the process over (inst.Restart); exit ends this one.
	// Both are replaced in tests.
	restart func(args, env []string) error
	exit    func(code int)

	firstAfter, every, idlePoll time.Duration
	// handoffTimeout bounds handoff (handOver).
	handoffTimeout time.Duration
	// warnedAt keeps the console to one line a day per failure kind.
	warnedAt map[string]time.Time
}

// newUpdater is the updater for this run, or nil with the reason it is
// off, printed by the caller.
func newUpdater(stateDir string, log *slog.Logger, out func(string, ...any)) (*updater, string) {
	if *noUpdate {
		return nil, "Updates are off (-no-update)."
	}
	current := buildinfo.Version()
	if updateAs != "" && isReleaseBuild(updateAs) {
		current = updateAs
	}
	if !isReleaseBuild(current) {
		// A build from a working tree ("(devel)", or Go's pseudo-version
		// with a commit and "+dirty" in it): nothing to compare with, and
		// nothing to be replaced by a release; said nowhere.
		return nil, ""
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	in := update.Detect(exe, runtime.GOOS, runtime.GOARCH)
	if in.GOARCH == "arm" {
		if arm := update.CurrentGOARM(); arm != "" {
			in.GOARM = arm
		}
	}
	if err := update.CheckInPlace(in); err != nil {
		var inPlace *update.InPlaceError
		if errors.As(err, &inPlace) {
			return nil, "Updates are off: " + inPlace.Reason + "."
		}
		return nil, "Updates are off: " + err.Error() + "."
	}
	u := &updater{
		client: &update.Client{
			ReleasesURL: updateReleasesURL,
			Verifier:    update.NewVerifier(stateDir, log),
			Current:     current,
			Install:     in,
			StateDir:    stateDir,
			Logger:      log,
		},
		// A bundle moved aside goes under the state directory, so the
		// Applications folder shows one application (update.PreviousName).
		inst:           &update.Installer{Install: in, Logger: log, Aside: filepath.Join(stateDir, "previous")},
		state:          update.LoadState(stateDir),
		log:            log,
		out:            out,
		idle:           func() bool { return true },
		handoff:        func() {},
		args:           os.Args[1:],
		env:            withoutEnv(os.Environ(), "MASSEUSE_CAMLINK_UPDATE_AS"),
		now:            time.Now,
		firstAfter:     updateFirstAfter,
		every:          updateEvery,
		idlePoll:       updateIdlePoll,
		handoffTimeout: updateHandoffTimeout,
		warnedAt:       map[string]time.Time{},
		exit:           exit,
	}
	u.restart = u.inst.Restart
	return u, ""
}

// withoutEnv is env without the variable named: the new program must not
// inherit the pretence of being an older release, or it would update
// itself again at once.
func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// isReleaseBuild says whether v is exactly a release tag (vX.Y.Z): what a
// release build reports, and what the latest release is compared with.
// Pre-releases, pseudo-versions and dirty builds are not.
func isReleaseBuild(v string) bool {
	return attest.ValidRelease(v) && !strings.ContainsAny(v, "-+")
}

// stateDir is where the updater's state and downloads live.
func (u *updater) stateDir() string { return u.client.StateDir }

// save writes the state; a failure to is logged, never fatal.
func (u *updater) save() {
	if err := u.state.Save(u.stateDir()); err != nil {
		u.log.Warn("update: could not save the update state", "err", err)
	}
}

// warn prints one console line per kind per day (every time when there is
// no memory of them, the `update` command), and logs every time.
func (u *updater) warn(kind, format string, args ...any) {
	u.log.Warn("update: " + fmt.Sprintf(format, args...))
	if u.warnedAt != nil {
		if at, ok := u.warnedAt[kind]; ok && u.now().Sub(at) < 24*time.Hour {
			return
		}
		u.warnedAt[kind] = u.now()
	}
	u.out(format+"\n", args...)
}

// announce says what the last update did: the new program prints it once.
func (u *updater) announce() {
	if u.state.Installed == "" {
		return
	}
	if u.state.Installed == u.client.Current {
		u.out("Updated to %s.\n", u.state.Installed)
	} else {
		// The update was put in place but this is not it: the person
		// started the previous, or the swap did not take.
		u.log.Warn("update: an update was installed but another version is running", "installed", u.state.Installed, "running", u.client.Current)
	}
	u.state.Installed, u.state.InstalledAt = "", time.Time{}
	u.save()
}

// proven is called once the new program has done its job (its first
// successful connection to the service): the previous install goes.
// Windows may still hold the old program's files open for a moment.
func (u *updater) proven() {
	if u.state.Previous == "" {
		return
	}
	go func() {
		for attempt := 0; attempt < 10; attempt++ {
			err := u.state.RemovePrevious()
			if err == nil || errors.Is(err, update.ErrNoPrevious) {
				u.save()
				u.log.Info("update: the previous version is removed")
				return
			}
			time.Sleep(3 * time.Second)
		}
		u.log.Warn("update: the previous version could not be removed", "path", u.state.Previous)
	}()
}

// applyStaged puts in place, at launch, an update an earlier run had
// downloaded and verified but not installed (it was never idle, or ended
// first): the release is looked up again so the file is checked against
// the signed list and its provenance once more, staged, and applied at
// once, the person having just opened the program. False when there is
// nothing staged, the network is not there (the loop will get to it), or
// something refused. True only on Windows, where the new program has been
// started and this one must end; elsewhere the new program has replaced
// this one by then.
func (u *updater) applyStaged(ctx context.Context) bool {
	st := u.state.Staged
	if st == nil {
		return false
	}
	if _, err := os.Stat(st.Path); err != nil {
		u.state.Staged = nil
		u.save()
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rel, err := u.client.Check(cctx)
	if err != nil {
		if errors.Is(err, update.ErrUpToDate) {
			u.state.Staged = nil
			u.client.Prune("")
			u.save()
		}
		return false
	}
	if rel.Tag != st.Tag {
		return false // a newer one since; the loop takes it
	}
	staged, err := u.client.Download(cctx, rel)
	if err != nil {
		u.refused(rel.Tag, err)
		return false
	}
	root, err := u.inst.Stage(cctx, staged, filepath.Join(u.client.UpdatesDir(), staged.Tag, "staged"))
	if err != nil {
		u.refused(rel.Tag, err)
		return false
	}
	return u.apply(ctx, staged, root)
}

// run is the loop: the first look after a moment, then every so often,
// and a staged update applied as soon as the computer is idle.
func (u *updater) run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(u.firstAfter):
	}
	for {
		if restarted := u.once(ctx); restarted {
			return
		}
		jitter := time.Duration(rand.Int64N(int64(u.every / 10)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(u.every + jitter):
		}
	}
}

// once is one pass: find, download, verify, stage, wait for idle, apply.
// It returns true when the process has been handed over (it never returns
// in that case on Unix, where the new program replaces this one).
func (u *updater) once(ctx context.Context) bool {
	staged, root, ok := u.prepare(ctx)
	if !ok {
		return false
	}
	if !u.idle() {
		u.out("Update: %s %s downloaded and verified; installing when the session ends.\n", appName, staged.Tag)
		for !u.idle() {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(u.idlePoll):
			}
		}
	}
	return u.apply(ctx, staged, root)
}

// prepare finds a newer release and brings it to the point of being
// installable: downloaded, verified, staged, the new program's --version
// answered. False when there is nothing to do or something refused.
func (u *updater) prepare(ctx context.Context) (*update.Staged, string, bool) {
	u.state.LastCheck = u.now()
	rel, err := u.client.Check(ctx)
	switch {
	case errors.Is(err, update.ErrUpToDate):
		u.log.Debug("update: up to date", "version", u.client.Current)
		u.client.Prune("")
		u.state.Staged = nil
		u.save()
		return nil, "", false
	case err != nil:
		if strings.Contains(err.Error(), "do not verify") {
			u.warn("verify", "Update check: the latest release did not verify; staying on %s.", u.client.Current)
		} else {
			u.log.Debug("update: could not check for a newer version", "err", err)
		}
		u.save()
		return nil, "", false
	}
	if u.state.FailedRecently(rel.Tag, u.now()) {
		u.log.Debug("update: a recent failure with this release; not tried again yet", "tag", rel.Tag)
		return nil, "", false
	}
	u.log.Info("update: a newer release", "tag", rel.Tag, "current", u.client.Current, "signedBy", rel.Signature.Identity)
	u.client.Prune(rel.Tag)
	dctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	staged, err := u.client.Download(dctx, rel)
	cancel()
	if err != nil {
		u.refused(rel.Tag, err)
		return nil, "", false
	}
	u.state.Staged = staged
	u.save()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	root, err := u.inst.Stage(sctx, staged, filepath.Join(u.client.UpdatesDir(), staged.Tag, "staged"))
	cancel()
	if err != nil {
		u.refused(rel.Tag, err)
		return nil, "", false
	}
	return staged, root, true
}

// refused records a release that could not be taken and says so once.
func (u *updater) refused(tag string, err error) {
	u.state.MarkFailed(tag, u.now())
	u.state.Staged = nil
	u.save()
	var space *update.SpaceError
	switch {
	case errors.As(err, &space):
		u.warn("space", "Not enough free space for the update to %s (%d MB needed, %d MB free in %s).", tag, space.Need>>20, space.Free>>20, space.Dir)
	case ctxErr(err):
		u.log.Debug("update: interrupted", "tag", tag, "err", err)
	case isNetwork(err):
		u.log.Debug("update: could not download", "tag", tag, "err", err)
	default:
		u.warn("refused", "Update %s did not verify; staying on %s. (%v)", tag, u.client.Current, firstLine(err))
	}
}

func ctxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isNetwork says whether the failure was reaching the release, not what
// it contained.
func isNetwork(err error) bool {
	s := err.Error()
	return strings.Contains(s, "fetching") || strings.Contains(s, "HTTP ") || strings.Contains(s, "downloading")
}

func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

// apply swaps the staged install in and hands the process over to it. The
// swap comes first (renames; the running program is untouched by them),
// then what this program holds is let go, then the new program starts.
// If it will not start, the swap is undone and this program ends, saying
// so: what it held is gone by then.
func (u *updater) apply(ctx context.Context, staged *update.Staged, root string) bool {
	u.out("Updating to %s; back in a moment.\n", staged.Tag)
	previous, err := u.inst.Swap(root)
	if err != nil {
		u.refused(staged.Tag, err)
		u.warn("swap", "Update %s could not be put in place: %v", staged.Tag, firstLine(err))
		return false
	}
	u.state.Previous, u.state.From = previous, u.client.Current
	u.state.Installed, u.state.InstalledAt = staged.Tag, u.now()
	u.state.Staged = nil
	u.save()
	update.Cleanup(filepath.Join(u.client.UpdatesDir(), staged.Tag))
	u.log.Info("update: installed; restarting", "tag", staged.Tag, "previous", previous)
	u.out("Handing over: camera off, unit released, restarting.\n")
	u.handOver()
	err = u.restart(u.args, u.env)
	switch {
	case errors.Is(err, update.ErrRelaunch):
		// The shell that started this program starts the new one in the
		// same window on this exit code (desktop.go commandScript).
		u.log.Info("update: ending for the shell to start the new program", "code", update.RelaunchExitCode)
		u.exit(update.RelaunchExitCode)
		return true
	case errors.Is(err, update.ErrStartedApart):
		u.out("%s %s is starting in a window of its own; this one is done.\n", appName, staged.Tag)
		u.exit(0)
		return true
	case err != nil:
		// Put the old program back and stop: the camera, the unit and the
		// lock were let go for the handoff.
		u.undo(previous)
		u.state.MarkFailed(staged.Tag, u.now())
		u.save()
		u.out("The new version could not be started (%v); the previous one is back. Open %s again.\n", firstLine(err), appName)
		u.exit(1)
		return true
	}
	// Windows: the new program has the console; this one ends.
	u.exit(0)
	return true
}

// handOver runs the handoff (tunnels closed, camera off, unit released,
// the awake hold and the instance lock let go) under a deadline: a step
// that does not finish (a unit runtime that will not close, a camera
// process that will not end) is logged and left behind, and the restart
// goes ahead. The new program takes the camera and the unit for itself,
// and what the old one left ends with it.
func (u *updater) handOver() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.handoff()
	}()
	select {
	case <-done:
		u.log.Info("update: handed over")
	case <-time.After(u.handoffTimeout):
		u.log.Warn("update: the handoff did not finish in time; restarting anyway", "after", u.handoffTimeout)
	}
}

// undo puts the previous install back after a failed restart: the bundle
// by renaming it back, the files of the other layouts one by one.
func (u *updater) undo(previous string) {
	root := u.inst.Install.Root
	if u.inst.Install.Layout == update.LayoutBundle {
		_ = os.RemoveAll(root)
		_ = os.Rename(previous, root)
	} else if entries, err := os.ReadDir(previous); err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(root, e.Name()))
			_ = os.Rename(filepath.Join(previous, e.Name()), filepath.Join(root, e.Name()))
		}
		_ = os.RemoveAll(previous)
	}
	u.state.Previous, u.state.From, u.state.Installed = "", "", ""
}

// updateNow is the `update` command: one pass, applied at once, the new
// program not started (the caller does). Exit code 0 when up to date or
// updated, 1 on a failure, 2 when updates are off for this install.
func updateNow(ctx context.Context, stateDir string, log *slog.Logger) int {
	out := func(format string, args ...any) { fmt.Printf(format, args...) }
	release, err := lockInstance(stateDir)
	if err != nil {
		if errors.Is(err, errAlreadyRunning) {
			fmt.Fprintln(os.Stderr, appName+" is running; it updates itself when it is idle. Close it first to update by hand.")
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		return 1
	}
	defer release()
	u, why := newUpdater(stateDir, log, out)
	if u == nil {
		if why == "" {
			why = "Updates are off: this is not a release build."
		}
		fmt.Fprintln(os.Stderr, why)
		return 2
	}
	u.warnedAt = nil // every line, this once
	rel, err := u.client.Check(ctx)
	switch {
	case errors.Is(err, update.ErrUpToDate):
		fmt.Printf("Up to date: %s is the latest release.\n", u.client.Current)
		return 0
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("A newer release: %s (running %s), signed by %s.\n", rel.Tag, u.client.Current, rel.Signature.Identity)
	staged, err := u.client.Download(ctx, rel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("Downloaded and verified %s (%d MB); provenance by %s.\n", staged.Name, staged.Size>>20, staged.Provenance.Identity)
	root, err := u.inst.Stage(ctx, staged, filepath.Join(u.client.UpdatesDir(), staged.Tag, "staged"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	previous, err := u.inst.Swap(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	u.state.Previous, u.state.From = previous, u.client.Current
	u.state.Installed, u.state.InstalledAt = staged.Tag, u.now()
	u.state.Staged = nil
	u.save()
	update.Cleanup(filepath.Join(u.client.UpdatesDir(), staged.Tag))
	fmt.Printf("Updated to %s. Start %s again to run it; the previous version is kept at %s until then.\n", staged.Tag, appName, previous)
	return 0
}
