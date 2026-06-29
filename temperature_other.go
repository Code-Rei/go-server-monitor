//go:build !windows

package main

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// getTemperature reads CPU temperature from Linux sysfs.
// Tries hwmon (coretemp / k10temp / zenpower) first for accuracy,
// then falls back to ACPI thermal zones.
// Works on bare-metal Ubuntu; returns nil inside VMs (no physical sensors).
func getTemperature() *float64 {
	if t := hwmonTemp(); t != nil {
		return t
	}
	return thermalZoneTemp()
}

// hwmonTemp reads CPU package temperature from /sys/class/hwmon.
// Supports Intel (coretemp) and AMD (k10temp, zenpower) CPUs.
func hwmonTemp() *float64 {
	cpuDrivers := []string{
		"coretemp",   // Intel
		"k10temp",    // AMD Zen
		"zenpower",   // AMD Zen (alt driver)
		"cpu_thermal",
		"cpu-thermal",
	}

	hwmons, err := filepath.Glob("/sys/class/hwmon/hwmon*")
	if err != nil || len(hwmons) == 0 {
		return nil
	}

	for _, hwmon := range hwmons {
		nameBytes, err := os.ReadFile(filepath.Join(hwmon, "name"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(strings.ToLower(string(nameBytes)))

		isCPU := false
		for _, drv := range cpuDrivers {
			if strings.Contains(name, drv) {
				isCPU = true
				break
			}
		}
		if !isCPU {
			continue
		}

		// Prefer the "Package id 0" / "Tdie" / "Physical id 0" label
		// which represents the whole-package temperature
		labels, _ := filepath.Glob(filepath.Join(hwmon, "temp*_label"))
		for _, labelPath := range labels {
			labelBytes, err := os.ReadFile(labelPath)
			if err != nil {
				continue
			}
			label := strings.TrimSpace(strings.ToLower(string(labelBytes)))

			isPackage := strings.Contains(label, "package") ||
				strings.Contains(label, "physical") ||
				strings.Contains(label, "tdie") ||
				strings.Contains(label, "tccd")

			if !isPackage {
				continue
			}

			inputPath := strings.Replace(labelPath, "_label", "_input", 1)
			if t := readMilliCelsius(inputPath); t != nil {
				return t
			}
		}

		// No labelled package sensor — temp1_input is usually the package
		if t := readMilliCelsius(filepath.Join(hwmon, "temp1_input")); t != nil {
			return t
		}
	}
	return nil
}

// thermalZoneTemp reads from /sys/class/thermal/thermal_zone*/temp
// (ACPI thermal zones — available on most systems including ARM servers).
func thermalZoneTemp() *float64 {
	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(zones) == 0 {
		return nil
	}

	// Try to find the highest CPU-related zone first
	var bestTemp float64
	found := false

	for _, zone := range zones {
		zoneDir := filepath.Dir(zone)
		typeBytes, _ := os.ReadFile(filepath.Join(zoneDir, "type"))
		zoneType := strings.TrimSpace(strings.ToLower(string(typeBytes)))

		t := readMilliCelsius(zone)
		if t == nil {
			continue
		}

		isCPU := strings.Contains(zoneType, "cpu") ||
			strings.Contains(zoneType, "x86_pkg") ||
			strings.Contains(zoneType, "soc") ||
			strings.Contains(zoneType, "acpitz")

		if isCPU && *t > bestTemp {
			bestTemp = *t
			found = true
		}
	}

	if found {
		return &bestTemp
	}

	// Fallback: return the first valid reading regardless of type
	for _, zone := range zones {
		if t := readMilliCelsius(zone); t != nil {
			return t
		}
	}
	return nil
}

// readMilliCelsius reads a sysfs temperature file (value in millidegrees Celsius)
// and converts it to whole degrees, returning nil if the value is out of range.
func readMilliCelsius(path string) *float64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	milliC, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || milliC <= 0 {
		return nil
	}
	celsius := float64(milliC) / 1000.0
	if celsius < 1 || celsius > 120 {
		return nil
	}
	v := math.Round(celsius)
	return &v
}
