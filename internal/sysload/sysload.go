// Package sysload reads how busy this machine is right now (processor, memory, graphics), for the AI page's live meter.
// Linux reads the kernel's counters (and nvidia-smi or an AMD card's own figure); a Mac reads ps, vm_stat and the
// graphics chip's own utilisation figure (ioreg), none of which need root.
package sysload

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Load is a moment's reading.
type Load struct {
	CPU      int   `json:"cpu"` // percent of all cores
	Cores    int   `json:"cores"`
	MemUsed  int64 `json:"memUsed"` // bytes
	MemTotal int64 `json:"memTotal"`
	GPU      *GPU  `json:"gpu,omitempty"`
}

// GPU is a graphics chip's reading (nil when there's none Homeport can read).
type GPU struct {
	Name     string `json:"name"`
	Percent  int    `json:"percent"`
	MemUsed  int64  `json:"memUsed,omitempty"`
	MemTotal int64  `json:"memTotal,omitempty"`
}

// Meter keeps the last processor counters, so each reading covers the time since the one before.
type Meter struct {
	mu        sync.Mutex
	lastBusy  uint64
	lastTotal uint64
	lastAt    time.Time
	// An Intel card's idle time (milliseconds) at the last reading, for its load since.
	gpuIdle   int64
	gpuIdleAt time.Time
}

// Read takes a reading.
func (m *Meter) Read(ctx context.Context) Load {
	l := Load{Cores: runtime.NumCPU()}
	if runtime.GOOS == "darwin" {
		l.CPU = macCPU(ctx, l.Cores)
		l.MemUsed, l.MemTotal = macMemory(ctx)
		l.GPU = macGPU(ctx)
		return l
	}
	l.CPU = m.linuxCPU()
	l.MemUsed, l.MemTotal = linuxMemory()
	l.GPU = linuxGPU(ctx)
	if l.GPU == nil {
		l.GPU = m.intelGPU()
	}
	return l
}

// linuxCPU is the share of time not idle since the last reading (or over a fifth of a second, the first time).
func (m *Meter) linuxCPU() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	busy, total, ok := procStat()
	if !ok {
		return 0
	}
	if m.lastAt.IsZero() || time.Since(m.lastAt) > 10*time.Second {
		time.Sleep(200 * time.Millisecond)
		m.lastBusy, m.lastTotal = busy, total
		busy, total, _ = procStat()
	}
	db, dt := busy-m.lastBusy, total-m.lastTotal
	m.lastBusy, m.lastTotal, m.lastAt = busy, total, time.Now()
	if dt == 0 {
		return 0
	}
	return int(db * 100 / dt)
}

func procStat() (busy, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text()) // cpu user nice system idle iowait irq softirq steal …
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var idle uint64
	for i, s := range fields[1:] {
		n, _ := strconv.ParseUint(s, 10, 64)
		if i >= 8 { // guest time is already counted in user
			break
		}
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return total - idle, total, true
}

func linuxMemory() (used, total int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var avail int64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = n * 1024
		case "MemAvailable:":
			avail = n * 1024
		}
	}
	return total - avail, total
}

func linuxGPU(ctx context.Context) *GPU {
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, path, "--query-gpu=name,utilization.gpu,memory.used,memory.total", "--format=csv,noheader,nounits").Output()
		if err == nil {
			f := strings.Split(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0], ", ")
			if len(f) == 4 {
				pct, _ := strconv.Atoi(f[1])
				used, _ := strconv.ParseInt(f[2], 10, 64)
				total, _ := strconv.ParseInt(f[3], 10, 64)
				return &GPU{Name: f[0], Percent: pct, MemUsed: used << 20, MemTotal: total << 20}
			}
		}
	}
	// AMD cards report their own load and memory.
	cards, _ := filepath.Glob("/sys/class/drm/card*/device/gpu_busy_percent")
	for _, c := range cards {
		dir := filepath.Dir(c)
		pct := readInt(c)
		total := readInt(filepath.Join(dir, "mem_info_vram_total"))
		if total < 1<<30 { // a processor's built-in graphics, with borrowed memory: not where models run
			continue
		}
		return &GPU{Name: "AMD graphics", Percent: int(pct), MemUsed: readInt(filepath.Join(dir, "mem_info_vram_used")), MemTotal: total}
	}
	return nil
}

func readInt(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// macCPU adds up every process's share of a core (ps), as a share of all cores.
func macCPU(ctx context.Context, cores int) int {
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "%cpu=").Output()
	if err != nil || cores == 0 {
		return 0
	}
	var sum float64
	for _, f := range strings.Fields(string(out)) {
		n, _ := strconv.ParseFloat(strings.ReplaceAll(f, ",", "."), 64)
		sum += n
	}
	return min(100, int(sum/float64(cores)))
}

var vmStatLine = regexp.MustCompile(`^(Pages [a-z ]+|Pages occupied by compressor):\s+(\d+)`)

func macMemory(ctx context.Context) (used, total int64) {
	if out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output(); err == nil {
		total, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}
	out, err := exec.CommandContext(ctx, "vm_stat").Output()
	if err != nil {
		return 0, total
	}
	page := int64(16384)
	if m := regexp.MustCompile(`page size of (\d+) bytes`).FindSubmatch(out); m != nil {
		page, _ = strconv.ParseInt(string(m[1]), 10, 64)
	}
	pages := map[string]int64{}
	for _, line := range strings.Split(string(out), "\n") {
		if m := vmStatLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			pages[m[1]] = n
		}
	}
	// What Activity Monitor calls "memory used": app memory (active + wired) plus compressed.
	used = (pages["Pages active"] + pages["Pages wired down"] + pages["Pages occupied by compressor"]) * page
	return used, total
}

var gpuUtil = regexp.MustCompile(`"Device Utilization %"=(\d+)`)

// macGPU reads the graphics chip's own utilisation figure.
func macGPU(ctx context.Context) *GPU {
	out, err := exec.CommandContext(ctx, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator").Output()
	if err != nil {
		return nil
	}
	m := gpuUtil.FindSubmatch(out)
	if m == nil {
		return nil
	}
	pct, _ := strconv.Atoi(string(m[1]))
	name := "Apple graphics"
	if n := regexp.MustCompile(`"model" = "([^"]+)"`).FindSubmatch(out); n != nil {
		name = string(n[1])
	}
	return &GPU{Name: name, Percent: pct}
}

// Roots of /sys and /proc (tests point them at a folder of their own).
var sysRoot, procRoot = "/sys", "/proc"

// intelGPU reads an Intel Arc card: its load is the share of time its render/compute engines weren't idle since the
// last reading (the driver counts idle milliseconds), and its memory in use is what the programs using it hold, as
// they report it (drm-total-vram0 in /proc/<pid>/fdinfo; Homeport runs as root, so it can read every program's).
func (m *Meter) intelGPU() *GPU {
	cards, _ := filepath.Glob(filepath.Join(sysRoot, "class/drm/card*/device"))
	for _, dev := range cards {
		vendor, _ := os.ReadFile(filepath.Join(dev, "vendor"))
		id, _ := os.ReadFile(filepath.Join(dev, "device"))
		a, ok := intelArc[strings.TrimSpace(string(id))]
		if strings.TrimSpace(string(vendor)) != "0x8086" || !ok {
			continue
		}
		g := &GPU{Name: a.name, MemTotal: int64(a.gb) << 30}
		idle := readInt(filepath.Join(dev, "tile0/gt0/gtidle/idle_residency_ms"))
		m.mu.Lock()
		if !m.gpuIdleAt.IsZero() && idle >= m.gpuIdle {
			if ms := time.Since(m.gpuIdleAt).Milliseconds(); ms > 0 {
				g.Percent = min(100, max(0, int(100-(idle-m.gpuIdle)*100/ms)))
			}
		}
		m.gpuIdle, m.gpuIdleAt = idle, time.Now()
		m.mu.Unlock()
		g.MemUsed = drmVRAM(filepath.Base(mustEval(dev)))
		return g
	}
	return nil
}

func mustEval(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// drmVRAM adds up the graphics memory programs hold on the card at PCI address pdev (0000:09:00.0), counting each
// client once (a program can have it open more than once).
func drmVRAM(pdev string) int64 {
	files, _ := filepath.Glob(filepath.Join(procRoot, "[0-9]*/fdinfo/*"))
	seen := map[string]bool{}
	var total int64
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || !strings.Contains(string(b), "drm-pdev:") {
			continue
		}
		var client, dev string
		var kib int64
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			switch k {
			case "drm-client-id":
				client = v
			case "drm-pdev":
				dev = v
			case "drm-total-vram0":
				n, unit, _ := strings.Cut(v, " ")
				kib, _ = strconv.ParseInt(n, 10, 64)
				switch unit {
				case "MiB":
					kib <<= 10
				case "GiB":
					kib <<= 20
				case "":
					kib >>= 10 // bytes
				}
			}
		}
		if dev != pdev || client == "" || seen[client] {
			continue
		}
		seen[client] = true
		total += kib << 10
	}
	return total
}
