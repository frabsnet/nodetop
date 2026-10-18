package usage

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/frabsnet/nodetop/internal/model"
)

// A Proxmox container with 2 vCPUs (cgroup v2), read twice: the rates and the
// levels it is over must come out exactly.
func TestWindow(t *testing.T) {
	root := t.TempDir()
	Root, Proc = root, filepath.Join(root, "proc")
	dir := filepath.Join(root, "lxc", "101")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.max", "200000 100000\n")
	write("memory.max", strconv.Itoa(1<<30)+"\n")
	set := func(cpuUsec, rbytes, wbytes, rios, wios int) {
		write("cpu.stat", "usage_usec "+strconv.Itoa(cpuUsec)+"\nuser_usec 0\n")
		write("io.stat", "8:0 rbytes="+strconv.Itoa(rbytes)+" wbytes="+strconv.Itoa(wbytes)+" rios="+strconv.Itoa(rios)+" wios="+strconv.Itoa(wios)+"\n")
		write("memory.current", strconv.Itoa(900<<20)+"\n")
	}
	vm := model.VM{Key: model.VMKey{Node: "n1", Platform: "proxmox-lxc", ID: "101"}}
	c := NewCollector(model.Levels{CPUPct: 90, DiskMB: 100})
	c.SetVMs([]model.VM{vm})
	set(0, 0, 0, 0, 0)
	c.Prime()
	c.at = c.at.Add(-10 * time.Second) // as if the first reading was 10 s ago
	// 10 s later: 19 s of CPU on 2 vCPUs (95%), 1.5 GB read and written (150 MB/s), 3000 operations.
	set(19_000_000, 1_000_000_000, 500_000_000, 2000, 1000)
	s := c.Window("n1", "proxmox", "test").VMs[0]
	near := func(got, want float64) bool { return got > want*0.98 && got < want*1.02 }
	if !s.Measured || s.VCPUs != 2 || !near(s.CPUPct, 95) || !near(s.DiskMBps(), 150) || !near(s.IOPS, 300) {
		t.Fatalf("got %+v", s)
	}
	if len(s.Over) != 2 || s.Over[0] != "cpu" || s.Over[1] != "disk" {
		t.Fatalf("over %v, want cpu and disk", s.Over)
	}
	if !near(s.MemPct, 900.0/1024*100) {
		t.Fatalf("memory %.1f%%", s.MemPct)
	}
}

// A Xen guest has no control group: shown, never measured or over.
func TestUnreadable(t *testing.T) {
	Root, Proc = t.TempDir(), t.TempDir()
	c := NewCollector(model.Levels{CPUPct: 90})
	c.SetVMs([]model.VM{{Key: model.VMKey{Node: "n1", Platform: "xen", ID: "web1"}}})
	c.Prime()
	s := c.Window("n1", "xen", "test")
	if s.VMs[0].Measured || len(s.VMs[0].Over) != 0 || len(s.Warnings) != 1 {
		t.Fatalf("got %+v", s)
	}
}
