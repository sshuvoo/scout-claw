//go:build linux

package system

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"scoutclaw/internal/platform"
	"scoutclaw/internal/redact"
)

func collectSysInfo(p platform.Info) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "host: %s\nos: %s (%s/%s)\n", p.Hostname, p.Distro, p.OS, p.Arch)
	if v, err := os.ReadFile("/proc/version"); err == nil {
		fmt.Fprintf(&b, "kernel: %s\n", strings.TrimSpace(string(v)))
	}
	if v, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(v)); len(f) > 0 {
			if s, err := strconv.ParseFloat(f[0], 64); err == nil {
				fmt.Fprintf(&b, "uptime: %s\n", (time.Duration(s) * time.Second).String())
			}
		}
	}
	if v, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Fprintf(&b, "load average (1/5/15m): %s\n", strings.Join(strings.Fields(string(v))[:3], " "))
	}
	fmt.Fprintf(&b, "cpu: %s, %d logical CPUs\n", cpuModel(), numCPU())
	mem := meminfo()
	kb := func(k string) string { return fmtKB(mem[k]) }
	fmt.Fprintf(&b, "memory: total %s, available %s, free %s, buffers+cache %s\n", kb("MemTotal"), kb("MemAvailable"), kb("MemFree"), fmtKB(mem["Buffers"]+mem["Cached"]))
	fmt.Fprintf(&b, "swap: total %s, free %s\n", kb("SwapTotal"), kb("SwapFree"))
	fmt.Fprintf(&b, "privileges: root=%v systemd=%v package_manager=%s\n\ndisks:\n", p.IsRoot, p.Systemd, p.PackageManager)
	b.WriteString(diskUsage())
	return b.String(), nil
}

func numCPU() int {
	n := 0
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "processor") {
			n++
		}
	}
	return n
}

func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

func meminfo() map[string]uint64 {
	m := map[string]uint64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) > 0 {
			n, _ := strconv.ParseUint(fields[0], 10, 64)
			m[k] = n
		}
	}
	return m
}

func fmtKB(kb uint64) string { return fmtBytes(kb * 1024) }

func fmtBytes(n uint64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := uint64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "zfs": true, "f2fs": true, "overlay": true, "tmpfs": true, "fuseblk": true}

func diskUsage() string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "(cannot read /proc/mounts)\n"
	}
	defer f.Close()
	seen := map[string]bool{}
	var b strings.Builder
	fmt.Fprintf(&b, "%-28s %-8s %9s %9s %9s %5s %7s\n", "mount", "type", "size", "used", "avail", "use%", "inode%")
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 3 || !realFS[fs[2]] || seen[fs[1]] {
			continue
		}
		if fs[2] == "tmpfs" && !(fs[1] == "/tmp" || fs[1] == "/dev/shm" || fs[1] == "/run") {
			continue
		}
		seen[fs[1]] = true
		var st syscall.Statfs_t
		if syscall.Statfs(fs[1], &st) != nil || st.Blocks == 0 {
			continue
		}
		bs := uint64(st.Bsize)
		size, avail := st.Blocks*bs, st.Bavail*bs
		used := size - st.Bfree*bs
		pct := 100 * float64(used) / float64(used+avail)
		inode := "-"
		if st.Files > 0 {
			inode = fmt.Sprintf("%.0f%%", 100*float64(st.Files-st.Ffree)/float64(st.Files))
		}
		fmt.Fprintf(&b, "%-28s %-8s %9s %9s %9s %4.0f%% %7s\n", fs[1], fs[2], fmtBytes(size), fmtBytes(used), fmtBytes(avail), pct, inode)
	}
	return b.String()
}

type proc struct {
	pid     int
	name    string
	user    string
	ticks   uint64
	rssKB   uint64
	cmdline string
	cpu     float64
}

func snapshot() map[int]*proc {
	out := map[int]*proc{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	pageKB := uint64(os.Getpagesize() / 1024)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(raw)
		l, r := strings.Index(s, "("), strings.LastIndex(s, ")")
		if l < 0 || r < l {
			continue
		}
		rest := strings.Fields(s[r+1:])
		if len(rest) < 22 {
			continue
		}
		ut, _ := strconv.ParseUint(rest[11], 10, 64)
		st, _ := strconv.ParseUint(rest[12], 10, 64)
		rss, _ := strconv.ParseUint(rest[21], 10, 64)
		out[pid] = &proc{pid: pid, name: s[l+1 : r], ticks: ut + st, rssKB: rss * pageKB}
	}
	return out
}

func listProcesses(ctx context.Context, sortBy string, limit int) (string, error) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		return "", fmt.Errorf("/proc is not available: %w", err)
	}
	const interval = 500 * time.Millisecond
	first := snapshot()
	select {
	case <-time.After(interval):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	second := snapshot()
	var procs []*proc
	for pid, p := range second {
		if f, ok := first[pid]; ok && p.ticks >= f.ticks {
			p.cpu = float64(p.ticks-f.ticks) / (100 * interval.Seconds()) * 100 // assumes CLK_TCK=100 (Linux default)
		}
		procs = append(procs, p)
	}
	sort.Slice(procs, func(i, j int) bool {
		if sortBy == "mem" {
			return procs[i].rssKB > procs[j].rssKB
		}
		return procs[i].cpu > procs[j].cpu
	})
	if len(procs) > limit {
		procs = procs[:limit]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d processes total; top %d by %s\n%-7s %-10s %6s %10s  %s\n", len(second), len(procs), sortBy, "PID", "USER", "CPU%", "RSS", "COMMAND")
	for _, p := range procs {
		p.user = procUser(p.pid)
		raw, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p.pid))
		cmd := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
		if cmd == "" {
			cmd = "[" + p.name + "]"
		}
		if len(cmd) > 160 {
			cmd = cmd[:160] + "…"
		}
		fmt.Fprintf(&b, "%-7d %-10s %6.1f %10s  %s\n", p.pid, p.user, p.cpu, fmtKB(p.rssKB), redact.String(cmd))
	}
	return b.String(), nil
}

func procUser(pid int) string {
	st, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return "?"
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return lookupUser(sys.Uid)
	}
	return "?"
}
