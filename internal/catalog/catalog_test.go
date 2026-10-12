package catalog

import (
	"strings"
	"testing"
)

// Every manifest renders to a valid compose file that only listens on 127.0.0.1.
func TestEveryAppRenders(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("only %d apps in the catalog", len(all))
	}
	for _, a := range all {
		secrets := map[string]string{}
		for _, s := range a.AllSecrets() {
			secrets[s] = "s3cret-" + s
		}
		v := Vars{ID: a.ID, Domain: a.ID + ".example.com", Port: 18000, Data: "/var/lib/homeport/apps/" + a.ID + "/data",
			Secrets: secrets, AdminUser: "alice", TimeZone: "UTC", MainDomain: "home.example.com", Issuer: "https://home.example.com",
			ClientID: a.ID, CABundle: "/var/lib/homeport/ca-bundle.crt", CALocal: "/var/lib/homeport/ca-local.crt", HostIP: "192.168.4.27"}
		for _, step := range a.AfterInstall {
			run, err := a.RenderStep(step, v)
			if err != nil {
				t.Errorf("%s: step %v: %v", a.ID, step.Run, err)
			}
			for _, arg := range run {
				if strings.Contains(arg, "{{") || strings.Contains(arg, "<no value>") {
					t.Errorf("%s: step argument not filled in: %q", a.ID, arg)
				}
			}
		}
		if a.OIDC != nil && len(a.OIDC.RedirectPaths) == 0 {
			t.Errorf("%s: OIDC needs at least one redirect path", a.ID)
		}
		out, err := a.Render(v)
		if err != nil {
			t.Errorf("%s: %v", a.ID, err)
			continue
		}
		if !strings.Contains(out, `"127.0.0.1:18000:`) {
			t.Errorf("%s: its port must be published on 127.0.0.1 only:\n%s", a.ID, out)
		}
		if strings.Contains(out, "<no value>") {
			t.Errorf("%s: a template value is missing", a.ID)
		}
		if notes := a.RenderNotes(v); strings.Contains(notes, "{{") || strings.Contains(notes, "<no value>") {
			t.Errorf("%s: notes not filled in: %q", a.ID, notes)
		}
		if a.Name == "" || a.Description == "" {
			t.Errorf("%s: needs a name and a description", a.ID)
		}
	}
}

func TestUnknownSecretIsAnError(t *testing.T) {
	a := App{ID: "x", Compose: "services:\n  app:\n    image: \"{{secret \"nope\"}}\"\n"}
	if _, err := a.Render(Vars{Secrets: map[string]string{}}); err == nil {
		t.Fatal("a missing secret must fail, not render empty")
	}
}
