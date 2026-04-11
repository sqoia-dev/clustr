// Package hardware provides system hardware discovery for HPC nodes.
// It is designed to work in minimal initramfs environments with no external
// tools beyond lsblk for disk discovery.
package hardware

import (
	"os"
	"strings"
)

// DMIInfo holds BIOS/firmware and system identity information read from
// /sys/class/dmi/id/. These values uniquely identify physical hardware.
type DMIInfo struct {
	SystemUUID         string
	SystemSerial       string
	SystemManufacturer string
	SystemProductName  string
	BIOSVendor         string
	BIOSVersion        string
	BIOSDate           string
	BoardSerial        string
}

// SystemInfo is the complete hardware profile for a node.
type SystemInfo struct {
	Hostname  string
	CPUs      []CPU
	Memory    MemoryInfo
	Disks     []Disk
	NICs      []NIC
	DMI       DMIInfo
	IBDevices []IBDevice
	MDArrays  []MDArray
}

// Discover runs all hardware discovery routines and returns a consolidated
// SystemInfo. Partial failures are tolerated — a failed sub-discovery logs
// an error in the corresponding field but does not abort the whole run.
func Discover() (*SystemInfo, error) {
	info := &SystemInfo{}

	hostname, err := os.Hostname()
	if err == nil {
		info.Hostname = hostname
	}

	cpus, err := DiscoverCPUs()
	if err == nil {
		info.CPUs = cpus
	}

	mem, err := DiscoverMemory()
	if err == nil {
		info.Memory = *mem
	}

	disks, err := DiscoverDisks()
	if err == nil {
		info.Disks = disks
	}

	nics, err := DiscoverNICs()
	if err == nil {
		info.NICs = nics
	}

	dmi, err := DiscoverDMI()
	if err == nil {
		info.DMI = *dmi
	}

	ibDevices, err := DiscoverIBDevices()
	if err == nil {
		info.IBDevices = ibDevices
	}

	mdArrays, err := DiscoverMDArrays()
	if err == nil {
		info.MDArrays = mdArrays
	}

	return info, nil
}

// DiscoverDMI reads system identity fields from the DMI sysfs interface.
// On virtual machines some fields will be empty or contain placeholder values.
func DiscoverDMI() (*DMIInfo, error) {
	dmi := &DMIInfo{}

	fields := map[*string]string{
		&dmi.SystemUUID:         "/sys/class/dmi/id/product_uuid",
		&dmi.SystemSerial:       "/sys/class/dmi/id/product_serial",
		&dmi.SystemManufacturer: "/sys/class/dmi/id/sys_vendor",
		&dmi.SystemProductName:  "/sys/class/dmi/id/product_name",
		&dmi.BIOSVendor:         "/sys/class/dmi/id/bios_vendor",
		&dmi.BIOSVersion:        "/sys/class/dmi/id/bios_version",
		&dmi.BIOSDate:           "/sys/class/dmi/id/bios_date",
		&dmi.BoardSerial:        "/sys/class/dmi/id/board_serial",
	}

	for ptr, path := range fields {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // DMI fields are optional; skip on permission/missing errors
		}
		*ptr = strings.TrimSpace(string(raw))
	}

	return dmi, nil
}
