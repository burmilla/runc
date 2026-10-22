// +build linux

package libcontainer

import (
	"fmt"
	"net"

	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/vishvananda/netlink"
)

var strategies = map[string]networkStrategy{
	"veth":     &veth{},
	"loopback": &loopback{},
}

// networkStrategy represents a specific network configuration for
// a container's networking stack
type networkStrategy interface {
	create(*network, int) error
	initialize(*network) error
	detach(*configs.Network) error
	attach(*configs.Network) error
}

// getStrategy returns the specific network strategy for the
// provided type.
func getStrategy(tpe string) (networkStrategy, error) {
	s, exists := strategies[tpe]
	if !exists {
		return nil, fmt.Errorf("unknown strategy type %q", tpe)
	}
	return s, nil
}

// loopback is a network strategy that provides a basic loopback device
type loopback struct {
}

func (l *loopback) create(n *network, nspid int) error {
	return nil
}

func (l *loopback) initialize(config *network) error {
	return netlink.LinkSetUp(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: "lo"}})
}

func (l *loopback) attach(n *configs.Network) (err error) {
	return nil
}

func (l *loopback) detach(n *configs.Network) (err error) {
	return nil
}

// veth is a network strategy that uses a bridge and creates
// a veth pair, one that is attached to the bridge on the host and the other
// is placed inside the container's namespace
type veth struct {
}

func (v *veth) detach(n *configs.Network) (err error) {
	return netlink.LinkSetMaster(&netlink.Device{LinkAttrs: netlink.LinkAttrs{Name: n.HostInterfaceName}}, nil)
}

// attach a container network interface to an external network
func (v *veth) attach(n *configs.Network) (err error) {
	brl, err := netlink.LinkByName(n.Bridge)
	if err != nil {
		return err
	}
	br, ok := brl.(*netlink.Bridge)
	if !ok {
		return fmt.Errorf("Wrong device type %T", brl)
	}
	host, err := netlink.LinkByName(n.HostInterfaceName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMaster(host, br); err != nil {
		return err
	}
	if err := netlink.LinkSetMTU(host, n.Mtu); err != nil {
		return err
	}
	if n.HairpinMode {
		if err := netlink.LinkSetHairpin(host, true); err != nil {
			return err
		}
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return err
	}

	return nil
}

func (v *veth) create(n *network, nspid int) (err error) {
	tmpName, err := v.generateTempPeerName()
	if err != nil {
		return err
	}
	n.TempVethPeerName = tmpName
	if n.Bridge == "" {
		return fmt.Errorf("bridge is not specified")
	}
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:   n.HostInterfaceName,
			TxQLen: n.TxQueueLen,
		},
		PeerName: n.TempVethPeerName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			netlink.LinkDel(veth)
		}
	}()
	if err := v.attach(&n.Network); err != nil {
		return err
	}
	child, err := netlink.LinkByName(n.TempVethPeerName)
	if err != nil {
		return err
	}
	return netlink.LinkSetNsPid(child, nspid)
}

func (v *veth) generateTempPeerName() (string, error) {
	return utils.GenerateRandomName("veth", 7)
}

func (v *veth) initialize(config *network) error {
	peer := config.TempVethPeerName
	if peer == "" {
		return fmt.Errorf("peer is not specified")
	}
	child, err := netlink.LinkByName(peer)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetDown(child); err != nil {
		return err
	}
	if err := netlink.LinkSetName(child, config.Name); err != nil {
		return err
	}
	// get the interface again after we changed the name as the index also changes.
	if child, err = netlink.LinkByName(config.Name); err != nil {
		return err
	}
	if config.MacAddress != "" {
		mac, err := net.ParseMAC(config.MacAddress)
		if err != nil {
			return err
		}
		if err := netlink.LinkSetHardwareAddr(child, mac); err != nil {
			return err
		}
	}
	ip, err := netlink.ParseAddr(config.Address)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(child, ip); err != nil {
		return err
	}
	if config.IPv6Address != "" {
		ip6, err := netlink.ParseAddr(config.IPv6Address)
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(child, ip6); err != nil {
			return err
		}
	}
	if err := netlink.LinkSetMTU(child, config.Mtu); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(child); err != nil {
		return err
	}
	if config.Gateway != "" {
		gw := net.ParseIP(config.Gateway)
		if err := netlink.RouteAdd(&netlink.Route{
			Scope:     netlink.SCOPE_UNIVERSE,
			LinkIndex: child.Attrs().Index,
			Gw:        gw,
		}); err != nil {
			return err
		}
	}
	if config.IPv6Gateway != "" {
		gw := net.ParseIP(config.IPv6Gateway)
		if err := netlink.RouteAdd(&netlink.Route{
			Scope:     netlink.SCOPE_UNIVERSE,
			LinkIndex: child.Attrs().Index,
			Gw:        gw,
		}); err != nil {
			return err
		}
	}
	return nil
}
