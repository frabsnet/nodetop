// Package model holds what NodeTop measures. A VM is identified by node +
// platform + local ID, the same as in FlowWatch: VM IDs repeat across
// hypervisors, the triple does not.
package model

import "time"

const SchemaVersion = 1

type VMKey struct {
	Node     string `json:"node"`
	Platform string `json:"platform"` // proxmox-qemu, proxmox-lxc, libvirt, xen, generic
	ID       string `json:"id"`
}

func (k VMKey) String() string { return k.Node + "/" + k.Platform + "/" + k.ID }

type Interface struct {
	Name   string `json:"name"`
	MAC    string `json:"mac,omitempty"`
	Bridge string `json:"bridge,omitempty"`
}

type VM struct {
	Key        VMKey       `json:"key"`
	Name       string      `json:"name,omitempty"`
	IPs        []string    `json:"ips,omitempty"`
	Interfaces []Interface `json:"interfaces"`
}

// Stats is one VM in one measurement window.
type Stats struct {
	VM        VM       `json:"vm"`
	Measured  bool     `json:"measured"`        // false: no control group could be read (Xen, or stopped)
	VCPUs     int      `json:"vcpus,omitempty"` // allocated vCPUs
	CPUPct    float64  `json:"cpu_pct"`         // % of its own vCPUs
	ReadMBps  float64  `json:"disk_read_mbps"`  // MB/s
	WriteMBps float64  `json:"disk_write_mbps"` // MB/s
	IOPS      float64  `json:"disk_iops"`       // read + write operations per second
	MemBytes  uint64   `json:"mem_bytes"`       // in use now
	MemCap    uint64   `json:"mem_cap_bytes,omitempty"`
	MemPct    float64  `json:"mem_pct,omitempty"` // of its allocated memory, when known
	Over      []string `json:"over,omitempty"`    // which of your levels it is above now
	OverFor   float64  `json:"over_seconds"`      // how long it has stayed above one of them
}

// DiskMBps is reads and writes together.
func (s Stats) DiskMBps() float64 { return s.ReadMBps + s.WriteMBps }

// Levels are the limits you choose to see highlighted. Zero leaves one off.
// NodeTop only shows them; it never limits anything.
type Levels struct {
	CPUPct float64 `json:"cpu_pct,omitempty"`
	DiskMB float64 `json:"disk_mbps,omitempty"`
	IOPS   float64 `json:"disk_iops,omitempty"`
	MemPct float64 `json:"mem_pct,omitempty"`
}

// Snapshot is one node's view of one measurement window.
type Snapshot struct {
	Schema   int       `json:"schema"`
	Node     string    `json:"node"`
	Platform string    `json:"platform"`
	Version  string    `json:"nodetop_version"`
	Seq      uint64    `json:"seq"`
	Start    time.Time `json:"window_start"`
	End      time.Time `json:"window_end"`
	HostCPU  float64   `json:"host_cpu_pct"` // the node's own CPUs, busy %
	Levels   Levels    `json:"levels"`
	VMs      []Stats   `json:"vms"`
	Warnings []string  `json:"warnings,omitempty"`
}
