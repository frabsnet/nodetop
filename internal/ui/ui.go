// Package ui draws the live terminal dashboard with plain ANSI escapes (no
// dependencies), and writes JSON and CSV reports.
package ui

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/frabsnet/nodetop/internal/model"
)

const (
	Brand  = "NodeTop by Frabs.net"
	Footer = "Resource abuse capped automatically, only when the node is under pressure: frabs.net"
)

// MB/s, human readable.
func MBps(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.1f GB/s", v/1000)
	case v >= 10:
		return fmt.Sprintf("%.0f MB/s", v)
	case v >= 0.05:
		return fmt.Sprintf("%.1f MB/s", v)
	}
	return "0 MB/s"
}

func Bytes(b uint64) string {
	f := float64(b)
	u := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", f, u[i])
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

// Dur: how long, as 45s, 12m, 3h05m.
func Dur(s float64) string {
	n := int(s)
	switch {
	case n <= 0:
		return ""
	case n < 60:
		return strconv.Itoa(n) + "s"
	case n < 3600:
		return strconv.Itoa(n/60) + "m"
	}
	return fmt.Sprintf("%dh%02dm", n/3600, n%3600/60)
}

func Pct(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) + "%" }

// ---- terminal

type termios syscall.Termios

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// Raw puts the terminal in raw mode and returns a restore function.
func Raw() (func(), error) {
	var old termios
	if err := ioctl(0, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(0, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	fmt.Print("\x1b[?1049h\x1b[?25l")
	return func() {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		_ = ioctl(0, syscall.TCSETS, unsafe.Pointer(&old))
	}, nil
}

func Size() (int, int) {
	var ws struct{ Row, Col, X, Y uint16 }
	if ioctl(1, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil || ws.Col == 0 {
		return 120, 40
	}
	return int(ws.Col), int(ws.Row)
}

func Keys() <-chan []byte {
	ch := make(chan []byte)
	go func() {
		buf := make([]byte, 8)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(ch)
				return
			}
			ch <- append([]byte(nil), buf[:n]...)
		}
	}()
	return ch
}

// ---- dashboard

const (
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
	teal  = "\x1b[38;5;37m"
	amber = "\x1b[38;5;214m"
	inv   = "\x1b[7m"
	reset = "\x1b[0m"
)

// State is what the viewer has chosen.
type State struct {
	Selected int
	SortBy   string // cpu, disk, iops, mem, over
	Filter   string
	Typing   bool
	Paused   bool
}

var Sorts = []string{"cpu", "disk", "iops", "mem", "over"}

func Sort(vms []model.Stats, by string) {
	sort.SliceStable(vms, func(a, b int) bool {
		x, y := vms[a], vms[b]
		if x.Measured != y.Measured {
			return x.Measured // the ones NodeTop cannot read go last
		}
		switch by {
		case "disk":
			return x.DiskMBps() > y.DiskMBps()
		case "iops":
			return x.IOPS > y.IOPS
		case "mem":
			return x.MemPct > y.MemPct
		case "over":
			return x.OverFor > y.OverFor
		}
		return x.CPUPct > y.CPUPct
	})
}

func Filter(vms []model.Stats, q string) []model.Stats {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return vms
	}
	var out []model.Stats
	for _, v := range vms {
		hay := strings.ToLower(v.VM.Key.String() + " " + v.VM.Name + " " + strings.Join(v.VM.IPs, " "))
		if strings.Contains(hay, q) {
			out = append(out, v)
		}
	}
	return out
}

func pad(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		if n <= 1 {
			return string(r[:n])
		}
		return string(r[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(r))
}

func lpad(s string, n int) string {
	if len([]rune(s)) >= n {
		return s
	}
	return strings.Repeat(" ", n-len([]rune(s))) + s
}

// LevelsText: the levels in force, for the header ("CPU 90%, disk 200 MB/s").
func LevelsText(l model.Levels) string {
	var p []string
	if l.CPUPct > 0 {
		p = append(p, "CPU "+Pct(l.CPUPct))
	}
	if l.DiskMB > 0 {
		p = append(p, "disk "+MBps(l.DiskMB))
	}
	if l.IOPS > 0 {
		p = append(p, "IOPS "+strconv.FormatFloat(l.IOPS, 'f', 0, 64))
	}
	if l.MemPct > 0 {
		p = append(p, "memory "+Pct(l.MemPct))
	}
	if len(p) == 0 {
		return "none"
	}
	return strings.Join(p, ", ")
}

// Row is one VM's line, the same in the dashboard and the report.
func Row(v model.Stats, nameW int) string {
	name := v.VM.Key.ID
	if v.VM.Name != "" && v.VM.Name != v.VM.Key.ID {
		name += " " + v.VM.Name
	}
	ip := ""
	if len(v.VM.IPs) > 0 {
		ip = v.VM.IPs[0]
	}
	if !v.Measured {
		return fmt.Sprintf("%s %s %s %s %s %s %s %s", pad(name, nameW), pad(ip, 15), lpad("-", 6), lpad("-", 5), lpad("-", 10), lpad("-", 7), lpad("-", 6), lpad("", 9))
	}
	mem := "-"
	if v.MemCap > 0 {
		mem = Pct(v.MemPct)
	}
	over := ""
	if len(v.Over) > 0 {
		over = Dur(max(v.OverFor, 1)) + " " + strings.Join(v.Over, "+")
	}
	return fmt.Sprintf("%s %s %s %s %s %s %s %s", pad(name, nameW), pad(ip, 15), lpad(Pct(v.CPUPct), 6), lpad(strconv.Itoa(v.VCPUs), 5), lpad(MBps(v.DiskMBps()), 10), lpad(strconv.FormatFloat(v.IOPS, 'f', 0, 64), 7), lpad(mem, 6), lpad(over, 9))
}

// Header matches Row.
func Header(nameW int) string {
	return fmt.Sprintf("%s %s %s %s %s %s %s %s", pad("VM", nameW), pad("IP", 15), lpad("CPU", 6), lpad("vCPU", 5), lpad("DISK", 10), lpad("IOPS", 7), lpad("MEM", 6), lpad("OVER", 9))
}

// Draw renders one frame.
func Draw(w io.Writer, snap model.Snapshot, panel string, st *State, version string) {
	cols, rows := Size()
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	where := snap.Platform
	if panel != "" {
		where = panel
	}
	fmt.Fprintf(&b, "%s%s NodeTop %s%s by Frabs.net%s  %sv%s%s\x1b[%dG%s%s%s\r\n", teal, bold, reset, bold, reset, dim, version, reset, max(1, cols-len(snap.Node)-len(where)-3), dim, snap.Node+" · "+where, reset)
	over := 0
	for _, v := range snap.VMs {
		if len(v.Over) > 0 {
			over++
		}
	}
	fmt.Fprintf(&b, "%sNode CPU%s %-6s %sVMs%s %-5d %sOver your levels%s %d  %s(%s)%s", dim, reset, Pct(snap.HostCPU), dim, reset, len(snap.VMs), dim, reset, over, dim, LevelsText(snap.Levels), reset)
	if st.Paused {
		fmt.Fprintf(&b, "   %sPAUSED%s", amber, reset)
	}
	b.WriteString("\r\n")
	for _, w := range snap.Warnings {
		fmt.Fprintf(&b, "%s! %s%s\r\n", amber, w, reset)
	}
	b.WriteString("\r\n")

	vms := Filter(append([]model.Stats(nil), snap.VMs...), st.Filter)
	Sort(vms, st.SortBy)
	if st.Selected >= len(vms) {
		st.Selected = max(0, len(vms)-1)
	}
	body := rows - 6 - len(snap.Warnings)
	nameW := max(12, cols-15-57)
	fmt.Fprintf(&b, "%s%s%s\r\n", dim, pad(Header(nameW), cols), reset)
	start := 0
	if st.Selected >= body-1 {
		start = st.Selected - body + 2
	}
	for i := start; i < len(vms) && i-start < body-1; i++ {
		line := pad(Row(vms[i], nameW), cols)
		switch {
		case i == st.Selected:
			fmt.Fprintf(&b, "%s%s%s\r\n", inv, line, reset)
		case len(vms[i].Over) > 0:
			fmt.Fprintf(&b, "%s%s%s\r\n", amber, line, reset)
		default:
			fmt.Fprintf(&b, "%s\r\n", line)
		}
	}
	if len(vms) == 0 && st.Filter != "" {
		fmt.Fprintf(&b, "%s  No VMs match %q%s\r\n", dim, st.Filter, reset)
	} else if len(vms) == 0 {
		fmt.Fprintf(&b, "%s  No VMs found. NodeTop looks for Proxmox, libvirt and Xen guests.%s\r\n", dim, reset)
	}
	keys := "↑↓ select · s sort (" + st.SortBy + ") · / search · p pause · q quit"
	if st.Typing {
		keys = "search: " + st.Filter + "█  (enter to keep, esc to clear)"
	}
	fmt.Fprintf(&b, "\x1b[%d;1H%s%s%s\r\n%s%s%s", rows-1, dim, pad(keys, cols), reset, teal, pad(Footer, cols), reset)
	io.WriteString(w, b.String())
}

// ---- reports

func JSON(w io.Writer, snaps []model.Snapshot) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(snaps)
}

func CSV(w io.Writer, snaps []model.Snapshot) error {
	c := csv.NewWriter(w)
	_ = c.Write([]string{"window_end", "node", "platform", "vm_id", "vm_name", "ip", "measured", "vcpus", "cpu_pct", "disk_read_mbps", "disk_write_mbps", "disk_iops", "mem_bytes", "mem_pct", "over", "over_seconds"})
	for _, s := range snaps {
		for _, v := range s.VMs {
			ip := ""
			if len(v.VM.IPs) > 0 {
				ip = v.VM.IPs[0]
			}
			f := func(x float64) string { return strconv.FormatFloat(x, 'f', 2, 64) }
			_ = c.Write([]string{s.End.UTC().Format("2006-01-02T15:04:05Z"), s.Node, v.VM.Key.Platform, v.VM.Key.ID, v.VM.Name, ip, strconv.FormatBool(v.Measured), strconv.Itoa(v.VCPUs), f(v.CPUPct), f(v.ReadMBps), f(v.WriteMBps), f(v.IOPS), strconv.FormatUint(v.MemBytes, 10), f(v.MemPct), strings.Join(v.Over, "+"), f(v.OverFor)})
		}
	}
	c.Flush()
	return c.Error()
}
