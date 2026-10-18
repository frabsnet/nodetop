// NodeTop by Frabs.net: see what every VM on a hypervisor uses of it (CPU,
// disk, memory), and which ones stay above the levels you set.
// https://frabs.net
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/frabsnet/nodetop/internal/inventory"
	"github.com/frabsnet/nodetop/internal/model"
	"github.com/frabsnet/nodetop/internal/ui"
	"github.com/frabsnet/nodetop/internal/usage"
)

var version = "0.1.0"

const help = `NodeTop by Frabs.net %s · https://frabs.net
See what every VM on this server uses of it, and which ones stay above your levels.

Usage:
  nodetop                   live dashboard
  nodetop report            measure, then print a report
  nodetop vms               list the VMs NodeTop found
  nodetop version

Levels (shown, never enforced; 0 turns one off):
  -cpu 90                   %% of the VM's own vCPUs
  -disk 0                   disk MB/s, reads and writes together
  -iops 0                   disk operations per second
  -mem 0                    %% of the VM's allocated memory

Options:
  -interval 5s              length of each measurement window
  -duration 60s             report: how long to measure
  -format table|json|csv    report format
  -o file                   write the report to a file

NodeTop reads each VM's control group on the host (cgroup v1 or v2), never
anything inside a VM, and changes nothing. Run it as root.
`

func main() {
	cmd := "top"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, help, version) }
	interval := fs.Duration("interval", 5*time.Second, "")
	duration := fs.Duration("duration", 60*time.Second, "")
	format := fs.String("format", "table", "")
	out := fs.String("o", "", "")
	var lv model.Levels
	fs.Float64Var(&lv.CPUPct, "cpu", 90, "")
	fs.Float64Var(&lv.DiskMB, "disk", 0, "")
	fs.Float64Var(&lv.IOPS, "iops", 0, "")
	fs.Float64Var(&lv.MemPct, "mem", 0, "")
	_ = fs.Parse(args)

	node, _ := os.Hostname()
	switch cmd {
	case "version", "-v", "--version":
		fmt.Printf("NodeTop by Frabs.net %s (https://frabs.net)\n", version)
	case "help", "-h", "--help":
		fs.Usage()
	case "vms":
		platform, vms := inventory.Discover(node)
		fmt.Printf("NodeTop by Frabs.net · %s · %s%s\n\n", node, platform, panelSuffix())
		c := usage.NewCollector(lv)
		c.SetVMs(vms)
		snap := c.Window(node, platform, version)
		snap.Warnings = nil
		for _, s := range snap.VMs {
			found := "control group found"
			if !s.Measured {
				found = "no control group"
			}
			fmt.Printf("%-36s %-24s %-16s %s\n", s.VM.Key.String(), s.VM.Name, strings.Join(s.VM.IPs, ","), found)
		}
		if len(vms) == 0 {
			fmt.Println("No VMs found. NodeTop looks for Proxmox, libvirt (Virtualizor, VirtFusion, SolusVM) and Xen guests.")
		}
	case "report":
		os.Exit(report(node, lv, *interval, *duration, *format, *out))
	case "top":
		os.Exit(dashboard(node, lv, *interval))
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func panelSuffix() string {
	if p := inventory.Panel(); p != "" {
		return " (" + p + ")"
	}
	return ""
}

func report(node string, lv model.Levels, interval, duration time.Duration, format, out string) int {
	if format != "table" && format != "json" && format != "csv" {
		fmt.Fprintf(os.Stderr, "nodetop: unknown format %q (use table, json or csv)\n", format)
		return 2
	}
	platform, vms := inventory.Discover(node)
	c := usage.NewCollector(lv)
	c.SetVMs(vms)
	c.Prime()
	interval = min(interval, duration)
	fmt.Fprintf(os.Stderr, "NodeTop by Frabs.net: measuring %d VMs on %s for %s…\n", len(vms), node, duration)
	var snaps []model.Snapshot
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		time.Sleep(min(interval, time.Until(deadline)))
		snaps = append(snaps, c.Window(node, platform, version))
	}
	w := os.Stdout
	if out != "" {
		f, err := os.Create(out)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer f.Close()
		w = f
	}
	var err error
	switch format {
	case "json":
		err = ui.JSON(w, snaps)
	case "csv":
		err = ui.CSV(w, snaps)
	default:
		err = table(w, snaps, lv)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if out != "" {
		fmt.Fprintf(os.Stderr, "Report written to %s\n", out)
	}
	return 0
}

// summary is one VM across the whole report.
type summary struct {
	s                     model.Stats
	cpuSum, diskSum       float64
	cpuPeak, iopsPeak     float64
	windows, overWindows  int
	overSeconds, overLast float64
}

func table(w *os.File, snaps []model.Snapshot, lv model.Levels) error {
	if len(snaps) == 0 {
		return nil
	}
	first, last := snaps[0], snaps[len(snaps)-1]
	by := map[string]*summary{}
	var order []string
	for _, sn := range snaps {
		span := sn.End.Sub(sn.Start).Seconds()
		for _, v := range sn.VMs {
			k := v.VM.Key.String()
			m := by[k]
			if m == nil {
				m = &summary{}
				by[k] = m
				order = append(order, k)
			}
			m.s = v
			if !v.Measured {
				continue
			}
			m.windows++
			m.cpuSum += v.CPUPct
			m.diskSum += v.DiskMBps()
			m.cpuPeak = max(m.cpuPeak, v.CPUPct)
			m.iopsPeak = max(m.iopsPeak, v.IOPS)
			if len(v.Over) > 0 {
				m.overWindows++
				m.overSeconds += span
			}
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := by[order[a]], by[order[b]]
		if (x.windows == 0) != (y.windows == 0) {
			return x.windows > 0
		}
		if x.overSeconds != y.overSeconds {
			return x.overSeconds > y.overSeconds
		}
		return x.cpuSum > y.cpuSum
	})
	fmt.Fprintf(w, "NodeTop by Frabs.net · %s · %s%s · %s to %s · levels: %s\n\n", last.Node, last.Platform, panelSuffix(), first.Start.Format("15:04:05"), last.End.Format("15:04:05"), ui.LevelsText(lv))
	fmt.Fprintf(w, "%-34s %-15s %8s %9s %10s %9s %6s  %s\n", "VM", "IP", "AVG CPU", "PEAK CPU", "AVG DISK", "PEAK IOPS", "MEM", "TIME OVER")
	for _, k := range order {
		m := by[k]
		v := m.s
		name := v.VM.Key.ID
		if v.VM.Name != "" && v.VM.Name != v.VM.Key.ID {
			name += " " + v.VM.Name
		}
		ip := ""
		if len(v.VM.IPs) > 0 {
			ip = v.VM.IPs[0]
		}
		if m.windows == 0 {
			fmt.Fprintf(w, "%-34.34s %-15s %8s %9s %10s %9s %6s\n", name, ip, "-", "-", "-", "-", "-")
			continue
		}
		n := float64(m.windows)
		mem := "-"
		if v.MemCap > 0 {
			mem = ui.Pct(v.MemPct)
		}
		fmt.Fprintf(w, "%-34.34s %-15s %8s %9s %10s %9.0f %6s  %s\n", name, ip, ui.Pct(m.cpuSum/n), ui.Pct(m.cpuPeak), ui.MBps(m.diskSum/n), m.iopsPeak, mem, ui.Dur(m.overSeconds))
	}
	for _, wn := range last.Warnings {
		fmt.Fprintf(w, "\n! %s", wn)
	}
	fmt.Fprintf(w, "\n%s\n", ui.Footer)
	return nil
}

func dashboard(node string, lv model.Levels, interval time.Duration) int {
	platform, vms := inventory.Discover(node)
	panel := inventory.Panel()
	c := usage.NewCollector(lv)
	c.SetVMs(vms)
	c.Prime()
	restore, err := ui.Raw()
	if err != nil {
		fmt.Fprintln(os.Stderr, "nodetop: the dashboard needs a terminal; try `nodetop report`.")
		return 1
	}
	defer restore()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGWINCH)
	keys := ui.Keys()
	st := &ui.State{SortBy: "cpu"}
	snap := model.Snapshot{Node: node, Platform: platform, Levels: lv, Warnings: []string{"Measuring the first window…"}}
	for _, vm := range vms {
		snap.VMs = append(snap.VMs, model.Stats{VM: vm})
	}
	ui.Draw(os.Stdout, snap, panel, st, version)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	rescan := time.NewTicker(60 * time.Second)
	defer rescan.Stop()
	for {
		select {
		case s := <-sig:
			if s == syscall.SIGWINCH {
				ui.Draw(os.Stdout, snap, panel, st, version)
				continue
			}
			return 0
		case <-rescan.C:
			_, vms = inventory.Discover(node)
			c.SetVMs(vms)
		case <-tick.C:
			next := c.Window(node, platform, version)
			if !st.Paused {
				snap = next
			}
		case k, ok := <-keys:
			if !ok {
				return 0
			}
			key := string(k)
			if st.Typing {
				switch {
				case key == "\r" || key == "\n":
					st.Typing = false
				case key == "\x1b":
					st.Typing, st.Filter = false, ""
				case key == "\x7f" || key == "\b":
					if len(st.Filter) > 0 {
						st.Filter = st.Filter[:len(st.Filter)-1]
					}
				case len(key) == 1 && key[0] >= 32:
					st.Filter += key
				}
				st.Selected = 0
				break
			}
			switch key {
			case "q", "\x03":
				return 0
			case "\x1b[A", "k":
				if st.Selected > 0 {
					st.Selected--
				}
			case "\x1b[B", "j":
				st.Selected++
			case "s":
				for i, s := range ui.Sorts {
					if s == st.SortBy {
						st.SortBy = ui.Sorts[(i+1)%len(ui.Sorts)]
						break
					}
				}
			case "/":
				st.Typing = true
			case "p":
				st.Paused = !st.Paused
			}
		}
		ui.Draw(os.Stdout, snap, panel, st, version)
	}
}
