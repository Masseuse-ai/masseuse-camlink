package estim_test

import (
	"encoding/json"
	"testing"

	"github.com/FemLed/masseuse-camlink/internal/estim"
)

// FuzzParseCommand: a command of any bytes never panics, an accepted
// command carries an allowed verb and channel, and CheckCaps refuses every
// accepted actuation that steps outside the connector's caps.
func FuzzParseCommand(f *testing.F) {
	f.Add([]byte(`{"verb":"set_level","channel":"a","level":6}`))
	f.Add([]byte(`{"verb":"set_mode","mode":118}`))
	f.Add([]byte(`{"verb":"adjust_ma","delta":-10,"reason":"x"}`))
	f.Add([]byte(`{"verb":"status"}`))
	f.Add([]byte(`{"verb":"set_level","channel":"b","level":1}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	caps := estim.Capabilities{LevelMax: 99, Channels: []string{"a"}, Modes: []int{0x76, 0x77}, Tempo: true}
	f.Fuzz(func(t *testing.T, raw []byte) {
		cmd, err := estim.ParseCommand(json.RawMessage(raw))
		if err != nil {
			return
		}
		if cmd.Channel != "" && cmd.Channel != "a" {
			t.Fatalf("channel %q accepted", cmd.Channel)
		}
		capsErr := estim.CheckCaps(cmd, caps)
		switch cmd.Verb {
		case "status", "release":
			if capsErr != nil {
				t.Fatalf("%s refused: %v", cmd.Verb, capsErr)
			}
		case "set_level":
			if cmd.Level != nil && (*cmd.Level < 0 || *cmd.Level > estim.LevelCap) && capsErr == nil {
				t.Fatalf("level %d accepted", *cmd.Level)
			}
		case "adjust_level":
			if cmd.Delta != nil && (*cmd.Delta < -estim.LevelDeltaCap || *cmd.Delta > estim.LevelDeltaCap) && capsErr == nil {
				t.Fatalf("delta %d accepted", *cmd.Delta)
			}
		case "set_ma":
			if cmd.Percent != nil && (*cmd.Percent < 0 || *cmd.Percent > estim.TempoPercentCap) && capsErr == nil {
				t.Fatalf("percent %d accepted", *cmd.Percent)
			}
		case "adjust_ma":
			if cmd.Delta != nil && (*cmd.Delta < -estim.TempoDeltaCap || *cmd.Delta > estim.TempoDeltaCap) && capsErr == nil {
				t.Fatalf("tempo delta %d accepted", *cmd.Delta)
			}
		case "set_mode":
			if cmd.Mode != nil && *cmd.Mode != 0x76 && *cmd.Mode != 0x77 && capsErr == nil {
				t.Fatalf("mode %d accepted", *cmd.Mode)
			}
		default:
			t.Fatalf("verb %q accepted", cmd.Verb)
		}
	})
}
