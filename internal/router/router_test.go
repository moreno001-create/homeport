package router

import (
	"context"
	"strings"
	"testing"
)

type fakeGW struct {
	maps  map[uint16]string // port → internal client
	added []string
}

func (f *fakeGW) AddPortMappingCtx(_ context.Context, _ string, ext uint16, _ string, _ uint16, client string, _ bool, _ string, _ uint32) error {
	f.maps[ext] = client
	f.added = append(f.added, client)
	return nil
}
func (f *fakeGW) GetSpecificPortMappingEntryCtx(_ context.Context, _ string, ext uint16, _ string) (uint16, string, bool, string, uint32, error) {
	c, ok := f.maps[ext]
	if !ok {
		return 0, "", false, "", 0, errNoEntry
	}
	return ext, c, true, "user port mapping", 0, nil
}
func (f *fakeGW) DeletePortMappingCtx(_ context.Context, _ string, ext uint16, _ string) error {
	delete(f.maps, ext)
	return nil
}
func (f *fakeGW) GetExternalIPAddressCtx(context.Context) (string, error) { return "47.1.2.3", nil }

type noEntry struct{}

func (noEntry) Error() string { return "NoSuchEntryInArray" }

var errNoEntry = noEntry{}

func TestOpenAndClose(t *testing.T) {
	gw := &fakeGW{maps: map[uint16]string{80: "192.168.6.237"}}
	r := &Router{Name: "eero", Local: "192.168.4.37", gw: gw}
	ctx := context.Background()
	if err := r.Open(ctx, 80); err == nil || !strings.Contains(err.Error(), "192.168.6.237") {
		t.Fatalf("a port another device has isn't taken over: %v", err)
	}
	if err := r.Open(ctx, 443); err != nil || !r.Where(ctx, 443).Mine {
		t.Fatalf("443: %v %+v", err, r.Where(ctx, 443))
	}
	_ = r.Close(ctx, 80) // not ours: left alone
	if gw.maps[80] != "192.168.6.237" {
		t.Fatal("closing must only remove this machine's forwarding")
	}
	_ = r.Close(ctx, 443)
	if _, ok := gw.maps[443]; ok {
		t.Fatal("443 should be closed")
	}
}

func TestShared(t *testing.T) {
	for addr, want := range map[string]bool{"100.70.1.2": true, "192.168.1.1": true, "10.0.0.1": true, "47.148.196.210": false, "": false} {
		if Shared(addr) != want {
			t.Errorf("Shared(%q) = %v", addr, !want)
		}
	}
}
