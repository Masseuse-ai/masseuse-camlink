package mastago

import (
	"testing"
	"unicode/utf8"
)

func TestDecodeReply(t *testing.T) {
	cases := []struct {
		line string
		want Reply
	}{
		{"+CMODE:OK\r\n", Reply{Kind: ReplyAck, Name: "CMODE", Value: "OK", Raw: "+CMODE:OK"}},
		{"OK\r\n", Reply{Kind: ReplyAck, Raw: "OK"}},
		{"+CSTR:12\r\n", Reply{Kind: ReplyValue, Name: "CSTR", Value: "12", Raw: "+CSTR:12"}},
		{"+CSTR:1,7\r\n", Reply{Kind: ReplyValue, Name: "CSTR", Value: "1,7", Raw: "+CSTR:1,7"}},
		{"++CLOAD:0\r\n", Reply{Kind: ReplyValue, Name: "CLOAD", Value: "0", Unsolicited: true, Raw: "++CLOAD:0"}},
		{"+CSTR ERROR:102\r\n", Reply{Kind: ReplyError, Name: "CSTR", Code: 102, Raw: "+CSTR ERROR:102"}},
		{"\x80\x1b ERROR:104\r\n", Reply{Kind: ReplyError, Code: 104, Raw: "ERROR:104"}},
		{"+CHEART:0,0\r\n", Reply{Kind: ReplyHeartbeat, Name: "CHEART", Value: "0,0", Raw: "+CHEART:0,0"}},
		{"+CBC:0,4.00", Reply{Kind: ReplyValue, Name: "CBC", Value: "0,4.00", Raw: "+CBC:0,4.00"}},
		{"+CDCLK:003000\r\n", Reply{Kind: ReplyValue, Name: "CDCLK", Value: "003000", Raw: "+CDCLK:003000"}},
		{"+QPOWD:0\r\n", Reply{Kind: ReplyValue, Name: "QPOWD", Value: "0", Raw: "+QPOWD:0"}},
		{"+CLKEND:000000", Reply{Kind: ReplyValue, Name: "CLKEND", Value: "000000", Raw: "+CLKEND:000000"}},
		{"garbage\r\n", Reply{Kind: ReplyUnknown, Raw: "garbage"}},
		{"\r\n", Reply{}},
		{"+:", Reply{Kind: ReplyUnknown, Raw: "+:"}},
	}
	for _, c := range cases {
		if got := DecodeReply([]byte(c.line)); got != c.want {
			t.Errorf("DecodeReply(%q) = %+v, want %+v", c.line, got, c.want)
		}
	}
}

func TestCommandName(t *testing.T) {
	for cmd, want := range map[string]string{
		"AT+CSTR?": "CSTR", "AT+CSTR=0,3": "CSTR", "AT+QPOWS": "QPOWS", "at+cmode=0,4\r\n": "CMODE", "AT+CDCLK=003000": "CDCLK",
	} {
		if got := CommandName(cmd); got != want {
			t.Errorf("CommandName(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestCommands(t *testing.T) {
	if got := CmdSetMode(24); got != "AT+CMODE=0,24" {
		t.Error(got)
	}
	if got := CmdSetLevel(7); got != "AT+CSTR=0,7" {
		t.Error(got)
	}
	if got := CmdSetTimer(1800); got != "AT+CDCLK=003000" {
		t.Error(got)
	}
	if got := string(Encode("AT+QPOWP")); got != "AT+QPOWP\r\n" {
		t.Errorf("%q", got)
	}
}

func TestTimerFormat(t *testing.T) {
	for seconds, want := range map[int]string{0: "000001", 1: "000001", 59: "000059", 60: "000100", 1800: "003000", 3600: "010000", 7200: "020000", 99999: "020000"} {
		if got := FormatHHMMSS(seconds); got != want {
			t.Errorf("FormatHHMMSS(%d) = %q, want %q", seconds, got, want)
		}
	}
	for value, want := range map[string]int{"003000": 1800, "+CDCLK:005943": 3600 - 17, "010000": 3600, "000000": 0} {
		got, ok := ParseHHMMSS(value)
		if !ok || got != want {
			t.Errorf("ParseHHMMSS(%q) = %d,%v, want %d", value, got, ok, want)
		}
	}
	if _, ok := ParseHHMMSS("12"); ok {
		t.Error("short value parsed")
	}
}

func TestBattery(t *testing.T) {
	v, ok := ParseVolts("0,4.00")
	if !ok || v != 4.0 {
		t.Fatalf("ParseVolts = %v,%v", v, ok)
	}
	for volts, want := range map[float64]int{4.0: 100, 3.2: 0, 3.6: 50, 5.0: 100, 3.0: 0} {
		if got := BatteryPercent(volts); got != want {
			t.Errorf("BatteryPercent(%v) = %d, want %d", volts, got, want)
		}
	}
}

func TestInts(t *testing.T) {
	if got := Ints("1,7"); len(got) != 2 || got[0] != 1 || got[1] != 7 {
		t.Error(got)
	}
	if n, ok := LastInt("0,4.00"); !ok || n != 0 {
		t.Error(n, ok)
	}
	if _, ok := LastInt(""); ok {
		t.Error("empty parsed")
	}
}

func TestModes(t *testing.T) {
	if len(AllowedModes) != 32 || AllowedModes[0] != 0 || AllowedModes[31] != 31 {
		t.Error(AllowedModes)
	}
	if ModeAllowed(32) || ModeAllowed(-1) || !ModeAllowed(31) {
		t.Error("ModeAllowed bounds")
	}
}

func FuzzDecodeReply(f *testing.F) {
	for _, seed := range []string{"+CSTR:OK\r\n", "+CSTR:12\r\n", "+CSTR ERROR:102\r\n", "\x80\x1b ERROR:104\r\n", "OK\r\n", "++CLKEND:000000", "+", "++", ":", "+:OK", "ERROR", "+CHEART"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		r := DecodeReply(line)
		if !utf8.ValidString(r.Raw) {
			t.Fatalf("Raw is not valid UTF-8: %q", r.Raw)
		}
		for i := 0; i < len(r.Raw); i++ {
			if r.Raw[i] < 0x20 || r.Raw[i] > 0x7e {
				t.Fatalf("Raw keeps a non-printable byte: %q", r.Raw)
			}
		}
		switch r.Kind {
		case ReplyError:
			if r.Code < 0 {
				t.Fatalf("negative code: %+v", r)
			}
		case ReplyAck, ReplyValue, ReplyHeartbeat, ReplyUnknown:
		default:
			t.Fatalf("unknown kind %d", r.Kind)
		}
		if _, ok := ParseHHMMSS(r.Value); ok && len(r.Value) < 6 {
			t.Fatalf("short countdown parsed: %q", r.Value)
		}
	})
}
