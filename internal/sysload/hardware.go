package sysload

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Hardware is what decides which AI models run well here: memory, the processor, and a graphics card's own memory.
type Hardware struct {
	Memory    int64  `json:"memory"` // bytes
	Cores     int    `json:"cores"`  // threads
	CPU       string `json:"cpu"`
	AVX2      bool   `json:"avx2"`              // the processor has the newer vector instructions models run much faster with
	Apple     bool   `json:"apple"`             // Apple silicon: the graphics share the (fast) memory
	GPU       string `json:"gpu,omitempty"`     // a graphics card with its own memory
	GPUKind   string `json:"gpuKind,omitempty"` // "nvidia", "amd" or "intel"
	GPUMemory int64  `json:"gpuMemory,omitempty"`
}

// ReadHardware looks at this machine.
func ReadHardware(ctx context.Context) Hardware {
	h := Hardware{Cores: runtime.NumCPU()}
	if runtime.GOOS == "darwin" {
		h.Memory, _ = strconv.ParseInt(run(ctx, "sysctl", "-n", "hw.memsize"), 10, 64)
		h.CPU = run(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		h.Apple = runtime.GOARCH == "arm64"
		h.AVX2 = strings.Contains(run(ctx, "sysctl", "-n", "machdep.cpu.leaf7_features"), "AVX2")
		return h
	}
	_, h.Memory = linuxMemory()
	h.CPU = cpuModel()
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "flags" {
				h.AVX2 = strings.Contains(" "+v+" ", " avx2 ")
				break
			}
		}
	}
	if out := run(ctx, "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"); out != "" {
		if name, mem, ok := strings.Cut(strings.Split(out, "\n")[0], ", "); ok {
			mb, _ := strconv.ParseInt(strings.TrimSpace(mem), 10, 64)
			h.GPU, h.GPUKind, h.GPUMemory = name, "nvidia", mb<<20
			return h
		}
	}
	cards, _ := filepath.Glob("/sys/class/drm/card*/device/mem_info_vram_total")
	for _, c := range cards {
		if v := readInt(c); v >= 1<<30 && v > h.GPUMemory {
			h.GPUKind, h.GPUMemory = "amd", v
		}
	}
	if h.GPUKind == "amd" {
		h.GPU = linuxGPUName(ctx)
		return h
	}
	// Intel Arc cards don't say how much memory they have where AMD's do: their model (PCI id) does.
	ids, _ := filepath.Glob("/sys/class/drm/card*/device/device")
	for _, f := range ids {
		vendor, _ := os.ReadFile(filepath.Join(filepath.Dir(f), "vendor"))
		dev, _ := os.ReadFile(f)
		if strings.TrimSpace(string(vendor)) != "0x8086" {
			continue
		}
		if a, ok := intelArc[strings.TrimSpace(string(dev))]; ok && int64(a.gb)<<30 > h.GPUMemory {
			h.GPU, h.GPUKind, h.GPUMemory = a.name, "intel", int64(a.gb)<<30
		}
	}
	return h
}

// intelArc are Intel's graphics cards with their own memory, by PCI device id (the A770 comes with 8 or 16 GB: the
// smaller is assumed).
var intelArc = map[string]struct {
	name string
	gb   int
}{
	"0xe20b": {"Intel Arc B580", 12}, "0xe20c": {"Intel Arc B570", 10},
	"0xe211": {"Intel Arc Pro B60", 24}, "0xe212": {"Intel Arc Pro B50", 16},
	"0x56a0": {"Intel Arc A770", 8}, "0x56a1": {"Intel Arc A750", 8}, "0x56a2": {"Intel Arc A580", 8},
	"0x56a5": {"Intel Arc A380", 6}, "0x56a6": {"Intel Arc A310", 4},
}
