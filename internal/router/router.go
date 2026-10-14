// Package router opens ports on the home router through UPnP (Internet Gateway Device), the way most home routers let
// devices ask for port forwarding: Homeport asks for 80 and 443 to come to it, so people don't have to find the
// router's port forwarding settings.
package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

// gateway is what Homeport needs from a router; goupnp's three kinds of WAN connection all have it.
type gateway interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16,
		internalClient string, enabled bool, description string, leaseDuration uint32) error
	GetSpecificPortMappingEntryCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) (
		internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32, err error)
	DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
}

// Router is the home router, found on the network.
type Router struct {
	Name  string // "NETGEAR RAX50", say
	Local string // this machine's address, as the router sees it
	gw    gateway
}

// Find looks for a router that takes port forwarding requests (UPnP). It takes a few seconds.
func Find(ctx context.Context) (*Router, error) {
	pick := func(sc goupnp.ServiceClient, gw gateway) *Router {
		r := &Router{gw: gw}
		if sc.RootDevice != nil {
			d := sc.RootDevice.Device
			r.Name = strings.TrimSpace(strings.Join([]string{d.Manufacturer, d.ModelName}, " "))
			if r.Name == "" {
				r.Name = d.FriendlyName
			}
		}
		if ip := sc.LocalAddr(); ip != nil {
			r.Local = ip.String()
		}
		return r
	}
	if cs, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx); err == nil && len(cs) > 0 {
		return pick(cs[0].ServiceClient, cs[0]), nil
	}
	if cs, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(cs) > 0 {
		return pick(cs[0].ServiceClient, cs[0]), nil
	}
	if cs, _, err := internetgateway2.NewWANPPPConnection1ClientsCtx(ctx); err == nil && len(cs) > 0 {
		return pick(cs[0].ServiceClient, cs[0]), nil
	}
	return nil, errors.New("no router on this network takes port forwarding requests (UPnP is off on it, or it doesn't have it)")
}

// OutsideAddress is the router's own address on the internet side.
func (r *Router) OutsideAddress(ctx context.Context) (string, error) {
	return r.gw.GetExternalIPAddressCtx(ctx)
}

// Forward is where the router sends a port now ("" when it doesn't forward it).
type Forward struct {
	Port int    `json:"port"`
	To   string `json:"to,omitempty"` // the address it goes to
	Mine bool   `json:"mine"`         // it comes to this machine
	What string `json:"what,omitempty"`
}

// Where says where the router sends a TCP port.
func (r *Router) Where(ctx context.Context, port int) Forward {
	f := Forward{Port: port}
	_, client, enabled, desc, _, err := r.gw.GetSpecificPortMappingEntryCtx(ctx, "", uint16(port), "TCP")
	if err != nil || !enabled {
		return f
	}
	f.To, f.What, f.Mine = client, desc, client == r.Local
	return f
}

// Lease is how long a forwarding lasts before Homeport asks again (some routers refuse forwardings that never end).
const Lease = 3600

// Open asks the router to send a TCP port to this machine. A port the router already sends somewhere else isn't
// taken over.
func (r *Router) Open(ctx context.Context, port int) error { return r.OpenAs(ctx, port, port) }

// OpenAs asks the router to send outside port to this machine's inside port (8443 from outside to 443 here, when
// another machine has the router's 443).
func (r *Router) OpenAs(ctx context.Context, outside, inside int) error {
	if r.Local == "" {
		return errors.New("couldn't tell this machine's address on the home network")
	}
	if f := r.Where(ctx, outside); f.To != "" && !f.Mine {
		return fmt.Errorf("the router already sends port %d to %s (%s): change or remove that in its settings first", outside, f.To, f.What)
	}
	err := r.gw.AddPortMappingCtx(ctx, "", uint16(outside), "TCP", uint16(inside), r.Local, true, fmt.Sprintf("Homeport (%d)", outside), Lease)
	if err != nil {
		return fmt.Errorf("the router refused to forward port %d: %v", outside, err)
	}
	return nil
}

// Close stops the router sending a port here (only if it's this machine's forwarding).
func (r *Router) Close(ctx context.Context, port int) error {
	if f := r.Where(ctx, port); !f.Mine {
		return nil
	}
	return r.gw.DeletePortMappingCtx(ctx, "", uint16(port), "TCP")
}

// Shared says whether an address is one the internet can't reach directly: a private one (the router is behind
// another router) or one of the addresses providers share between many customers (100.64.0.0/10).
func Shared(addr string) bool {
	ip := net.ParseIP(addr)
	if ip == nil {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return ip.IsPrivate() || cgnat.Contains(ip) || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}
