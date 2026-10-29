package apps

import (
	"testing"

	"homeport/internal/catalog"
)

// Every catalog app can be installed under its own id (an id like "13ft" was refused at install).
func TestCatalogIDsAreValid(t *testing.T) {
	all, err := catalog.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if !idRe.MatchString(a.ID) {
			t.Errorf("catalog app %q (%s) has an id installs refuse", a.ID, a.Name)
		}
	}
}
