package common

import (
	"fmt"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"io/ioutil"
	"net"
	"os"
	"runtime"
	"strings"
)

// Assert test is true, panic otherwise
func Assert(test bool) {
	if !test {
		panic("Assertion failure")
	}
}

func ErrorMessages(errors []error) string {
	var result []string
	for _, err := range errors {
		result = append(result, err.Error())
	}
	return strings.Join(result, "\n")
}

func WithNetNS(ns netns.NsHandle, work func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	oldNs, err := netns.Get()
	if err == nil {
		defer oldNs.Close()

		err = netns.Set(ns)
		if err == nil {
			defer netns.Set(oldNs)

			err = work()
		}
	}

	return err
}

type NetDev struct {
	Name  string
	MAC   net.HardwareAddr
	CIDRs []*net.IPNet
}

// Search the network namespace of a process for interfaces matching a predicate
func FindNetDevs(processID int, match func(link netlink.Link) bool) ([]NetDev, error) {
	var netDevs []NetDev

	ns, err := netns.GetFromPid(processID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer ns.Close()

	err = WithNetNS(ns, func() error {
		return forEachLink(func(link netlink.Link) error {
			if match(link) {
				netDev, err := linkToNetDev(link)
				if err != nil {
					return err
				}
				netDevs = append(netDevs, *netDev)
			}
			return nil
		})
	})

	return netDevs, err
}

func forEachLink(f func(netlink.Link) error) error {
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		if err := f(link); err != nil {
			return err
		}
	}
	return nil
}

func linkToNetDev(link netlink.Link) (*NetDev, error) {
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}

	netDev := &NetDev{Name: link.Attrs().Name, MAC: link.Attrs().HardwareAddr}
	for _, addr := range addrs {
		netDev.CIDRs = append(netDev.CIDRs, addr.IPNet)
	}
	return netDev, nil
}

// Lookup the weave interface of a container
func GetWeaveNetDevs(processID int) ([]NetDev, error) {
	// Bail out if this container is running in the root namespace
	nsToplevel, err := netns.GetFromPid(1)
	if err != nil {
		return nil, fmt.Errorf("unable to open root namespace: %s", err)
	}
	nsContainr, err := netns.GetFromPid(processID)
	if err != nil {
		return nil, fmt.Errorf("unable to open process %d namespace: %s", processID, err)
	}
	if nsToplevel.Equal(nsContainr) {
		return nil, nil
	}

	weaveBridge, err := netlink.LinkByName("weave")
	if err != nil {
		return nil, fmt.Errorf("Cannot find weave bridge: %s", err)
	}
	// Scan devices in root namespace to find those attached to weave bridge
	indexes := make(map[int]struct{})
	err = forEachLink(func(link netlink.Link) error {
		if link.Attrs().MasterIndex == weaveBridge.Attrs().Index {
			peerIndex := link.Attrs().ParentIndex
			if peerIndex == 0 {
				// perhaps running on an older kernel where ParentIndex doesn't work.
				// as fall-back, assume the indexes are consecutive
				peerIndex = link.Attrs().Index - 1
			}
			indexes[peerIndex] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return FindNetDevs(processID, func(link netlink.Link) bool {
		_, isveth := link.(*netlink.Veth)
		_, found := indexes[link.Attrs().Index]
		return isveth && found
	})
}

// Get the weave bridge interface
func GetBridgeNetDev(bridgeName string) ([]NetDev, error) {
	return FindNetDevs(1, func(link netlink.Link) bool {
		return link.Attrs().Name == bridgeName
	})
}

func EnforceDockerBridgeAddrAssignType(bridgeName string) error {
	addrAssignType, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/addr_assign_type", bridgeName))
	if err != nil {
		return err
	}

	// From include/uapi/linux/netdevice.h
	// #define NET_ADDR_PERM       0   /* address is permanent (default) */
	// #define NET_ADDR_RANDOM     1   /* address is generated randomly */
	// #define NET_ADDR_STOLEN     2   /* address is stolen from other device */
	// #define NET_ADDR_SET        3   /* address is set using dev_set_mac_address() */
	if string(addrAssignType) != "3" {
		link, err := netlink.LinkByName(bridgeName)
		if err != nil {
			return err
		}

		mac, err := RandomMAC()
		if err != nil {
			return err
		}

		if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
			return err
		}
	}

	return nil
}
