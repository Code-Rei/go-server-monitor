//go:build windows

package main

import (
	"math"

	"github.com/yusufpapurcu/wmi"
)

// ThermalZone maps to WMI MSAcpi_ThermalZoneTemperature (root\wmi namespace).
// Temperature is returned in tenths of Kelvin.
type ThermalZone struct {
	CurrentTemperature uint32
}

// getTemperature queries the Windows WMI thermal zone for CPU temperature.
// Returns nil if no sensor is available or the reading is out of range.
// NOTE: Run as Administrator for accurate readings on most systems.
func getTemperature() *float64 {
	var zones []ThermalZone
	err := wmi.QueryNamespace(
		"SELECT CurrentTemperature FROM MSAcpi_ThermalZoneTemperature",
		&zones,
		`root\wmi`,
	)
	if err != nil || len(zones) == 0 {
		return nil
	}

	// Pick the highest reading (usually the CPU zone)
	var maxTemp uint32
	for _, z := range zones {
		if z.CurrentTemperature > maxTemp {
			maxTemp = z.CurrentTemperature
		}
	}

	// Convert tenths-of-Kelvin → Celsius
	celsius := float64(maxTemp)/10.0 - 273.15
	if celsius <= 0 || celsius > 120 {
		return nil
	}
	v := math.Round(celsius)
	return &v
}
