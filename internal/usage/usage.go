// Package usage reads what each VM uses of its host (CPU, disk, memory) from
// the control group the platform already made for it. It reads only: no
// limits are written, and nothing runs inside a VM.
package usage

import (
	"bufio"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Root is the cgroup filesystem (a variable for tests).
var Root = "/sys/fs/cgroup"

// Proc is /proc (a variable for tests).
var Proc = "/proc"

// Target is where one VM's usage is read.
type Target struct {
	V2     bool              `json:"v2"`
	Dir    string            `json:"dir"`     // v2: the VM's cgroup directory
	V1     map[string]string `json:"v1"`      // v1: controller -> directory (cpu, cpuacct, blkio, memory)
	VCPUs  int               `json:"vcpus"`   // allocated vCPUs (CPU % is of these)
	MemCap uint64            `json:"mem_cap"` // allocated memory in bytes, 0 when unknown
}

// Usage is a reading of cumulative counters (and memory now).
type Usage struct {
	CPUUsec    uint64
	ReadBytes  uint64
	WriteBytes uint64
	ReadOps    uint64
	WriteOps   uint64
	MemBytes   uint64
	Devices    []string // MAJ:MIN of the block devices the VM has used
}

// ErrNotFound: the platform runs this VM outside any cgroup NodeTop can read (Xen, or stopped).
var ErrNotFound = errors.New("no control group for this VM")

// Locate finds a VM's cgroup. id and name are the platform's identifiers; platform and kind come
// from discovery ("proxmox", "libvirt", "qemu", "lxd"...; "vm" or "container").
func Locate(id, name, platform, kind string) (*Target, error) {
	if kind == "container" {
		return container(id, name)
	}
	pid := vmPID(id, name, platform)
	if pid == "" {
		return nil, ErrNotFound
	}
	t, err := fromPID(pid)
	if err != nil {
		return nil, err
	}
	t.VCPUs, t.MemCap = qemuSize(pid)
	return t, nil
}

// vmPID: the QEMU process of a VM, from the platform's pid file or the running processes.
func vmPID(id, name, platform string) string {
	var files []string
	if platform == "proxmox" {
		files = append(files, "/var/run/qemu-server/"+id+".pid")
	}
	for _, n := range []string{name, id} {
		if n != "" {
			files = append(files, "/run/libvirt/qemu/"+n+".pid")
		}
	}
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			p := strings.TrimSpace(string(b))
			if _, err := os.Stat(filepath.Join(Proc, p)); err == nil {
				return p
			}
		}
	}
	// Any other QEMU (a panel's own layout, or started by hand): by -uuid or -name.
	dirs, _ := os.ReadDir(Proc)
	for _, d := range dirs {
		if !d.IsDir() || d.Name()[0] < '0' || d.Name()[0] > '9' {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(Proc, d.Name(), "cmdline"))
		if err != nil || !strings.Contains(string(raw), "qemu") {
			continue
		}
		args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		for i := 0; i+1 < len(args); i++ {
			v := args[i+1]
			if args[i] == "-name" {
				v = strings.Split(strings.TrimPrefix(v, "guest="), ",")[0]
			}
			if (args[i] == "-uuid" || args[i] == "-name") && (v == id || name != "" && v == name) {
				return d.Name()
			}
		}
	}
	return ""
}

// fromPID: the VM's own cgroup from its process: the nearest ".scope" above the process's group
// (libvirt keeps QEMU in .../machine-qemu-N-name.scope/libvirt/emulator, Proxmox in
// qemu.slice/<vmid>.scope), so the reading covers every vCPU thread.
func fromPID(pid string) (*Target, error) {
	f, err := os.Open(filepath.Join(Proc, pid, "cgroup"))
	if err != nil {
		return nil, ErrNotFound
	}
	defer f.Close()
	v1 := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := scopeOf(parts[2])
		if parts[0] == "0" && parts[1] == "" {
			if path == "/" {
				return nil, ErrNotFound
			}
			return &Target{V2: true, Dir: filepath.Join(Root, path)}, nil
		}
		for _, c := range strings.Split(parts[1], ",") {
			switch c {
			case "cpu", "cpuacct", "blkio", "memory":
				v1[c] = filepath.Join(Root, parts[1], path)
			}
		}
	}
	if v1["cpu"] == "" && v1["cpuacct"] == "" {
		return nil, ErrNotFound
	}
	return &Target{V1: v1}, nil
}

func scopeOf(p string) string {
	for q := p; q != "/" && q != "." && q != ""; q = filepath.Dir(q) {
		if strings.HasSuffix(q, ".scope") {
			return q
		}
	}
	return p
}

// container: an LXC container's own cgroup (Proxmox, LXD and plain LXC layouts).
func container(id, name string) (*Target, error) {
	for _, n := range []string{id, name} {
		if n == "" {
			continue
		}
		for _, d := range []string{"lxc/" + n, "lxc.payload." + n, "lxc.payload/" + n} {
			dir := filepath.Join(Root, d)
			if _, err := os.Stat(filepath.Join(dir, "cpu.stat")); err == nil {
				t := &Target{V2: true, Dir: dir, VCPUs: v2CPUs(dir)}
				t.MemCap = readUint(filepath.Join(dir, "memory.max"))
				return t, nil
			}
		}
		v1 := map[string]string{}
		for _, c := range []string{"cpu", "cpuacct", "blkio", "memory"} {
			for _, pair := range []string{c, "cpu,cpuacct"} {
				dir := filepath.Join(Root, pair, "lxc", n)
				if _, err := os.Stat(dir); err == nil && (pair == c || c == "cpu" || c == "cpuacct") {
					v1[c] = dir
					break
				}
			}
		}
		if v1["cpuacct"] != "" || v1["cpu"] != "" {
			t := &Target{V1: v1, VCPUs: runtime.NumCPU()}
			if q, p := readInt(filepath.Join(v1["cpu"], "cpu.cfs_quota_us")), readInt(filepath.Join(v1["cpu"], "cpu.cfs_period_us")); q > 0 && p > 0 {
				t.VCPUs = int(math.Ceil(float64(q) / float64(p)))
			}
			if m := readUint(filepath.Join(v1["memory"], "memory.limit_in_bytes")); m < 1<<60 {
				t.MemCap = m
			}
			return t, nil
		}
	}
	return nil, ErrNotFound
}

// v2CPUs: a container's CPU allowance (cpu.max), or every CPU of the server when it has none.
func v2CPUs(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err == nil {
		f := strings.Fields(string(b))
		if len(f) == 2 && f[0] != "max" {
			q, _ := strconv.ParseFloat(f[0], 64)
			p, _ := strconv.ParseFloat(f[1], 64)
			if q > 0 && p > 0 {
				return int(math.Ceil(q / p))
			}
		}
	}
	return runtime.NumCPU()
}

// qemuSize: vCPUs (-smp) and memory (-m) from the QEMU command line.
func qemuSize(pid string) (int, uint64) {
	raw, err := os.ReadFile(filepath.Join(Proc, pid, "cmdline"))
	if err != nil {
		return 1, 0
	}
	args := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	cpus, mem := 1, uint64(0)
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-smp":
			for _, kv := range strings.Split(args[i+1], ",") {
				if n, err := strconv.Atoi(strings.TrimPrefix(kv, "cpus=")); err == nil && n > 0 {
					cpus = n
					break
				}
			}
		case "-m":
			v := args[i+1]
			for _, kv := range strings.Split(v, ",") {
				if strings.HasPrefix(kv, "size=") || !strings.Contains(kv, "=") {
					mem = sizeMB(strings.TrimPrefix(kv, "size="))
					break
				}
			}
		}
	}
	return cpus, mem
}

// sizeMB: QEMU's -m value ("4096", "4096M", "4G", "4194304k") in bytes.
func sizeMB(s string) uint64 {
	s = strings.TrimSpace(s)
	mult := uint64(1 << 20)
	switch {
	case strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		s = s[:len(s)-1]
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"): // libvirt: -m size=4194304k
		mult, s = 1<<10, s[:len(s)-1]
	}
	n, _ := strconv.ParseUint(s, 10, 64)
	return n * mult
}

// Read takes one reading of a VM's counters.
func Read(t *Target) (Usage, error) {
	var u Usage
	if t.V2 {
		stat, err := os.ReadFile(filepath.Join(t.Dir, "cpu.stat"))
		if err != nil {
			return u, ErrNotFound
		}
		for _, l := range strings.Split(string(stat), "\n") {
			if v, ok := strings.CutPrefix(l, "usage_usec "); ok {
				u.CPUUsec, _ = strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			}
		}
		if io, err := os.ReadFile(filepath.Join(t.Dir, "io.stat")); err == nil {
			for _, l := range strings.Split(string(io), "\n") {
				f := strings.Fields(l)
				if len(f) < 2 {
					continue
				}
				u.Devices = append(u.Devices, f[0])
				for _, kv := range f[1:] {
					k, v, _ := strings.Cut(kv, "=")
					n, _ := strconv.ParseUint(v, 10, 64)
					switch k {
					case "rbytes":
						u.ReadBytes += n
					case "wbytes":
						u.WriteBytes += n
					case "rios":
						u.ReadOps += n
					case "wios":
						u.WriteOps += n
					}
				}
			}
		}
		u.MemBytes = readUint(filepath.Join(t.Dir, "memory.current"))
		return u, nil
	}
	acct := t.V1["cpuacct"]
	if acct == "" {
		acct = t.V1["cpu"]
	}
	ns, err := os.ReadFile(filepath.Join(acct, "cpuacct.usage"))
	if err != nil {
		return u, ErrNotFound
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(ns)), 10, 64)
	u.CPUUsec = n / 1000
	if b := t.V1["blkio"]; b != "" {
		devs := map[string]bool{}
		read := func(file string, r, w *uint64) {
			raw, err := os.ReadFile(filepath.Join(b, file))
			if err != nil {
				return
			}
			for _, l := range strings.Split(string(raw), "\n") {
				f := strings.Fields(l)
				if len(f) != 3 {
					continue
				}
				v, _ := strconv.ParseUint(f[2], 10, 64)
				devs[f[0]] = true
				switch f[1] {
				case "Read":
					*r += v
				case "Write":
					*w += v
				}
			}
		}
		read("blkio.throttle.io_service_bytes_recursive", &u.ReadBytes, &u.WriteBytes)
		read("blkio.throttle.io_serviced_recursive", &u.ReadOps, &u.WriteOps)
		if u.ReadBytes == 0 && u.WriteBytes == 0 {
			read("blkio.throttle.io_service_bytes", &u.ReadBytes, &u.WriteBytes)
			read("blkio.throttle.io_serviced", &u.ReadOps, &u.WriteOps)
		}
		for d := range devs {
			u.Devices = append(u.Devices, d)
		}
	}
	if m := t.V1["memory"]; m != "" {
		u.MemBytes = readUint(filepath.Join(m, "memory.usage_in_bytes"))
	}
	return u, nil
}

func readUint(f string) uint64 {
	b, err := os.ReadFile(f)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func readInt(f string) int64 {
	b, err := os.ReadFile(f)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// HostCPU reads the host's own CPU time: busy and total jiffies across every CPU (/proc/stat).
func HostCPU() (busy, total uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(Proc, "stat"))
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(b), "\n")
	f := strings.Fields(line)
	if len(f) < 8 || f[0] != "cpu" {
		return 0, 0, false
	}
	var v [10]uint64
	for i := 1; i < len(f) && i <= 10; i++ {
		v[i-1], _ = strconv.ParseUint(f[i], 10, 64)
	}
	// user nice system idle iowait irq softirq steal guest guest_nice; guest time is already in user.
	idle := v[3] + v[4]
	for _, x := range v[:8] {
		total += x
	}
	return total - idle, total, true
}
