package capture

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Kind is a device's kind.
type Kind string

const (
	// Video is a camera.
	Video Kind = "video"
	// Audio is a microphone.
	Audio Kind = "audio"
)

// Device is a camera or microphone ffmpeg can open.
type Device struct {
	Kind Kind
	// ID is what ffmpeg's input needs: the avfoundation index, the dshow
	// name, the v4l2 node or the PulseAudio source name.
	ID string
	// Name is what the person sees.
	Name string
}

// ErrNoFFmpeg is returned when ffmpeg cannot be found.
var ErrNoFFmpeg = errors.New("capture: ffmpeg not found")

// FindFFmpeg resolves the ffmpeg executable: path as given, or "ffmpeg" on
// PATH plus the usual install locations.
func FindFFmpeg(path string) (string, error) {
	if path != "" {
		if p, err := exec.LookPath(path); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("%w at %s", ErrNoFFmpeg, path)
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p, nil
	}
	candidates := []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		candidates = append(candidates, filepath.Join(pf, "ffmpeg", "bin", "ffmpeg.exe"))
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates, filepath.Join(la, "Microsoft", "WinGet", "Links", "ffmpeg.exe"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", ErrNoFFmpeg
}

// InstallHint says how to install ffmpeg on this system.
func InstallHint() string {
	return installHint(runtime.GOOS)
}

func installHint(goos string) string {
	switch goos {
	case "darwin":
		return "install ffmpeg with Homebrew: brew install ffmpeg"
	case "windows":
		return "install ffmpeg with winget: winget install Gyan.FFmpeg (then open a new terminal)"
	default:
		return "install ffmpeg with your package manager, for example: sudo apt install ffmpeg"
	}
}

// Devices lists the cameras and microphones ffmpeg can open on this system.
func Devices(ctx context.Context, ffmpeg string) ([]Device, error) {
	return devices(ctx, runtime.GOOS, ffmpeg, "/sys/class/video4linux")
}

func devices(ctx context.Context, goos, ffmpeg, v4l2Root string) ([]Device, error) {
	switch goos {
	case "darwin":
		out, _ := run(ctx, ffmpeg, "-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "")
		return parseAVFoundation(out), nil
	case "windows":
		out, _ := run(ctx, ffmpeg, "-hide_banner", "-f", "dshow", "-list_devices", "true", "-i", "dummy")
		return parseDShow(out), nil
	default:
		devs, err := readV4L2(v4l2Root)
		if err != nil {
			return nil, err
		}
		if out, err := run(ctx, "pactl", "list", "sources"); err == nil {
			devs = append(devs, parsePactlSources(out)...)
		}
		return devs, nil
	}
}

// run returns a command's combined output; ffmpeg's device listings end in
// a deliberate "error opening input", so the exit status is not an error.
func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", err
	}
	return string(out), nil
}

var (
	avfSection = regexp.MustCompile(`AVFoundation (video|audio) devices:`)
	avfDevice  = regexp.MustCompile(`\[(\d+)\] (.+)$`)
	dshowLine  = regexp.MustCompile(`"(.+)" \((video|audio)(?:, \w+)?\)\s*$`)
)

// parseAVFoundation reads `ffmpeg -f avfoundation -list_devices true`.
// Screen recorders are not cameras and are left out.
func parseAVFoundation(out string) []Device {
	var devs []Device
	var kind Kind
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if m := avfSection.FindStringSubmatch(line); m != nil {
			kind = Kind(m[1])
			continue
		}
		if kind == "" {
			continue
		}
		if m := avfDevice.FindStringSubmatch(line); m != nil {
			name := strings.TrimSpace(m[2])
			if kind == Video && strings.HasPrefix(name, "Capture screen") {
				continue
			}
			devs = append(devs, Device{Kind: kind, ID: m[1], Name: name})
		}
	}
	return devs
}

// parseDShow reads `ffmpeg -f dshow -list_devices true`. dshow inputs are
// named, so the name is the ID.
func parseDShow(out string) []Device {
	var devs []Device
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Alternative name") {
			continue
		}
		if m := dshowLine.FindStringSubmatch(line); m != nil {
			devs = append(devs, Device{Kind: Kind(m[2]), ID: m[1], Name: m[1]})
		}
	}
	return devs
}

// readV4L2 lists cameras from sysfs. A camera exposes several nodes (the
// second is usually metadata); the lowest-numbered one per name captures.
func readV4L2(root string) ([]Device, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("capture: %w", err)
	}
	type node struct {
		index int
		name  string
	}
	var nodes []node
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "video") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "video"))
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name(), "name"))
		if err != nil {
			continue
		}
		nodes = append(nodes, node{index: idx, name: strings.TrimSpace(string(b))})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].index < nodes[j].index })
	seen := map[string]bool{}
	var devs []Device
	for _, n := range nodes {
		if seen[n.name] {
			continue
		}
		seen[n.name] = true
		devs = append(devs, Device{Kind: Video, ID: "/dev/video" + strconv.Itoa(n.index), Name: n.name})
	}
	return devs, nil
}

// parsePactlSources reads `pactl list sources`, skipping monitors of
// outputs.
func parsePactlSources(out string) []Device {
	var devs []Device
	var name, desc string
	flush := func() {
		if name != "" && !strings.HasSuffix(name, ".monitor") {
			if desc == "" {
				desc = name
			}
			devs = append(devs, Device{Kind: Audio, ID: name, Name: desc})
		}
		name, desc = "", ""
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Source #") {
			flush()
			continue
		}
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Name: "):
			name = strings.TrimPrefix(t, "Name: ")
		case strings.HasPrefix(t, "Description: "):
			desc = strings.TrimPrefix(t, "Description: ")
		}
	}
	flush()
	return devs
}

// ErrNoDevice is returned when a selector matches nothing.
var ErrNoDevice = errors.New("capture: no such device")

// Select picks the device of a kind named by selector: its number in the
// listing, its exact name, or a case-insensitive substring of its name
// matching exactly one device. An empty selector picks the first.
func Select(devs []Device, kind Kind, selector string) (Device, error) {
	var ofKind []Device
	for _, d := range devs {
		if d.Kind == kind {
			ofKind = append(ofKind, d)
		}
	}
	if len(ofKind) == 0 {
		return Device{}, fmt.Errorf("%w: no %s devices", ErrNoDevice, kind)
	}
	sel := strings.TrimSpace(selector)
	if sel == "" {
		return ofKind[0], nil
	}
	if n, err := strconv.Atoi(sel); err == nil {
		if n < 0 || n >= len(ofKind) {
			return Device{}, fmt.Errorf("%w: %s %d (have %d)", ErrNoDevice, kind, n, len(ofKind))
		}
		return ofKind[n], nil
	}
	for _, d := range ofKind {
		if strings.EqualFold(d.Name, sel) || d.ID == sel {
			return d, nil
		}
	}
	var matches []Device
	for _, d := range ofKind {
		if strings.Contains(strings.ToLower(d.Name), strings.ToLower(sel)) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Device{}, fmt.Errorf("%w: %s %q", ErrNoDevice, kind, selector)
	default:
		names := make([]string, len(matches))
		for i, d := range matches {
			names[i] = d.Name
		}
		return Device{}, fmt.Errorf("%w: %q matches several %s devices: %s", ErrNoDevice, selector, kind, strings.Join(names, ", "))
	}
}
