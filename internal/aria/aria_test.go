package aria

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A stand-in aria2: one HTTP download going, one magnet still fetching its details (already done: hidden), the
// torrent that followed it, and one that failed.
func TestList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		if req.Params[0] != "token:s3cret" {
			io.WriteString(w, `{"error":{"message":"Unauthorized"}}`)
			return
		}
		switch req.Method {
		case "aria2.tellActive":
			io.WriteString(w, `{"result":[
{"gid":"a1","status":"active","dir":"/z","totalLength":"1000","completedLength":"250","downloadSpeed":"50","connections":"3","files":[{"path":"/z/wiki.zim","uris":[]}]},
{"gid":"a2","status":"active","dir":"/z","totalLength":"2000","completedLength":"100","downloadSpeed":"20","connections":"9","numSeeders":"4","bittorrent":{"info":{"name":"big.zim"}},"files":[{"path":"/z/big.zim"}]}]}`)
		case "aria2.tellWaiting":
			io.WriteString(w, `{"result":[]}`)
		case "aria2.tellStopped":
			io.WriteString(w, `{"result":[
{"gid":"m1","status":"complete","dir":"/z","totalLength":"0","completedLength":"0","bittorrent":{},"files":[{"path":"[METADATA]big"}]},
{"gid":"e1","status":"error","dir":"/z","errorMessage":"404 Not Found","files":[{"path":"","uris":[{"uri":"https://x/gone.zim"}]}]}]}`)
		}
	}))
	defer srv.Close()
	m := &Manager{secret: "s3cret", url: srv.URL, client: srv.Client(), started: true}
	list, err := m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("the finished magnet details are hidden: %+v", list)
	}
	if d := list[0]; d.Name != "wiki.zim" || d.Done != 250 || d.Total != 1000 || d.Speed != 50 || d.Torrent {
		t.Errorf("http download: %+v", d)
	}
	if d := list[1]; d.Name != "big.zim" || !d.Torrent || d.Seeders != 4 || d.Peers != 9 {
		t.Errorf("torrent: %+v", d)
	}
	if d := list[2]; d.Name != "gone.zim" || d.Status != "error" || d.Error != "404 Not Found" {
		t.Errorf("failed: %+v", d)
	}
	m.secret = "wrong"
	if _, err := m.List(context.Background()); err == nil {
		t.Error("a wrong secret is refused")
	}
}
