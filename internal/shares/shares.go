// Package shares is the Shares page's side of Samba: folders shared on the home network over SMB (Finder, Windows
// Explorer, phones' file apps), the accounts that sign in to them, and who may read or write each one.
//
// Homeport writes its shares to their own file (/etc/samba/homeport.conf), included from the end of smb.conf, and
// leaves everything else in smb.conf alone: shares set up by hand keep working and show on the page as they are.
// Every share's files belong to one system account (force user), so whoever writes over the network, the files stay
// usable; Samba itself checks who may read and write.
package shares

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"homeport/internal/runner"
	"homeport/internal/state"
)

// FilesAccount owns the files of the folders Homeport makes for shares.
const FilesAccount = "homeport-files"

const includeLine = "include = /etc/samba/homeport.conf"

type Manager struct {
	Runner runner.Runner
	OS     string
	// Dir is Samba's settings folder (/etc/samba); Avahi is where Avahi's service files go (/etc/avahi/services).
	Dir, Avahi string
	// Input runs a command with something on its standard input (passwords for smbpasswd; tests stand in for it).
	Input func(ctx context.Context, input, name string, args ...string) (string, error)
}

func New(r runner.Runner, goos string) *Manager {
	return &Manager{Runner: r, OS: goos, Dir: "/etc/samba", Avahi: "/etc/avahi/services", Input: input}
}

func input(ctx context.Context, in, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(in)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// Account is one sign-in for shares.
type Account struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
	Ours     bool   `json:"ours,omitempty"` // Homeport made its system account (else it's a person's own login)
}

// Other is a share set up outside Homeport (in smb.conf by hand).
type Other struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type Status struct {
	Supported bool      `json:"supported"`
	Why       string    `json:"why,omitempty"`
	Installed bool      `json:"installed"`
	Running   bool      `json:"running"`
	Enabled   bool      `json:"enabled"` // starts with the server
	Accounts  []Account `json:"accounts"`
	Others    []Other   `json:"others"`
}

func (m *Manager) usable() error {
	if m.OS != "linux" {
		return errors.New("on a Mac, share folders in System Settings → General → Sharing → File Sharing")
	}
	return nil
}

// Status is Samba's state: installed, running, its accounts, and shares set up by hand.
func (m *Manager) Status(ctx context.Context, ours []string) Status {
	st := Status{Accounts: []Account{}, Others: []Other{}}
	if err := m.usable(); err != nil {
		st.Why = err.Error()
		return st
	}
	st.Supported = true
	if _, err := os.Stat(filepath.Join(m.Dir, "smb.conf")); err != nil {
		return st
	}
	if out, _ := m.Runner.Output(ctx, "", "dpkg-query", "-W", "-f=${Status}", "samba"); strings.Contains(out, "install ok installed") {
		st.Installed = true
	}
	if !st.Installed {
		return st
	}
	out, _ := m.Runner.Output(ctx, "", "systemctl", "is-active", "smbd")
	st.Running = strings.TrimSpace(out) == "active"
	out, _ = m.Runner.Output(ctx, "", "systemctl", "is-enabled", "smbd")
	st.Enabled = strings.TrimSpace(out) == "enabled"
	st.Accounts = m.accounts(ctx, ours)
	if b, err := os.ReadFile(filepath.Join(m.Dir, "smb.conf")); err == nil {
		st.Others = others(string(b))
	}
	return st
}

// accounts are Samba's sign-ins (pdbedit -Lw: name:uid:lm:nt:[flags]:…).
func (m *Manager) accounts(ctx context.Context, ours []string) []Account {
	out, err := m.Runner.Output(ctx, "", "pdbedit", "-L", "-w")
	list := []Account{}
	if err != nil {
		return list
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) < 5 || f[0] == "" {
			continue
		}
		list = append(list, Account{Name: f[0], Disabled: strings.Contains(f[4], "D"), Ours: slices.Contains(ours, f[0])})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

var section = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)

// others are smb.conf's own shares (not global, homes or printers).
func others(conf string) []Other {
	list := []Other{}
	var cur *Other
	for _, line := range strings.Split(conf, "\n") {
		if m := section.FindStringSubmatch(line); m != nil {
			cur = nil
			switch strings.ToLower(m[1]) {
			case "global", "homes", "printers", "print$":
				continue
			}
			list = append(list, Other{Name: m[1]})
			cur = &list[len(list)-1]
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && cur != nil && strings.TrimSpace(k) == "path" {
			cur.Path = strings.TrimSpace(v)
		}
	}
	return list
}

// Install installs Samba (as its own system job, outside Homeport's sandbox, like the system updates).
func (m *Manager) Install(ctx context.Context, w io.Writer) error {
	if err := m.usable(); err != nil {
		return err
	}
	return m.Runner.Stream(ctx, "", w, "systemd-run", "--quiet", "--wait", "--pipe", "--collect", "--property=Type=exec",
		"--setenv=DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q", "samba")
}

// Service starts (and enables) or stops (and disables) file sharing.
func (m *Manager) Service(ctx context.Context, on bool) error {
	if err := m.usable(); err != nil {
		return err
	}
	args := []string{"disable", "--now", "smbd"}
	if on {
		args = []string{"enable", "--now", "smbd"}
	}
	if out, err := m.Runner.Output(ctx, "", "systemctl", args...); err != nil {
		return fmt.Errorf("couldn't %s file sharing: %s", map[bool]string{true: "start", false: "stop"}[on], lastLine(out, err))
	}
	return nil
}

// MARK: Accounts

var accountName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// AddAccount gives name a password for shares. A name that's already a person's login on this server (uid 1000 and
// up) uses that login; a new one gets a system account that can't sign in to the server itself. It reports whether
// Homeport made the system account.
func (m *Manager) AddAccount(ctx context.Context, name, password string) (made bool, err error) {
	if err := m.usable(); err != nil {
		return false, err
	}
	if !accountName.MatchString(name) {
		return false, errors.New("a name is lower-case letters, digits, - and _ (starting with a letter)")
	}
	if len(password) < 8 {
		return false, errors.New("use a password of at least 8 characters")
	}
	if u, err := user.Lookup(name); err == nil {
		if uid, _ := strconv.Atoi(u.Uid); uid < 1000 || uid >= 60000 {
			return false, fmt.Errorf("%q is one of the system's own accounts: pick another name", name)
		}
	} else {
		if out, err := m.Runner.Output(ctx, "", "useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent",
			"--shell", "/usr/sbin/nologin", "--comment", "Homeport file sharing", name); err != nil {
			return false, fmt.Errorf("couldn't make the account: %s", lastLine(out, err))
		}
		made = true
	}
	if out, err := m.Input(ctx, password+"\n"+password+"\n", "smbpasswd", "-s", "-a", name); err != nil {
		if made {
			_, _ = m.Runner.Output(ctx, "", "userdel", name)
		}
		return false, fmt.Errorf("couldn't set the password: %s", lastLine(out, err))
	}
	_, _ = m.Runner.Output(ctx, "", "smbpasswd", "-e", name)
	return made, nil
}

// SetPassword changes an account's password for shares.
func (m *Manager) SetPassword(ctx context.Context, name, password string) error {
	if len(password) < 8 {
		return errors.New("use a password of at least 8 characters")
	}
	if out, err := m.Input(ctx, password+"\n"+password+"\n", "smbpasswd", "-s", name); err != nil {
		return fmt.Errorf("couldn't set the password: %s", lastLine(out, err))
	}
	return nil
}

// Enable turns an account's sign-in on or off (its password stays).
func (m *Manager) Enable(ctx context.Context, name string, on bool) error {
	flag := "-d"
	if on {
		flag = "-e"
	}
	if out, err := m.Runner.Output(ctx, "", "smbpasswd", flag, name); err != nil {
		return errors.New(lastLine(out, err))
	}
	return nil
}

// RemoveAccount removes a sign-in, and the system account too when Homeport made it.
func (m *Manager) RemoveAccount(ctx context.Context, name string, ours bool) error {
	if out, err := m.Runner.Output(ctx, "", "smbpasswd", "-x", name); err != nil {
		return fmt.Errorf("couldn't remove it: %s", lastLine(out, err))
	}
	if ours {
		_, _ = m.Runner.Output(ctx, "", "userdel", name)
	}
	return nil
}

// MARK: Shares

var shareName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)

// blocked are folders that mustn't be shared (the system's own, and Homeport's).
var blocked = []string{"/", "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys",
	"/usr", "/var", "/tmp", "/snap"}

// CheckShare checks a new or changed share against the others (Homeport's and hand-made ones).
func (m *Manager) CheckShare(sh state.Share, ours []state.Share, accounts []Account) error {
	if !shareName.MatchString(sh.Name) {
		return errors.New("a share's name is letters, digits, - and _ (up to 40)")
	}
	switch strings.ToLower(sh.Name) {
	case "global", "homes", "printers", "print$", "ipc$":
		return fmt.Errorf("%q is a name Samba keeps for itself", sh.Name)
	}
	b, _ := os.ReadFile(filepath.Join(m.Dir, "smb.conf"))
	for _, o := range others(string(b)) {
		if strings.EqualFold(o.Name, sh.Name) {
			return fmt.Errorf("there's already a share called %s (set up outside Homeport)", o.Name)
		}
	}
	for _, o := range ours {
		if strings.EqualFold(o.Name, sh.Name) {
			return fmt.Errorf("there's already a share called %s", o.Name)
		}
	}
	if err := CheckPath(sh.Path); err != nil {
		return err
	}
	for name, a := range sh.Access {
		if a != "read" && a != "write" {
			return fmt.Errorf("%s: access is read or write", name)
		}
		if !slices.ContainsFunc(accounts, func(x Account) bool { return x.Name == name }) {
			return fmt.Errorf("%s isn't an account for shares (add it first)", name)
		}
	}
	return nil
}

// CheckPath allows folders for data (on /srv, /mnt, /media, a drive's own folder…), not the system's own.
func CheckPath(p string) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return errors.New("the folder is a full path, like /srv/shares/family")
	}
	for _, b := range blocked {
		if p == b || (b != "/" && b != "/var" && strings.HasPrefix(p, b+"/")) {
			return fmt.Errorf("%s is one of the system's own folders: share a folder for data (under /srv or a drive)", p)
		}
	}
	if strings.HasPrefix(p, "/var/") && !strings.HasPrefix(p, "/var/shares/") {
		return fmt.Errorf("%s is one of the system's own folders: share a folder for data (under /srv or a drive)", p)
	}
	if p == "/home" || strings.HasPrefix(p, "/home/") {
		return errors.New("Homeport can't reach folders in /home: pick one under /srv or on a drive")
	}
	return nil
}

// Prepare makes the share's folder if it isn't there yet (belonging to the files account), or works out who owns an
// existing one, and returns the owner. An existing folder keeps its owner, so files written over the network belong
// to whoever already uses it (an app, say).
func (m *Manager) Prepare(ctx context.Context, path string) (string, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := m.filesAccount(ctx); err != nil {
			return "", err
		}
		// The folders made on the way are open to pass through (Homeport's own default would keep them to root,
		// and Samba, opening the share as its files' account, couldn't reach it).
		missing := []string{}
		for d := filepath.Dir(path); d != "/" && d != "."; d = filepath.Dir(d) {
			if _, err := os.Stat(d); err == nil {
				break
			}
			missing = append(missing, d)
		}
		if err := os.MkdirAll(path, 0o775); err != nil {
			return "", err
		}
		for _, d := range missing {
			_ = os.Chmod(d, 0o755)
		}
		u, err := user.Lookup(FilesAccount)
		if err != nil {
			return "", err
		}
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(u.Gid)
		if err := os.Chown(path, uid, gid); err != nil {
			return "", err
		}
		return FilesAccount, os.Chmod(path, 0o775)
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is a file, not a folder", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return "", fmt.Errorf("%s belongs to the system (root): share a new folder inside it instead, which Homeport makes", path)
	}
	u, err := user.LookupId(strconv.Itoa(int(st.Uid)))
	if err != nil {
		return "", fmt.Errorf("%s belongs to an account this server doesn't know (%d)", path, st.Uid)
	}
	return u.Username, nil
}

func (m *Manager) filesAccount(ctx context.Context) error {
	if _, err := user.Lookup(FilesAccount); err == nil {
		return nil
	}
	if out, err := m.Runner.Output(ctx, "", "useradd", "--system", "--user-group", "--no-create-home", "--home-dir", "/nonexistent",
		"--shell", "/usr/sbin/nologin", "--comment", "Homeport shared files", FilesAccount); err != nil {
		return fmt.Errorf("couldn't make the %s account: %s", FilesAccount, lastLine(out, err))
	}
	return nil
}

// PassThrough lets every account pass through the folders above path (search permission only: what's in them stays
// private), so Samba, opening a share as its files' account, reaches it. It returns the folders it opened.
func PassThrough(path string) []string {
	var opened []string
	for d := filepath.Dir(filepath.Clean(path)); d != "/" && d != "."; d = filepath.Dir(d) {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o001 != 0 {
			continue
		}
		if os.Chmod(d, fi.Mode().Perm()|0o001) == nil {
			opened = append(opened, d)
		}
	}
	return opened
}

// Config is homeport.conf for these shares.
func Config(list []state.Share) string {
	var b strings.Builder
	b.WriteString("# Written by Homeport (the Shares page). Changes here are replaced.\n")
	for _, sh := range list {
		var readers, writers []string
		for name, a := range sh.Access {
			readers = append(readers, name)
			if a == "write" {
				writers = append(writers, name)
			}
		}
		sort.Strings(readers)
		sort.Strings(writers)
		fmt.Fprintf(&b, "\n[%s]\n", sh.Name)
		fmt.Fprintf(&b, "   path = %s\n", sh.Path)
		if sh.Comment != "" {
			fmt.Fprintf(&b, "   comment = %s\n", strings.Map(func(r rune) rune {
				if r < 0x20 {
					return -1
				}
				return r
			}, sh.Comment))
		}
		fmt.Fprintf(&b, "   available = %s\n", yes(!sh.Off))
		b.WriteString("   browseable = yes\n   read only = yes\n")
		if sh.Guests {
			b.WriteString("   guest ok = yes\n")
		} else {
			b.WriteString("   guest ok = no\n")
			if len(readers) == 0 {
				b.WriteString("   valid users = nobody\n") // nobody yet
			} else {
				fmt.Fprintf(&b, "   valid users = %s\n", strings.Join(readers, " "))
			}
		}
		if len(writers) > 0 {
			fmt.Fprintf(&b, "   write list = %s\n", strings.Join(writers, " "))
		}
		if sh.Owner != "" {
			fmt.Fprintf(&b, "   force user = %s\n", sh.Owner)
		}
		b.WriteString("   create mask = 0664\n   directory mask = 0775\n")
		// Macs: Finder's extra file information, kept the way macOS's own file sharing does.
		b.WriteString("   vfs objects = catia fruit streams_xattr\n   fruit:metadata = stream\n   fruit:posix_rename = yes\n" +
			"   fruit:veto_appledouble = no\n   fruit:wipe_intentionally_left_blank_rfork = yes\n   fruit:delete_empty_adfiles = yes\n")
	}
	return b.String()
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Apply writes the shares' settings, makes sure smb.conf includes them, checks the result with testparm and has
// Samba read it again. Shares turned off are closed for anyone still connected.
func (m *Manager) Apply(ctx context.Context, list []state.Share) error {
	if err := m.usable(); err != nil {
		return err
	}
	conf := filepath.Join(m.Dir, "homeport.conf")
	old, _ := os.ReadFile(conf)
	if err := os.WriteFile(conf, []byte(Config(list)), 0o644); err != nil {
		return err
	}
	main := filepath.Join(m.Dir, "smb.conf")
	b, err := os.ReadFile(main)
	if err != nil {
		return fmt.Errorf("Samba isn't set up here (%v)", err)
	}
	if !strings.Contains(string(b), includeLine) {
		add := "\n# Homeport's shares (the Shares page)\n" + includeLine + "\n"
		if err := os.WriteFile(main, append(bytes.TrimRight(b, "\n"), []byte("\n"+add)...), 0o644); err != nil {
			return err
		}
	}
	for _, sh := range list {
		PassThrough(sh.Path)
	}
	if out, err := m.Runner.Output(ctx, "", "testparm", "-s", main); err != nil {
		_ = os.WriteFile(conf, old, 0o644)
		return fmt.Errorf("Samba didn't accept the settings: %s", lastLine(out, err))
	}
	_, _ = m.Runner.Output(ctx, "", "smbcontrol", "smbd", "reload-config")
	for _, sh := range list {
		if sh.Off {
			_, _ = m.Runner.Output(ctx, "", "smbcontrol", "smbd", "close-share", sh.Name)
		}
	}
	m.announce(len(list) > 0)
	return nil
}

// announce tells Macs on the network about the shares (Bonjour), so the server shows up in Finder's sidebar.
func (m *Manager) announce(on bool) {
	if m.Avahi == "" {
		return
	}
	path := filepath.Join(m.Avahi, "homeport-smb.service")
	if !on {
		_ = os.Remove(path)
		return
	}
	if _, err := os.Stat(m.Avahi); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(`<?xml version="1.0" standalone='no'?>
<!DOCTYPE service-group SYSTEM "avahi-service.dtd">
<!-- Written by Homeport: file sharing, for Finder -->
<service-group>
  <name replace-wildcards="yes">%h</name>
  <service><type>_smb._tcp</type><port>445</port></service>
  <service><type>_device-info._tcp</type><port>0</port><txt-record>model=RackMac</txt-record></service>
</service-group>
`), 0o644)
}

// Close disconnects everyone from a share that's going away.
func (m *Manager) Close(ctx context.Context, name string) {
	_, _ = m.Runner.Output(ctx, "", "smbcontrol", "smbd", "close-share", name)
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}
