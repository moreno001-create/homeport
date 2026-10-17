package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

func enc(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestAttachments(t *testing.T) {
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 40)
	att, err := attachments(context.Background(), []fileIn{
		{Name: "shot.png", Type: "image/png", Data: enc(png)},
		{Name: "app.log", Type: "", Data: enc("line one\nERROR: disk full\n")},
		{Name: "pic-named-wrong.txt", Type: "text/plain", Data: enc(png)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(att.Images) != 2 || att.Images[0].Type != "image/png" || att.Images[1].Name != "pic-named-wrong.txt" {
		t.Fatalf("pictures are found by their content: %+v", att.Images)
	}
	if len(att.Files) != 1 || !strings.Contains(att.Text, "File app.log:\n```\nline one\nERROR: disk full\n```") {
		t.Fatalf("text files go along as text: %q %v", att.Text, att.Files)
	}
	for why, f := range map[string]fileIn{
		"binary":     {Name: "x.bin", Data: enc("\x00\x01\x02")},
		"bad base64": {Name: "x.txt", Data: "%%%"},
		"heic":       {Name: "x.heic", Type: "image/heic", Data: enc("....ftypheic")},
	} {
		if _, err := attachments(context.Background(), []fileIn{f}); err == nil {
			t.Errorf("%s: should be refused", why)
		}
	}
	many := make([]fileIn, maxPictures+1)
	for i := range many {
		many[i] = fileIn{Name: "p.png", Data: enc(png)}
	}
	if _, err := attachments(context.Background(), many); err == nil {
		t.Error("too many pictures should be refused")
	}
	long := fileBlock("big.txt", strings.Repeat("a", maxFileText+10))
	if !strings.Contains(long, "left out") || len(long) > maxFileText+200 {
		t.Error("long files are cut short")
	}
}
