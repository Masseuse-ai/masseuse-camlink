package serialport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commandRunner runs a system command and returns its standard output; the
// discovery code takes one so tests can hand it recorded output.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// darwinCandidates lists /dev/cu.usbserial* (the callout device of every
// FTDI adapter under Apple's and FTDI's drivers) and, when the system
// profiler answers, attaches each adapter's USB ids by matching its serial
// number into the device name. A profiler that fails or is slow costs
// nothing but the ids.
func darwinCandidates(ctx context.Context, dev string, run commandRunner) ([]Candidate, error) {
	paths, err := filepath.Glob(filepath.Join(dev, "cu.usbserial*"))
	if err != nil {
		return nil, err
	}
	var cands []Candidate
	for _, p := range paths {
		cands = append(cands, Candidate{Path: p})
	}
	if len(cands) == 0 || run == nil {
		return cands, nil
	}
	out, err := run(ctx, "system_profiler", "SPUSBDataType", "-json", "-detailLevel", "mini")
	if err != nil {
		return cands, nil
	}
	for _, usb := range parseSystemProfiler(out) {
		if usb.serial == "" {
			continue
		}
		for i := range cands {
			// The driver names the callout device after the serial number; a
			// two-port part gets an A/B suffix.
			base := filepath.Base(cands[i].Path)
			if strings.HasSuffix(base, usb.serial) || strings.HasSuffix(base, usb.serial+"A") {
				cands[i].VendorID, cands[i].ProductID = usb.vendor, usb.product
				cands[i].Serial = usb.serial
				cands[i].Product = usb.name
			}
		}
	}
	return cands, nil
}

type usbDevice struct {
	vendor, product uint16
	serial          string
	name            string
}

// parseSystemProfiler walks `system_profiler SPUSBDataType -json`: a tree of
// items, each with vendor_id "0x0403  (Future Technology Devices ...)",
// product_id "0x6001", serial_num and _name, and nested `_items` for hubs.
func parseSystemProfiler(out []byte) []usbDevice {
	var doc struct {
		Items []json.RawMessage `json:"SPUSBDataType"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil
	}
	var devs []usbDevice
	var walk func(items []json.RawMessage)
	walk = func(items []json.RawMessage) {
		for _, raw := range items {
			var item struct {
				Name      string            `json:"_name"`
				VendorID  string            `json:"vendor_id"`
				ProductID string            `json:"product_id"`
				Serial    string            `json:"serial_num"`
				Items     []json.RawMessage `json:"_items"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				continue
			}
			if v, ok := parseUSBID(item.VendorID); ok {
				p, _ := parseUSBID(item.ProductID)
				devs = append(devs, usbDevice{vendor: v, product: p, serial: strings.TrimSpace(item.Serial), name: strings.TrimSpace(item.Name)})
			}
			if len(item.Items) > 0 {
				walk(item.Items)
			}
		}
	}
	walk(doc.Items)
	return devs
}

// parseUSBID reads "0x0403", "0x0403  (Future Technology Devices
// International Limited)" or "0403".
func parseUSBID(s string) (uint16, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t("); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 16)
	if err != nil {
		return 0, false
	}
	return uint16(v), true
}

// linuxCandidates lists /dev/ttyUSB* through sysfs: each tty's `device`
// link is the USB interface, whose parent is the USB device carrying
// idVendor, idProduct, serial and product.
func linuxCandidates(sysTTY string) ([]Candidate, error) {
	entries, err := os.ReadDir(sysTTY)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cands []Candidate
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "ttyUSB") && !strings.HasPrefix(name, "ttyACM") {
			continue
		}
		c := Candidate{Path: "/dev/" + name}
		usb := filepath.Join(sysTTY, name, "device", "..")
		if v, ok := readHex(filepath.Join(usb, "idVendor")); ok {
			c.VendorID = v
		}
		if p, ok := readHex(filepath.Join(usb, "idProduct")); ok {
			c.ProductID = p
		}
		c.Serial = readTrimmed(filepath.Join(usb, "serial"))
		c.Product = readTrimmed(filepath.Join(usb, "product"))
		cands = append(cands, c)
	}
	return cands, nil
}

func readHex(path string) (uint16, bool) {
	return parseUSBID(readTrimmed(path))
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetLatency asks the FTDI driver to flush its receive buffer every ms
// milliseconds instead of its 16 ms default; every reply the device sends
// waits out that timer, so a smaller value shortens each register read.
// Only Linux publishes the knob (sysfs, usually root-writable); elsewhere,
// and without permission, the default stands and this is a no-op.
func SetLatency(c Candidate, ms int) error {
	if !strings.HasPrefix(c.Path, "/dev/ttyUSB") {
		return nil
	}
	p := filepath.Join("/sys/bus/usb-serial/devices", filepath.Base(c.Path), "latency_timer")
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return os.WriteFile(p, []byte(strconv.Itoa(ms)+"\n"), 0o644)
}
