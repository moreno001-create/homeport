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
