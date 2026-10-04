package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectMachineAndBattery(t *testing.T) {
	m := DetectMachine()
	if m.OS == "" || m.Arch == "" || m.CPUs < 1 {
		t.Fatalf("%+v", m)
	}
	ps := t.TempDir()
	write := func(name, attr, val string) {
		os.MkdirAll(filepath.Join(ps, name), 0o755)
		os.WriteFile(filepath.Join(ps, name, attr), []byte(val+"\n"), 0o644)
	}
	if onBatteryFrom(ps) != nil {
		t.Fatal("no supplies should be unknown")
	}
	write("BAT0", "type", "Battery")
	write("BAT0", "status", "Discharging")
	if b := onBatteryFrom(ps); b == nil || !*b {
		t.Fatal("discharging battery")
	}
	write("AC", "type", "Mains")
	write("AC", "online", "1")
	if b := onBatteryFrom(ps); b == nil || *b {
		t.Fatal("on mains")
	}
}
