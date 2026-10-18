package usage

import (
	"sort"
	"strconv"
	"time"

	"github.com/frabsnet/nodetop/internal/model"
)

// Collector turns two readings of each VM's counters into rates, and keeps
// how long each VM has stayed above the levels you chose.
type Collector struct {
	levels  model.Levels
	vms     []model.VM
	targets map[string]*Target
	missed  map[string]time.Time
	prev    map[string]Usage
	since   map[string]time.Time // first window a VM was over a level, in an unbroken run
	at      time.Time
	hostB   uint64
	hostT   uint64
	seq     uint64
}

func NewCollector(l model.Levels) *Collector {
	return &Collector{levels: l, targets: map[string]*Target{}, missed: map[string]time.Time{}, prev: map[string]Usage{}, since: map[string]time.Time{}}
}

// SetVMs replaces the VM list (discovery runs again every minute).
func (c *Collector) SetVMs(vms []model.VM) {
	// A bare interface with no platform behind it has nothing to measure.
	c.vms = nil
	for _, v := range vms {
		if v.Key.Platform != "generic" {
			c.vms = append(c.vms, v)
		}
	}
	keep := map[string]bool{}
	for _, v := range vms {
		keep[v.Key.String()] = true
	}
	for k := range c.targets {
		if !keep[k] {
			delete(c.targets, k)
			delete(c.prev, k)
			delete(c.since, k)
		}
	}
}

// locate maps a discovered VM to its control group. Xen and bare interfaces
// have none NodeTop can read.
func (c *Collector) locate(v model.VM, now time.Time) *Target {
	k := v.Key.String()
	if t := c.targets[k]; t != nil {
		return t
	}
	if at, ok := c.missed[k]; ok && now.Sub(at) < 5*time.Minute {
		return nil
	}
	var t *Target
	var err error
	switch v.Key.Platform {
	case "proxmox-qemu":
		t, err = Locate(v.Key.ID, v.Name, "proxmox", "vm")
	case "proxmox-lxc":
		t, err = Locate(v.Key.ID, v.Name, "proxmox", "container")
	case "libvirt":
		t, err = Locate(v.Key.ID, v.Name, "libvirt", "vm")
	default:
		err = ErrNotFound
	}
	if err != nil {
		c.missed[k] = now
		return nil
	}
	c.targets[k] = t
	return t
}

// Prime takes the first reading, so the first window has something to compare with.
func (c *Collector) Prime() { c.read(time.Now(), nil) }

// Window reads every VM again and returns the rates since the last reading.
func (c *Collector) Window(node, platform, version string) model.Snapshot {
	now := time.Now()
	start := c.at
	snap := model.Snapshot{Schema: model.SchemaVersion, Node: node, Platform: platform, Version: version, Start: start, End: now, Levels: c.levels}
	c.seq++
	snap.Seq = c.seq
	if b, t, ok := HostCPU(); ok && c.hostT > 0 && t > c.hostT {
		snap.HostCPU = 100 * float64(b-c.hostB) / float64(t-c.hostT)
	}
	c.read(now, &snap)
	sort.SliceStable(snap.VMs, func(a, b int) bool {
		if snap.VMs[a].Measured != snap.VMs[b].Measured {
			return snap.VMs[a].Measured
		}
		return snap.VMs[a].CPUPct > snap.VMs[b].CPUPct
	})
	return snap
}

func (c *Collector) read(now time.Time, snap *model.Snapshot) {
	dt := now.Sub(c.at).Seconds()
	if b, t, ok := HostCPU(); ok {
		c.hostB, c.hostT = b, t
	}
	unread := 0
	for _, v := range c.vms {
		k := v.Key.String()
		st := model.Stats{VM: v}
		t := c.locate(v, now)
		if t != nil {
			u, err := Read(t)
			if err != nil {
				delete(c.targets, k) // restarted into a new group: look again next time
			} else {
				st.Measured = true
				st.VCPUs = max(t.VCPUs, 1)
				st.MemBytes, st.MemCap = u.MemBytes, t.MemCap
				if t.MemCap > 0 {
					st.MemPct = min(100, 100*float64(u.MemBytes)/float64(t.MemCap))
				}
				if p, ok := c.prev[k]; ok && dt > 0 && u.CPUUsec >= p.CPUUsec {
					st.CPUPct = min(100, float64(u.CPUUsec-p.CPUUsec)/1e6/dt/float64(st.VCPUs)*100)
					st.ReadMBps = float64(sub(u.ReadBytes, p.ReadBytes)) / dt / 1e6
					st.WriteMBps = float64(sub(u.WriteBytes, p.WriteBytes)) / dt / 1e6
					st.IOPS = float64(sub(u.ReadOps, p.ReadOps)+sub(u.WriteOps, p.WriteOps)) / dt
				}
				c.prev[k] = u
			}
		}
		if !st.Measured {
			unread++
		}
		st.Over = c.over(st)
		if len(st.Over) > 0 {
			if _, ok := c.since[k]; !ok {
				c.since[k] = c.at
			}
			st.OverFor = now.Sub(c.since[k]).Seconds()
		} else {
			delete(c.since, k)
		}
		if snap != nil {
			snap.VMs = append(snap.VMs, st)
		}
	}
	c.at = now
	if snap != nil && unread > 0 {
		snap.Warnings = append(snap.Warnings, plural(unread, "VM has", "VMs have")+" no control group NodeTop can read (Xen guests, or not running), shown with dashes.")
	}
}

func (c *Collector) over(s model.Stats) []string {
	if !s.Measured {
		return nil
	}
	var out []string
	l := c.levels
	if l.CPUPct > 0 && s.CPUPct >= l.CPUPct {
		out = append(out, "cpu")
	}
	if l.DiskMB > 0 && s.DiskMBps() >= l.DiskMB {
		out = append(out, "disk")
	}
	if l.IOPS > 0 && s.IOPS >= l.IOPS {
		out = append(out, "iops")
	}
	if l.MemPct > 0 && s.MemPct >= l.MemPct {
		out = append(out, "mem")
	}
	return out
}

func sub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
