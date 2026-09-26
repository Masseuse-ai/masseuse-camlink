// Package mastago drives a Mastago transcutaneous electrical nerve
// stimulation unit (the "MASTOGO G-" family) over Bluetooth Low Energy:
// the connector's reference stimulation device.
//
// The unit exposes one GATT service (FFF0) with a write characteristic
// (FFF5) and a notify characteristic (FFF4). Commands are ASCII AT lines
// ("AT+CSTR=0,12\r\n"); every reply and unsolicited report arrives as its
// own notification, also a "\r\n"-terminated line. A set command is
// acknowledged with "+NAME:OK" or refused with "+NAME ERROR:<code>"; a
// query is answered with "+NAME:<value>" followed by a bare "OK". The unit
// pushes "+CHEART" heartbeats, "+CSTR:1,<n>" when its own buttons change
// the intensity, "+CLOAD:<0|1>" when pad contact changes, "+CLKEND" when
// its countdown ends and "+QPOWD:<reason>" as it switches itself off.
//
// This file is the codec: line framing, reply classification and the
// unit's bounds. device.go speaks the protocol; driver.go is the
// estim.Driver; probe.go finds units.
package mastago

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/FemLed/masseuse-camlink/internal/ble"
)

// GATT identifiers.
const (
	ServiceUUID ble.UUID = "fff0"
	WriteUUID   ble.UUID = "fff5"
	NotifyUUID  ble.UUID = "fff4"
)

// AdvertisedPrefix is how the units name themselves ("MASTOGO G-12AB").
const AdvertisedPrefix = "MASTOGO"

// The unit's own bounds.
const (
	// LevelMax is the top of the intensity scale (AT+CSTR=0,<0..25>).
	LevelMax = 25
	// ModeMax is the last program number (AT+CMODE=0,<0..31>).
	ModeMax = 31
	// TimerMinS and TimerMaxS bound the countdown (AT+CDCLK=HHMMSS).
	TimerMinS = 1
	TimerMaxS = 7200
	// Channel is the one output channel the two-pad unit has.
	Channel = 0
)

// AllowedModes are the program numbers the service may select: all of
// the unit's own, 0 to ModeMax. Their names are the service's business.
var AllowedModes = func() []int {
	out := make([]int, ModeMax+1)
	for i := range out {
		out[i] = i
	}
	return out
}()

// ModeAllowed says whether m is one of AllowedModes.
func ModeAllowed(m int) bool { return m >= 0 && m <= ModeMax }

// Error codes the unit answers with.
const (
	// ErrCodeNoLoad: the pads are not on the skin, so an intensity was
	// refused (even zero); the unit keeps its previous intensity.
	ErrCodeNoLoad = 102
	// ErrCodeUnsupported: the command is not implemented by this firmware.
	ErrCodeUnsupported = 104
)

// LineEnd terminates every command and reply.
const LineEnd = "\r\n"

// Commands.
const (
	CmdStart      = "AT+QPOWS"
	CmdPause      = "AT+QPOWP"
	QueryMode     = "AT+CMODE?"
	QueryLevel    = "AT+CSTR?"
	QueryTimer    = "AT+CDCLK?"
	QueryPaused   = "AT+QPOWP?"
	QueryBattery  = "AT+CBC?"
	QueryLoad     = "AT+CLOAD?"
	nameMode      = "CMODE"
	nameLevel     = "CSTR"
	nameTimer     = "CDCLK"
	namePaused    = "QPOWP"
	nameStarted   = "QPOWS"
	nameBattery   = "CBC"
	nameLoad      = "CLOAD"
	nameTimerEnd  = "CLKEND"
	nameShutdown  = "QPOWD"
	nameHeartbeat = "CHEART"
)

// CmdSetMode selects program m.
func CmdSetMode(m int) string { return fmt.Sprintf("AT+CMODE=%d,%d", Channel, m) }

// CmdSetLevel sets the intensity.
func CmdSetLevel(n int) string { return fmt.Sprintf("AT+CSTR=%d,%d", Channel, n) }

// CmdSetTimer arms the countdown for seconds (clamped to the unit's
// range).
func CmdSetTimer(seconds int) string { return "AT+CDCLK=" + FormatHHMMSS(seconds) }

// Encode frames one command line for the write characteristic.
func Encode(cmd string) []byte { return []byte(cmd + LineEnd) }

// CommandName is the reply name a command is answered under: "AT+CSTR?"
// and "AT+CSTR=0,3" are both answered as "+CSTR:...".
func CommandName(cmd string) string {
	s := strings.ToUpper(strings.TrimSpace(cmd))
	s = strings.TrimPrefix(s, "AT+")
	if i := strings.IndexAny(s, "=?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// ReplyKind classifies one line from the unit.
type ReplyKind int

const (
	// ReplyUnknown is a line the codec does not recognize.
	ReplyUnknown ReplyKind = iota
	// ReplyAck acknowledges a set command ("+CSTR:OK") or ends a query
	// answer (a bare "OK", Name empty).
	ReplyAck
	// ReplyValue answers a query ("+CSTR:12") or reports a change the unit
	// made on its own ("+CSTR:1,12", Unsolicited when pushed with "++").
	ReplyValue
	// ReplyError refuses a command ("+CSTR ERROR:102", "ERROR:104").
	ReplyError
	// ReplyHeartbeat is the unit's "+CHEART" keepalive.
	ReplyHeartbeat
)

// Reply is one decoded line.
type Reply struct {
	Kind ReplyKind
	// Name is the AT name without "+" ("CSTR"), empty for a bare OK/ERROR.
	Name string
	// Value is the text after the colon for ReplyValue.
	Value string
	// Code is the error code for ReplyError (0 when none was given).
	Code int
	// Unsolicited marks a "++" report.
	Unsolicited bool
	// Raw is the cleaned line.
	Raw string
}

// DecodeReply classifies one line. Stray bytes (error replies carry a
// couple in front) and the line ending are dropped first.
func DecodeReply(line []byte) Reply {
	clean := cleanLine(line)
	r := Reply{Raw: clean}
	if clean == "" {
		return r
	}
	if clean == "OK" {
		r.Kind = ReplyAck
		return r
	}
	if i := strings.Index(clean, "ERROR"); i >= 0 {
		r.Kind = ReplyError
		r.Name = strings.TrimSpace(strings.TrimLeft(clean[:i], "+"))
		if j := strings.LastIndex(clean, ":"); j >= 0 {
			r.Code = firstInt(clean[j+1:])
		}
		return r
	}
	if !strings.HasPrefix(clean, "+") {
		return r
	}
	r.Unsolicited = strings.HasPrefix(clean, "++")
	body := strings.TrimLeft(clean, "+")
	name, value, hasColon := strings.Cut(body, ":")
	r.Name = strings.TrimSpace(name)
	r.Value = strings.TrimSpace(value)
	switch {
	case r.Name == nameHeartbeat:
		r.Kind = ReplyHeartbeat
	case hasColon && r.Value == "OK":
		r.Kind = ReplyAck
	case r.Name == "":
		r.Kind = ReplyUnknown
	default:
		r.Kind = ReplyValue
	}
	return r
}

// cleanLine keeps the printable ASCII of a line and trims it.
func cleanLine(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c <= 0x7e {
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String())
}

// Ints parses the integers in a comma- or semicolon-separated value.
func Ints(value string) []int {
	var out []int
	for _, tok := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' }) {
		if n, err := strconv.Atoi(strings.TrimSpace(tok)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// LastInt is the last integer in a value (the unit reports "<channel>,
// <level>" in pushes and "<level>" in answers; the level is last).
func LastInt(value string) (int, bool) {
	ints := Ints(value)
	if len(ints) == 0 {
		return 0, false
	}
	return ints[len(ints)-1], true
}

func firstInt(s string) int {
	digits := strings.TrimFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	n, _ := strconv.Atoi(digits)
	return n
}

// FormatHHMMSS is the countdown argument for seconds, clamped to the
// unit's range and zero-padded.
func FormatHHMMSS(seconds int) string {
	seconds = max(TimerMinS, min(TimerMaxS, seconds))
	h, rem := seconds/3600, seconds%3600
	return fmt.Sprintf("%02d%02d%02d", h, rem/60, rem%60)
}

// ParseHHMMSS reads a countdown value ("003000", "+CDCLK:003000").
func ParseHHMMSS(value string) (int, bool) {
	var digits []byte
	for i := 0; i < len(value); i++ {
		if value[i] >= '0' && value[i] <= '9' {
			digits = append(digits, value[i])
		}
	}
	if len(digits) < 6 {
		return 0, false
	}
	digits = digits[len(digits)-6:]
	atoi := func(b []byte) int { n, _ := strconv.Atoi(string(b)); return n }
	return atoi(digits[0:2])*3600 + atoi(digits[2:4])*60 + atoi(digits[4:6]), true
}

// ParseVolts reads the battery answer ("0,4.00": channel, volts).
func ParseVolts(value string) (float64, bool) {
	var out float64
	ok := false
	for _, tok := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' }) {
		if v, err := strconv.ParseFloat(strings.TrimSpace(tok), 64); err == nil {
			out, ok = v, true
		}
	}
	return out, ok
}

// BatteryPercent maps the cell voltage to a percentage: 3.2 V is empty,
// 4.0 V is full.
func BatteryPercent(volts float64) int {
	v := max(3.2, min(4.0, volts))
	return int((v-3.2)/0.8*100 + 0.5)
}

// ShutdownReason names a "+QPOWD:<code>" auto-off reason.
func ShutdownReason(code int) string {
	switch code {
	case 0:
		return "no output for too long"
	case 1:
		return "continuous output for too long"
	case 2:
		return "low battery"
	case 3:
		return "power button held"
	}
	return fmt.Sprintf("reason %d", code)
}
