// Package serialport opens the USB serial adapter a stimulation device is
// wired through and lists the adapters present on this computer
// (docs/PROTOCOL.md, section 7).
//
// Everything here is pure Go: the connector's releases are built with
// CGO_ENABLED=0 and reproduced byte for byte, so USB enumeration reads what
// the operating system already publishes (sysfs, the registry, the system
// profiler) instead of linking a native library.
package serialport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"go.bug.st/serial"
)

// FTDIVendor is the USB vendor id of the FTDI parts in the serial cables the
// supported devices ship with (FT232R 0403:6001, FT231X 0403:6015).
const FTDIVendor = 0x0403

// Config is how a port is opened: 8 data bits, no parity, one stop bit, no
// flow control, DTR and RTS asserted, as a desktop serial library would.
type Config struct {
	BaudRate int
	// ReadTimeout bounds one Read; a Read that times out returns 0 bytes and
	// no error. Zero means 500 ms.
	ReadTimeout time.Duration
}

// Port is an open serial port.
type Port interface {
	io.ReadWriteCloser
	// ResetInput discards whatever the adapter has buffered for reading.
	ResetInput() error
}

// ErrNoPort is returned when a path names no serial port.
var ErrNoPort = errors.New("serialport: no such port")

// Open opens the port at path.
func Open(path string, cfg Config) (Port, error) {
	if cfg.BaudRate <= 0 {
		return nil, errors.New("serialport: baud rate must be positive")
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 500 * time.Millisecond
	}
	p, err := serial.Open(path, &serial.Mode{
		BaudRate: cfg.BaudRate,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		var pe *serial.PortError
		if errors.As(err, &pe) && pe.Code() == serial.PortNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNoPort, path)
		}
		return nil, fmt.Errorf("serialport: open %s: %w", path, err)
	}
	for _, step := range []func() error{
		func() error { return p.SetReadTimeout(cfg.ReadTimeout) },
		func() error { return p.SetDTR(true) },
		func() error { return p.SetRTS(true) },
		func() error { return p.ResetInputBuffer() },
		func() error { return p.ResetOutputBuffer() },
	} {
		if err := step(); err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("serialport: configure %s: %w", path, err)
		}
	}
	return &port{Port: p}, nil
}

type port struct {
	serial.Port
}

// Write sends data and waits for the adapter to take it, so a reply is
// never read before the request has left.
func (p *port) Write(b []byte) (int, error) {
	n, err := p.Port.Write(b)
	if err != nil {
		return n, err
	}
	if err := p.Port.Drain(); err != nil {
		return n, err
	}
	return n, nil
}

func (p *port) ResetInput() error { return p.Port.ResetInputBuffer() }

// Candidate is a serial port that may lead to a device.
type Candidate struct {
	// Path opens it: /dev/cu.usbserial-AB0JQ5W9 on macOS, /dev/ttyUSB0 on
	// Linux, COM5 on Windows.
	Path string
	// VendorID and ProductID are the USB ids when the system publishes them;
	// zero when it does not.
	VendorID, ProductID uint16
	// Serial is the adapter's USB serial number when known.
	Serial string
	// Product is the adapter's USB product string when known.
	Product string
}

// String names the candidate for a log line: the path and, when known, the
// USB ids. The serial number is a device identifier and stays out of it.
func (c Candidate) String() string {
	if c.VendorID == 0 {
		return c.Path
	}
	return fmt.Sprintf("%s (%04x:%04x)", c.Path, c.VendorID, c.ProductID)
}

// FTDI says whether the adapter is known to be an FTDI part.
func (c Candidate) FTDI() bool { return c.VendorID == FTDIVendor }

// Candidates lists the USB serial adapters on this computer, FTDI parts
// first. It reads only what the system already publishes and opens nothing.
func Candidates(ctx context.Context) ([]Candidate, error) {
	var cands []Candidate
	var err error
	switch runtime.GOOS {
	case "darwin":
		cands, err = darwinCandidates(ctx, "/dev", runCommand)
	case "linux":
		cands, err = linuxCandidates("/sys/class/tty")
	case "windows":
		cands, err = windowsCandidates()
	default:
		return nil, fmt.Errorf("serialport: %s is not supported", runtime.GOOS)
	}
	if err != nil {
		return nil, err
	}
	sortCandidates(cands)
	return cands, nil
}

// sortCandidates puts FTDI adapters first and orders the rest by path, so a
// scan tries the likeliest adapter first and is stable across runs.
func sortCandidates(cands []Candidate) {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].FTDI() != cands[j].FTDI() {
			return cands[i].FTDI()
		}
		return cands[i].Path < cands[j].Path
	})
}

// Likely says whether a candidate is worth probing for a device: an FTDI
// adapter, or one whose vendor the system does not tell but whose name is
// the one FTDI adapters get on this system.
func (c Candidate) Likely() bool {
	if c.VendorID != 0 {
		return c.FTDI()
	}
	return strings.Contains(strings.ToLower(c.Path), "usbserial")
}
