package store

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func totalMemory() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

const powerSupplyDir = "/sys/class/power_supply"

func onBattery() *bool {
	return onBatteryFrom(powerSupplyDir)
}

// onBatteryFrom reads a power_supply class directory: an online external
// supply means false; otherwise a discharging battery means true, and a
// charging/full battery false. No supplies (or unreadable) means unknown.
func onBatteryFrom(dir string) *bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	read := func(name, attr string) string {
		b, err := os.ReadFile(filepath.Join(dir, name, attr))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	var sawBattery, discharging, external bool
	for _, e := range ents {
		name := e.Name()
		switch read(name, "type") {
		case "Battery":
			if read(name, "scope") == "Device" {
				continue // peripheral (mouse, keyboard) battery
			}
			sawBattery = true
			if read(name, "status") == "Discharging" {
				discharging = true
			}
		case "Mains", "USB", "USB_C", "USB_PD", "USB_PD_DRP", "Wireless":
			if read(name, "online") == "1" {
				external = true
			}
		}
	}
	var v bool
	switch {
	case external:
		v = false
	case sawBattery:
		v = discharging
	default:
		return nil
	}
	return &v
}
