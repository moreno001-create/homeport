package sysload

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIntelArcLoadAndMemory(t *testing.T) {
	root := t.TempDir()
	sysRoot, procRoot = filepath.Join(root, "sys"), filepath.Join(root, "proc")
	t.Cleanup(func() { sysRoot, procRoot = "/sys", "/proc" })
	pci := filepath.Join(sysRoot, "devices/pci0000:00/0000:09:00.0")
	os.MkdirAll(filepath.Join(pci, "tile0/gt0/gtidle"), 0o755)
	os.WriteFile(filepath.Join(pci, "vendor"), []byte("0x8086\n"), 0o644)
	os.WriteFile(filepath.Join(pci, "device"), []byte("0xe20b\n"), 0o644)
	idle := filepath.Join(pci, "tile0/gt0/gtidle/idle_residency_ms")
	os.WriteFile(idle, []byte("1000\n"), 0o644)
	os.MkdirAll(filepath.Join(sysRoot, "class/drm/card0"), 0o755)
	os.Symlink(pci, filepath.Join(sysRoot, "class/drm/card0/device"))
	fdinfo := func(pid, fd, client, vram string) {
		dir := filepath.Join(procRoot, pid, "fdinfo")
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, fd), []byte("pos:\t0\ndrm-driver:\txe\ndrm-client-id:\t"+client+"\ndrm-pdev:\t0000:09:00.0\ndrm-total-vram0:\t"+vram+"\n"), 0o644)
	}
	fdinfo("100", "5", "7", "3145728 KiB") // Ollama: 3 GiB
	fdinfo("100", "9", "7", "3145728 KiB") // the same client again
	fdinfo("200", "4", "8", "512 MiB")     // something else
	os.MkdirAll(filepath.Join(procRoot, "300", "fdinfo"), 0o755)
	os.WriteFile(filepath.Join(procRoot, "300", "fdinfo", "1"), []byte("pos:\t0\nflags:\t02\n"), 0o644) // not graphics

	var m Meter
	g := m.intelGPU()
	if g == nil || g.Name != "Intel Arc B580" || g.MemTotal != 12<<30 || g.MemUsed != 3<<30+512<<20 {
		t.Fatalf("first reading: %+v", g)
	}
	m.gpuIdleAt = time.Now().Add(-1000 * time.Millisecond) // a second ago, 1000 ms idle; since then 250 ms more idle
	os.WriteFile(idle, []byte("1250\n"), 0o644)
	if g := m.intelGPU(); g.Percent < 70 || g.Percent > 80 {
		t.Fatalf("busy 3/4 of the last second: %d%%", g.Percent)
	}
}
