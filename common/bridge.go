package common

import "fmt"
import "github.com/vishvananda/netlink"
import "github.com/weaveworks/weave/common/odp"
import "github.com/coreos/go-iptables/iptables"

type BridgeType int

const (
	None BridgeType = iota
	Bridge
	Fastdp
	BridgedFastdp
	Inconsistent
)

// Returns a string that is consistent with the weave script
func (t BridgeType) String() string {
	switch t {
	case None:
		return "none"
	case Bridge:
		return "bridge"
	case Fastdp:
		return "fastdp"
	case BridgedFastdp:
		return "bridged_fastdp"
	case Inconsistent:
		return "inconsistent"
	}
	return "unknown"
}

type BridgeConfig struct {
	DockerBridgeName string
	WeaveBridgeName  string
	DatapathName     string
	NoFastdp         bool
	NoBridgedFastdp  bool
	MTU              int
	Port             int
}

func CreateBridge(config *BridgeConfig) (BridgeType, error) {
	bridgeType := DetectBridgeType(config)

	if bridgeType == None {
		bridgeType = Bridge
		if !config.NoFastdp {
			bridgeType = BridgedFastdp
			if config.NoBridgedFastdp {
				bridgeType = Fastdp
				config.DatapathName = config.WeaveBridgeName
			}
			odpSupported, err := odp.CreateDatapath(config.DatapathName)
			if err != nil {
				return None, err
			}
			if !odpSupported {
				bridgeType = Bridge
			}
		}

		var err error
		switch bridgeType {
		case Bridge:
			err = initBridge(config)
		case Fastdp:
			err = initFastdp(config)
		case BridgedFastdp:
			err = initBridgedFastdp(config)
		default:
			err = fmt.Errorf("Cannot initialise bridge type %v", bridgeType)
		}
		if err != nil {
			return None, err
		}

		if err = configureIPTables(config); err != nil {
			return bridgeType, err
		}
	}

	if bridgeType == Bridge {
		if err := EthtoolTXOff(config.WeaveBridgeName); err != nil {
			return bridgeType, err
		}
	}

	if err := linkSetUpByName(config.WeaveBridgeName); err != nil {
		return bridgeType, err
	}

	if err := ConfigureARPCache(config.WeaveBridgeName); err != nil {
		return bridgeType, err
	}

	return bridgeType, nil
}

func DetectBridgeType(config *BridgeConfig) BridgeType {
	bridge, _ := netlink.LinkByName(config.WeaveBridgeName)
	datapath, _ := netlink.LinkByName(config.DatapathName)

	switch {
	case bridge == nil && datapath == nil:
		return None
	case isBridge(bridge) && datapath == nil:
		return Bridge
	case isDatapath(bridge) && datapath == nil:
		return Fastdp
	case isDatapath(datapath) && isBridge(bridge):
		return BridgedFastdp
	default:
		return Inconsistent
	}
}

func isBridge(link netlink.Link) bool {
	_, isBridge := link.(*netlink.Bridge)
	return isBridge
}

func isDatapath(link netlink.Link) bool {
	switch link.(type) {
	case *netlink.GenericLink:
		return link.Type() == "openvswitch"
	case *netlink.Device:
		return true
	default:
		return false
	}
}

func initBridge(config *BridgeConfig) error {
	/* Derive the bridge MAC from the system (aka bios) UUID, or,
	   failing that, the hypervisor UUID. Elsewhere we in turn derive
	   the peer name from that, which we want to be stable across
	   reboots but otherwise unique. The system/hypervisor UUID fits
	   that bill, unlike, say, /etc/machine-id, which is often
	   identical on VMs created from cloned filesystems. If we cannot
	   determine the system/hypervisor UUID we just generate a random MAC. */
	mac, err := PersistentMAC("/sys/class/dmi/id/product_uuid")
	if err != nil {
		mac, err = PersistentMAC("/sys/hypervisor/uuid")
		if err != nil {
			mac, err = RandomMAC()
			if err != nil {
				return err
			}
		}
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = config.WeaveBridgeName
	linkAttrs.HardwareAddr = mac
	mtu := config.MTU
	if mtu == 0 {
		mtu = 65535
	}
	linkAttrs.MTU = mtu // TODO this probably doesn't work - see weave script
	if err = netlink.LinkAdd(&netlink.Bridge{LinkAttrs: linkAttrs}); err != nil {
		return err
	}

	return nil
}

func initFastdp(config *BridgeConfig) error {
	datapath, err := netlink.LinkByName(config.DatapathName)
	if err != nil {
		return err
	}
	mtu := config.MTU
	if mtu == 0 {
		/* GCE has the lowest underlay network MTU we're likely to encounter on
		   a local network, at 1460 bytes.  To get the overlay MTU from that we
		   subtract 20 bytes for the outer IPv4 header, 8 bytes for the outer
		   UDP header, 8 bytes for the vxlan header, and 14 bytes for the inner
		   ethernet header. */
		mtu = 1410
	}
	return netlink.LinkSetMTU(datapath, mtu)
}

func initBridgedFastdp(config *BridgeConfig) error {
	if err := initFastdp(config); err != nil {
		return err
	}
	if err := initBridge(config); err != nil {
		return err
	}

	link := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: "vethwe-bridge",
			MTU:  config.MTU},
		PeerName: "vethwe-datapath",
	}

	if err := netlink.LinkAdd(link); err != nil {
		return err
	}

	bridge, err := netlink.LinkByName(config.WeaveBridgeName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMasterByIndex(link, bridge.Attrs().Index); err != nil {
		return err
	}

	if err := odp.AddDatapathInterface(config.DatapathName, "vethwe-datapath"); err != nil {
		return err
	}

	if err := linkSetUpByName(config.DatapathName); err != nil {
		return err
	}

	return nil
}

// Add a rule to iptables, if it doesn't exist already
func addIPTablesRule(ipt *iptables.IPTables, table, chain string, rulespec ...string) error {
	exists, err := ipt.Exists(table, chain, rulespec...)
	if err == nil && !exists {
		err = ipt.Append(table, chain, rulespec...)
	}
	return err
}

func configureIPTables(config *BridgeConfig) error {
	ipt, err := iptables.New()
	if err != nil {
		return err
	}
	if config.WeaveBridgeName != config.DockerBridgeName {
		err := ipt.Insert("filter", "FORWARD", 1, "-i", config.DockerBridgeName, "-o", config.WeaveBridgeName, "-j", "DROP")
		if err != nil {
			return err
		}
	}

	dockerBridgeIP, err := DeviceIP(config.DockerBridgeName)
	if err != nil {
		return err
	}

	// forbid traffic to the Weave port from other containers
	if err = addIPTablesRule(ipt, "filter", "INPUT", "-i", config.DockerBridgeName, "-p", "tcp", "--dst", dockerBridgeIP.String(), "--dport", fmt.Sprint(config.Port), "-j", "DROP"); err != nil {
		return err
	}
	if err = addIPTablesRule(ipt, "filter", "INPUT", "-i", config.DockerBridgeName, "-p", "udp", "--dst", dockerBridgeIP.String(), "--dport", fmt.Sprint(config.Port), "-j", "DROP"); err != nil {
		return err
	}
	if err = addIPTablesRule(ipt, "filter", "INPUT", "-i", config.DockerBridgeName, "-p", "udp", "--dst", dockerBridgeIP.String(), "--dport", fmt.Sprint(config.Port+1), "-j", "DROP"); err != nil {
		return err
	}

	// let DNS traffic to weaveDNS, since otherwise it might get blocked by the likes of UFW
	if err = addIPTablesRule(ipt, "filter", "INPUT", "-i", config.DockerBridgeName, "-p", "udp", "--dport", "53", "-j", "ACCEPT"); err != nil {
		return err
	}
	if err = addIPTablesRule(ipt, "filter", "INPUT", "-i", config.DockerBridgeName, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"); err != nil {
		return err
	}

	// Work around the situation where there are no rules allowing traffic
	// across our bridge. E.g. ufw
	if err = addIPTablesRule(ipt, "filter", "FORWARD", "-i", config.WeaveBridgeName, "-o", config.WeaveBridgeName, "-j", "ACCEPT"); err != nil {
		return err
	}

	// create a chain for masquerading
	ipt.NewChain("nat", "WEAVE")
	if err = addIPTablesRule(ipt, "nat", "POSTROUTING", "-j", "WEAVE"); err != nil {
		return err
	}

	return nil
}

func linkSetUpByName(linkName string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return err
	}
	return netlink.LinkSetUp(link)
}
