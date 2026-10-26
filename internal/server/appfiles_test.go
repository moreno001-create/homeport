package server

import "testing"

func TestCheckLink(t *testing.T) {
	for _, ok := range []string{
		"https://download.kiwix.org/zim/other/zimgit-water_en_2024-08.zim",
		" https://download.kiwix.org/zim/other/zimgit-water_en_2024-08.zim.torrent ",
		"magnet:?xt=urn:btih:abcdef0123456789&dn=wikipedia.zim",
	} {
		if _, err := checkLink(ok, ".zim"); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "ftp://x/y.zim", "https://x/y.exe", "magnet:?dn=nothing", "file:///etc/passwd", "javascript:alert(1)"} {
		if _, err := checkLink(bad, ".zim"); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
