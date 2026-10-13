// Package mdns announces Homeport's .local names on the home network, so Macs, iPhones, Linux and Windows computers find
// homeport.local and cloud.homeport.local with no DNS to set up: through Avahi on Linux, and macOS's own mDNS (dns-sd)
// on a Mac.
//
// Each name is kept announced by a process (avahi-publish from avahi-utils, or dns-sd -P) running as long as the name
// is wanted.
// Avahi's hosts file can't do it: its names point at this machine's own address, whose reverse lookup belongs to the
// hostname, and Avahi refuses them as a "local name collision". avahi-publish --no-reverse leaves that out.
package mdns

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Local is the .local names among names, sorted, without repeats.
func Local(names []string) []string {
	var out []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if strings.HasSuffix(n, ".local") && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Publisher keeps one avahi-publish (or dns-sd) running for each .local name Homeport serves.
type Publisher struct {
	OS       string        // "linux" or "macos"
	Command  string        // default avahi-publish (Linux) or dns-sd (macOS)
	Hostname string        // this machine's name; Avahi announces Hostname.local itself
	Retry    time.Duration // wait before announcing a name again after its avahi-publish ended (default 10s)
	// HostsFile gets this machine's own names too (Linux: /etc/hosts), so they open on the server itself: Ubuntu
	// looks up .local names with mdns4_minimal, which ignores names with more than two parts (cloud.homeport.local).
	HostsFile string

	mu    sync.Mutex
	names map[string]chan struct{} // "name address" → closed to stop announcing it
}

// Publish announces names (the .local ones) at the first of ips, and stops announcing the ones no longer among them.
// Without Avahi installed it does nothing.
func (p *Publisher) Publish(_ context.Context, names, ips []string) error {
	program := "avahi-publish"
	if p.OS == "macos" {
		program = "dns-sd" // part of macOS
	}
	path, err := exec.LookPath(cmp.Or(p.Command, program))
	if err != nil {
		return nil
	}
	want := map[string][2]string{}
	if len(ips) > 0 {
		own := strings.ToLower(p.Hostname) + ".local"
		for _, n := range Local(names) {
			if n != own {
				want[n+" "+ips[0]] = [2]string{n, ips[0]}
			}
		}
	}
	if p.HostsFile != "" {
		if err := writeHosts(p.HostsFile, Local(names)); err != nil {
			log.Printf("Couldn't add this server's names to %s: %v", p.HostsFile, err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.names == nil {
		p.names = map[string]chan struct{}{}
	}
	for key, stop := range p.names {
		if _, ok := want[key]; !ok {
			close(stop)
			delete(p.names, key)
		}
	}
	for key, w := range want {
		if _, ok := p.names[key]; !ok {
			stop := make(chan struct{})
			p.names[key] = stop
			go p.keep(path, w[0], w[1], stop)
		}
	}
	return nil
}

const (
	hostsStart = "# Homeport: this server's own names (rewritten by Homeport)"
	hostsEnd   = "# Homeport: end"
)

// writeHosts keeps Homeport's block in a hosts file: names at this machine (127.0.0.1). It's only rewritten when
// the names change.
func writeHosts(path string, names []string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var kept []string
	in := false
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		switch {
		case line == hostsStart:
			in = true
		case line == hostsEnd:
			in = false
		case !in && (line != "" || len(kept) > 0):
			kept = append(kept, line)
		}
	}
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	if len(names) > 0 {
		kept = append(kept, "", hostsStart)
		for i := 0; i < len(names); i += 4 { // a few names per line
			kept = append(kept, "127.0.0.1\t"+strings.Join(names[i:min(i+4, len(names))], " "))
		}
		kept = append(kept, hostsEnd)
	}
	text := strings.Join(kept, "\n") + "\n"
	if text == string(b) {
		return nil
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".homeport-new"
	if err := os.WriteFile(tmp, []byte(text), mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Announced is what's being announced now ("name address"), sorted.
func (p *Publisher) Announced() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for key := range p.names {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// Close stops announcing everything.
func (p *Publisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, stop := range p.names {
		close(stop)
		delete(p.names, key)
	}
}

// keep runs avahi-publish for name until stop is closed, starting it again if it ends (Avahi restarted, say).
func (p *Publisher) keep(path, name, ip string, stop <-chan struct{}) {
	for {
		args := []string{"--address", "--no-reverse", name, ip}
		if p.OS == "macos" {
			// A proxy registration: a (made-up) web service whose host record is the name, at this Mac's address.
			args = []string{"-P", "Homeport " + strings.TrimSuffix(name, ".local"), "_http._tcp", "local", "443", name, ip}
		}
		cmd := exec.Command(path, args...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		done := make(chan error, 1)
		if err := cmd.Start(); err != nil {
			done <- err
		} else {
			go func() { done <- cmd.Wait() }()
		}
		select {
		case <-stop:
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				<-done
			}
			return
		case err := <-done:
			log.Printf("Announcing %s stopped (%v): %s", name, err, strings.TrimSpace(out.String()))
		}
		select {
		case <-stop:
			return
		case <-time.After(cmp.Or(p.Retry, 10*time.Second)):
		}
	}
}
