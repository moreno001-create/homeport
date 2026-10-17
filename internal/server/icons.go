package server

import (
	"io/fs"
	"path"
	"strings"
)

// MARK: App icons
//
// Logos for the catalog's apps (from the dashboard-icons project, Apache-2.0) are built into Homeport, so the portal and
// admin panel show them without loading anything from elsewhere. Apps without one get their initial.

var icons = func() map[string]string {
	m := map[string]string{}
	entries, _ := fs.ReadDir(webFiles, "web/assets/icons")
	for _, e := range entries {
		m[strings.TrimSuffix(e.Name(), path.Ext(e.Name()))] = "/assets/icons/" + e.Name()
	}
	return m
}()

// iconFor is the address of a catalog app's logo, or "" when there isn't one.
func iconFor(catalog string) string { return icons[catalog] }
