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
	GPUKind   string `json:"gpuKind,omitempty"` // "nvidia" or "amd"
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
	}
	return h
}
