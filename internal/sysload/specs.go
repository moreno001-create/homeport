package sysload

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Specs is what this machine is, for the Overview page.
type Specs struct {
	OS       string        `json:"os"`     // "Ubuntu 26.04.1 LTS", "macOS 27.0.1"
	Kernel   string        `json:"kernel"` // "7.0.0-30-generic"
	Hostname string        `json:"hostname"`
	Arch     string        `json:"arch"`
	CPU      string        `json:"cpu"` // the processor's model
	Cores    int           `json:"cores"`
	Memory   int64         `json:"memory"` // bytes
	Disk     Disk          `json:"disk"`
	GPU      string        `json:"gpu,omitempty"`
	Uptime   time.Duration `json:"uptime"` // nanoseconds
	Docker   string        `json:"docker,omitempty"`
	Caddy    string        `json:"caddy,omitempty"`
}

// Disk is the space on the disk Homeport's data is on.
type Disk struct {
	Total int64 `json:"total"`
	Free  int64 `json:"free"`
}

// ReadSpecs finds out what this machine is. dataDir is where Homeport keeps its data (for the disk it's on).
func ReadSpecs(ctx context.Context, dataDir string) Specs {
	s := Specs{Arch: runtime.GOARCH, Cores: runtime.NumCPU()}
	s.Hostname, _ = os.Hostname()
	s.Kernel = run(ctx, "uname", "-r")
	var st syscall.Statfs_t
	if syscall.Statfs(dataDir, &st) == nil {
		s.Disk = Disk{Total: int64(st.Blocks) * int64(st.Bsize), Free: int64(st.Bavail) * int64(st.Bsize)}
	}
	s.Docker = run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if v := strings.Fields(run(ctx, "caddy", "version")); len(v) > 0 {
		s.Caddy = v[0]
	}
	if runtime.GOOS == "darwin" {
		s.OS = "macOS " + run(ctx, "sw_vers", "-productVersion")
		s.CPU = run(ctx, "sysctl", "-n", "machdep.cpu.brand_string")
		s.Memory, _ = strconv.ParseInt(run(ctx, "sysctl", "-n", "hw.memsize"), 10, 64)
		if b := run(ctx, "sysctl", "-n", "kern.boottime"); b != "" { // { sec = 1791700000, usec = 0 } …
			if f := strings.Fields(strings.NewReplacer("{", " ", "}", " ", ",", " ").Replace(b)); len(f) >= 3 {
				if sec, err := strconv.ParseInt(f[2], 10, 64); err == nil {
					s.Uptime = time.Since(time.Unix(sec, 0)).Round(time.Minute)
				}
			}
		}
		if g := macGPU(ctx); g != nil {
			s.GPU = g.Name
		}
		return s
	}
	s.OS = osRelease()
	s.CPU = cpuModel()
	_, s.Memory = linuxMemory()
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			sec, _ := strconv.ParseFloat(f[0], 64)
			s.Uptime = (time.Duration(sec) * time.Second).Round(time.Minute)
		}
	}
	s.GPU = linuxGPUName(ctx)
	return s
}

func run(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func osRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

func cpuModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// linuxGPUName is the graphics chip's name: NVIDIA's from nvidia-smi, else the display controller lspci lists.
func linuxGPUName(ctx context.Context) string {
	if n := run(ctx, "nvidia-smi", "--query-gpu=name", "--format=csv,noheader"); n != "" {
		return strings.Split(n, "\n")[0]
	}
	for _, line := range strings.Split(run(ctx, "lspci"), "\n") {
		if strings.Contains(line, "VGA compatible controller") || strings.Contains(line, "Display controller") || strings.Contains(line, "3D controller") {
			if _, name, ok := strings.Cut(line, ": "); ok {
				return name
			}
		}
	}
	return ""
}
