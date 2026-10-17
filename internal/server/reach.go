package server

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"homeport/internal/state"
)

// MARK: How to reach this server
//
// One card, on the portal and the admin panel's Overview, with the addresses to use: the portal, the AI page and each
// app (with the internet port when there is one), whether each works from the internet or only at home, file sharing,
// and for admins the server's own addresses and SSH.

type reachLink struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Internet bool   `json:"internet"` // also from outside the home
}

type reach struct {
	Portal reachLink   `json:"portal"`
	AI     *reachLink  `json:"ai,omitempty"`
	Apps   []reachLink `json:"apps"`
	// Port is the internet port when it isn't 443 (8443: the router's 443 goes elsewhere).
	Port int `json:"port,omitempty"`
	// Local are addresses that work on the home network even when names don't (the server's own IP, .local names).
	Local []string `json:"local"`
	// Files is how to open shared folders (smb://…), when there are some.
	Files []string `json:"files,omitempty"`
	Admin *reachAdmin `json:"admin,omitempty"`
}

type reachAdmin struct {
	AdminURL string `json:"adminURL"`
	SSH      string `json:"ssh"`
	SSHPort  int    `json:"sshPort"`
	PublicIP string `json:"publicIP,omitempty"`
	// Router is what the router must do for the internet ("outside port 8443 → 192.168.7.3 port 443").
	Router string `json:"router,omitempty"`
}

func (s *Server) reachRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/reach", s.browserUser(s.myReach))
}

func (s *Server) myReach(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	var out reach
	var main, ai string
	s.Store.Read(func(d *state.Data) {
		st := d.Settings()
		main, ai = st.MainDomain, d.AIDomain()
		if st.InternetPort != 0 && st.InternetPort != 443 {
			out.Port = st.InternetPort
		}
		mainOut := st.HomeOnInternet || !state.PrivateName(main) && !d.UnderHome(main)
		out.Portal = reachLink{Name: "Your apps (this page)", URL: "https://" + d.Address(main) + "/", Internet: mainOut}
		if ai != "" {
			out.AI = &reachLink{Name: "AI", URL: "https://" + d.Address(ai) + "/", Internet: mainOut}
		}
		out.Apps = []reachLink{}
		for _, a := range d.Apps() {
			if canOpen(u, a) {
				out.Apps = append(out.Apps, reachLink{Name: a.Name, URL: "https://" + d.AppAddress(a) + "/", Internet: d.OnInternet(a)})
			}
		}
		if u.Admin {
			out.Admin = &reachAdmin{AdminURL: "https://" + d.Address(main) + "/admin"}
		}
	})
	ips := s.localAddrs()
	out.Local = []string{}
	for _, ip := range ips {
		if strings.Count(ip, ".") == 3 {
			out.Local = append(out.Local, "https://"+ip+"/")
		}
	}
	host, _ := os.Hostname()
	host = strings.TrimSuffix(host, ".local")
	if s.Shares != nil {
		if list, _ := s.sharing(); len(list) > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			st := s.Shares.Status(ctx, nil)
			cancel()
			if st.Running {
				if host != "" {
					out.Files = append(out.Files, "smb://"+host+".local")
				}
				if len(ips) > 0 {
					out.Files = append(out.Files, `\\`+ips[0])
				}
			}
		}
	}
	if out.Admin != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		port := 22
		if s.Firewall != nil {
			port = s.Firewall.SSHPort(ctx)
		}
		target := host + ".local"
		if len(ips) > 0 {
			target = ips[0]
		}
		user := cmpOr(loginUser(), "your-login")
		out.Admin.SSHPort = port
		out.Admin.SSH = "ssh " + user + "@" + target
		if port != 22 {
			out.Admin.SSH = "ssh -p " + strconv.Itoa(port) + " " + user + "@" + target
		}
		if s.DynDNS != nil {
			out.Admin.PublicIP = s.DynDNS.PublicIPv4(ctx)
		}
		if len(ips) > 0 && (out.Portal.Internet || anyInternet(out.Apps)) {
			outside := 443
			if out.Port != 0 {
				outside = out.Port
			}
			out.Admin.Router = "outside port " + strconv.Itoa(outside) + " → " + ips[0] + " port 443"
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func anyInternet(list []reachLink) bool {
	for _, l := range list {
		if l.Internet {
			return true
		}
	}
	return false
}

// loginUser is the first person's login on this machine (uid 1000 and up, with a shell), for the SSH line.
func loginUser() string {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 {
			continue
		}
		uid, _ := strconv.Atoi(f[2])
		if uid >= 1000 && uid < 60000 && !strings.HasSuffix(f[6], "nologin") && !strings.HasSuffix(f[6], "false") {
			return f[0]
		}
	}
	return ""
}
