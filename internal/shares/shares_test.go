package shares

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"homeport/internal/state"
)

type fake struct{ calls []string }

func (f *fake) Output(_ context.Context, _, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	switch {
	case strings.HasPrefix(cmd, "pdbedit"):
		return "user:1000:XXXX:ABCD:[U          ]:LCT-1\nalex:998:XXXX:ABCD:[DU         ]:LCT-1\n", nil
	case strings.HasPrefix(cmd, "dpkg-query"):
		return "install ok installed", nil
	case strings.HasPrefix(cmd, "systemctl is-active"):
		return "active\n", nil
	case strings.HasPrefix(cmd, "systemctl is-enabled"):
		return "enabled\n", nil
	}
	return "", nil
}
func (f *fake) Stream(context.Context, string, io.Writer, string, ...string) error { return nil }

const handMade = `[global]
   workgroup = WORKGROUP
[printers]
   path = /var/tmp
[Backup]
     path = /mnt/backup
     valid users = user
`

func manager(t *testing.T) (*Manager, *fake) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "smb.conf"), []byte(handMade), 0o644)
	f := &fake{}
	m := &Manager{Runner: f, OS: "linux", Dir: dir}
	return m, f
}

func TestStatus(t *testing.T) {
	m, _ := manager(t)
	st := m.Status(context.Background(), []string{"alex"})
	if !st.Installed || !st.Running || !st.Enabled {
		t.Fatalf("%+v", st)
	}
	if len(st.Accounts) != 2 || st.Accounts[0].Name != "alex" || !st.Accounts[0].Disabled || !st.Accounts[0].Ours || st.Accounts[1].Ours {
		t.Fatalf("accounts: %+v", st.Accounts)
	}
	if len(st.Others) != 1 || st.Others[0].Name != "Backup" || st.Others[0].Path != "/mnt/backup" {
		t.Fatalf("hand-made shares: %+v", st.Others)
	}
}

func TestCheckShare(t *testing.T) {
	m, _ := manager(t)
	accounts := []Account{{Name: "alex"}}
	ok := state.Share{Name: "Family", Path: "/srv/shares/Family", Access: map[string]string{"alex": "write"}}
	if err := m.CheckShare(ok, nil, accounts); err != nil {
		t.Fatal(err)
	}
	for why, sh := range map[string]state.Share{
		"hand-made name": {Name: "backup", Path: "/srv/x"},
		"taken":          {Name: "family", Path: "/srv/y"},
		"system folder":  {Name: "Etc", Path: "/etc"},
		"under /usr":     {Name: "Usr", Path: "/usr/share"},
		"var":            {Name: "Var", Path: "/var/lib/homeport"},
		"relative":       {Name: "Rel", Path: "srv/x"},
		"home":           {Name: "Home", Path: "/home/user"},
		"bad name":       {Name: "a b", Path: "/srv/x"},
		"no account":     {Name: "X", Path: "/srv/x", Access: map[string]string{"sam": "read"}},
		"bad access":     {Name: "X", Path: "/srv/x", Access: map[string]string{"alex": "all"}},
	} {
		if m.CheckShare(sh, []state.Share{ok}, accounts) == nil {
			t.Errorf("%s: should be refused", why)
		}
	}
	if err := CheckPath("/mnt/backup/Movies"); err != nil {
		t.Fatal(err)
	}
}

func TestConfig(t *testing.T) {
	c := Config([]state.Share{
		{Name: "Family", Path: "/srv/shares/Family", Owner: FilesAccount, Access: map[string]string{"sam": "read", "alex": "write"}},
		{Name: "Media", Path: "/mnt/backup/Media", Owner: "user", Guests: true, Off: true},
		{Name: "Empty", Path: "/srv/shares/Empty"},
	})
	for _, want := range []string{
		"[Family]\n   path = /srv/shares/Family\n   available = yes\n",
		"   valid users = alex sam\n   write list = alex\n   force user = homeport-files\n",
		"[Media]\n   path = /mnt/backup/Media\n   available = no\n",
		"   guest ok = yes\n   force user = user\n",
		"[Empty]\n   path = /srv/shares/Empty\n   available = yes\n   browseable = yes\n   read only = yes\n   guest ok = no\n   valid users = nobody\n",
		"vfs objects = catia fruit streams_xattr",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestApplyKeepsTheHandMadeSettings(t *testing.T) {
	m, f := manager(t)
	if err := m.Apply(context.Background(), []state.Share{{Name: "Family", Path: "/srv/shares/Family", Off: true}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(m.Dir, "smb.conf"))
	if !strings.HasPrefix(string(b), handMade) || strings.Count(string(b), includeLine) != 1 {
		t.Fatalf("smb.conf:\n%s", b)
	}
	_ = m.Apply(context.Background(), nil)
	b, _ = os.ReadFile(filepath.Join(m.Dir, "smb.conf"))
	if strings.Count(string(b), includeLine) != 1 {
		t.Fatal("the include line is added once")
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "smbcontrol smbd close-share Family") {
		t.Fatalf("a share turned off is closed: %v", f.calls)
	}
}

func TestAddAccount(t *testing.T) {
	m, f := manager(t)
	var gave string
	m.Input = func(_ context.Context, in, name string, args ...string) (string, error) {
		gave = in + "|" + name + " " + strings.Join(args, " ")
		return "", nil
	}
	if _, err := m.AddAccount(context.Background(), "Bad Name", "longenough"); err == nil {
		t.Fatal("bad names are refused")
	}
	if _, err := m.AddAccount(context.Background(), "newperson", "short"); err == nil {
		t.Fatal("short passwords are refused")
	}
	if _, err := m.AddAccount(context.Background(), "root", "longenough"); err == nil {
		t.Fatal("system accounts are refused")
	}
	made, err := m.AddAccount(context.Background(), "hp-test-nobody-has-this", "longenough")
	if err != nil || !made {
		t.Fatalf("made=%v err=%v", made, err)
	}
	if gave != "longenough\nlongenough\n|smbpasswd -s -a hp-test-nobody-has-this" {
		t.Fatalf("smbpasswd got %q", gave)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "useradd --system --no-create-home") {
		t.Fatalf("a new name gets a system account: %v", f.calls)
	}
}

func TestNotOnAMac(t *testing.T) {
	m := &Manager{OS: "macos"}
	if st := m.Status(context.Background(), nil); st.Supported || st.Why == "" {
		t.Fatalf("%+v", st)
	}
}

func TestPassThrough(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Backup")
	os.MkdirAll(filepath.Join(drive, "shares"), 0o700)
	os.Chmod(drive, 0o750)
	share := filepath.Join(drive, "shares", "8tb")
	os.Mkdir(share, 0o775)
	opened := PassThrough(share)
	for _, d := range []string{drive, filepath.Join(drive, "shares")} {
		fi, _ := os.Stat(d)
		if fi.Mode().Perm()&0o001 == 0 {
			t.Errorf("%s isn't open to pass through: %v", d, fi.Mode())
		}
		if fi.Mode().Perm()&0o004 != 0 && d == drive {
			t.Errorf("%s's contents shouldn't become readable to everyone: %v", d, fi.Mode())
		}
	}
	if len(opened) < 2 { // (the test's own temporary folder above them is closed too)
		t.Fatalf("opened %v", opened)
	}
	if again := PassThrough(share); len(again) != 0 {
		t.Fatalf("nothing more to open the second time: %v", again)
	}
}
