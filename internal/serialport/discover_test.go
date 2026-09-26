package serialport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const profilerOutput = `{
  "SPUSBDataType" : [
    {
      "_items" : [
        {
          "_name" : "USB3.1 Hub",
          "vendor_id" : "0x2109  (VIA Labs, Inc.)",
          "product_id" : "0x0817",
          "_items" : [
            {
              "_name" : "FT232R USB UART",
              "manufacturer" : "FTDI",
              "product_id" : "0x6001",
              "serial_num" : "AB0JQ5W9",
              "vendor_id" : "0x0403  (Future Technology Devices International Limited)"
            }
          ]
        },
        {
          "_name" : "Keyboard",
          "product_id" : "0x0250",
          "vendor_id" : "apple_vendor_id"
        }
      ],
      "_name" : "USB31Bus"
    }
  ]
}`

func TestDarwinCandidatesMatchProfilerBySerial(t *testing.T) {
	dev := t.TempDir()
	for _, name := range []string{"cu.usbserial-AB0JQ5W9", "cu.usbserial-110", "cu.Bluetooth-Incoming-Port", "tty.usbserial-AB0JQ5W9"} {
		if err := os.WriteFile(filepath.Join(dev, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		if name != "system_profiler" {
			t.Fatalf("ran %s", name)
		}
		return []byte(profilerOutput), nil
	}
	cands, err := darwinCandidates(context.Background(), dev, run)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("profiler ran %d times", calls)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v", cands)
	}
	sortCandidates(cands)
	ftdi := cands[0]
	if ftdi.Path != filepath.Join(dev, "cu.usbserial-AB0JQ5W9") || ftdi.VendorID != FTDIVendor || ftdi.ProductID != 0x6001 || ftdi.Serial != "AB0JQ5W9" || ftdi.Product != "FT232R USB UART" {
		t.Fatalf("ftdi candidate = %+v", ftdi)
	}
	if !ftdi.FTDI() || !ftdi.Likely() {
		t.Fatal("ftdi adapter should be likely")
	}
	other := cands[1]
	if other.VendorID != 0 || !other.Likely() {
		t.Fatalf("unidentified usbserial adapter should still be likely: %+v", other)
	}
	if got := ftdi.String(); got != ftdi.Path+" (0403:6001)" {
		t.Fatalf("String() = %q", got)
	}
	if got := other.String(); got != other.Path {
		t.Fatalf("String() = %q", got)
	}
}

func TestDarwinCandidatesSurviveProfilerFailure(t *testing.T) {
	dev := t.TempDir()
	if err := os.WriteFile(filepath.Join(dev, "cu.usbserial-110"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("profiler unavailable")
	}
	cands, err := darwinCandidates(context.Background(), dev, run)
	if err != nil || len(cands) != 1 || cands[0].VendorID != 0 {
		t.Fatalf("cands = %+v, err = %v", cands, err)
	}
	// Nothing to identify: the profiler is not even run.
	cands, err = darwinCandidates(context.Background(), t.TempDir(), func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("profiler ran with no adapters present")
		return nil, nil
	})
	if err != nil || len(cands) != 0 {
		t.Fatalf("cands = %+v, err = %v", cands, err)
	}
}

func TestParseUSBID(t *testing.T) {
	cases := map[string]struct {
		id uint16
		ok bool
	}{
		"0x0403":                          {0x0403, true},
		"0x0403  (Future Technology ...)": {0x0403, true},
		"6015":                            {0x6015, true},
		"apple_vendor_id":                 {0, false},
		"":                                {0, false},
		"0x10000":                         {0, false},
	}
	for in, want := range cases {
		id, ok := parseUSBID(in)
		if id != want.id || ok != want.ok {
			t.Errorf("parseUSBID(%q) = %04x, %v; want %04x, %v", in, id, ok, want.id, want.ok)
		}
	}
}

func TestLinuxCandidatesReadSysfs(t *testing.T) {
	sys := t.TempDir()
	// /sys/class/tty/ttyUSB0/device -> .../1-1.4:1.0 whose parent 1-1.4 is
	// the USB device. A real tree uses symlinks; a directory tree with the
	// same shape reads the same.
	usb := filepath.Join(sys, "ttyUSB0", "device", "..")
	if err := os.MkdirAll(filepath.Join(sys, "ttyUSB0", "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"idVendor": "0403\n", "idProduct": "6015\n", "serial": "DN03ABCD\n", "product": "FT231X USB UART\n"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(usb, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"ttyS0", "tty1", "ttyACM0"} {
		if err := os.MkdirAll(filepath.Join(sys, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cands, err := linuxCandidates(sys)
	if err != nil {
		t.Fatal(err)
	}
	sortCandidates(cands)
	if len(cands) != 2 {
		t.Fatalf("candidates = %+v", cands)
	}
	if c := cands[0]; c.Path != "/dev/ttyUSB0" || c.VendorID != FTDIVendor || c.ProductID != 0x6015 || c.Serial != "DN03ABCD" || c.Product != "FT231X USB UART" {
		t.Fatalf("ttyUSB0 = %+v", c)
	}
	if c := cands[1]; c.Path != "/dev/ttyACM0" || c.VendorID != 0 || c.Likely() {
		t.Fatalf("ttyACM0 = %+v", c)
	}
	if cands, err := linuxCandidates(filepath.Join(sys, "missing")); err != nil || cands != nil {
		t.Fatalf("missing sysfs: %+v, %v", cands, err)
	}
}

func TestSortCandidatesFTDIFirst(t *testing.T) {
	cands := []Candidate{{Path: "COM3"}, {Path: "COM9", VendorID: FTDIVendor, ProductID: 0x6001}, {Path: "COM1"}, {Path: "COM5", VendorID: FTDIVendor, ProductID: 0x6015}}
	sortCandidates(cands)
	var got []string
	for _, c := range cands {
		got = append(got, c.Path)
	}
	want := []string{"COM5", "COM9", "COM1", "COM3"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestOpenRejectsBadConfig(t *testing.T) {
	if _, err := Open("/dev/null", Config{}); err == nil {
		t.Fatal("zero baud rate accepted")
	}
	_, err := Open(filepath.Join(t.TempDir(), "absent"), Config{BaudRate: 19200})
	if err == nil {
		t.Fatal("absent port opened")
	}
}
