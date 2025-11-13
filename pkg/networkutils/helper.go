package networkutils

import (
	"net"
)

// BaseNumber is the base offset for multi-NIC route table IDs.
// This value is chosen to match the logic in Amazon EC2 net utils:
// https://github.com/amazonlinux/amazon-ec2-net-utils/blob/v2.7.1/lib/lib.sh#L301
const BaseNumber = 10000

func CalculateOldRouteTableId(deviceNumber int, networkCardIndex int, maxENIsPerNetworkCard int) int {
	return deviceNumber + 1 + (networkCardIndex * maxENIsPerNetworkCard)
}

func CalculateRouteTableId(deviceNumber int, networkCardIndex int) int {
	// Note: This function calculates routing table IDs for pod IP routing rules.
	// The primary ENI (deviceNumber 0) previously used RT_TABLE_MAIN (254), which
	// caused issues because the main table's routes include "src" parameters that
	// override the source IP for outgoing packets. This breaks connectivity for
	// pods using secondary IPs on the primary ENI.
	//
	// By assigning routing table 1 to the primary ENI (like we do for other ENIs),
	// we ensure proper source-based routing for all pod IPs.
	//
	// IMPORTANT: This does NOT affect the node's own networking. The node's primary
	// IP continues to use RT_TABLE_MAIN through normal Linux routing, since these
	// routing table IDs are only used for pod IP policy routing rules.
	if networkCardIndex == 0 {
		// Primary network card: assign table numbers starting from 1
		// deviceNumber 0 (primary ENI) -> table 1
		// deviceNumber 1 (secondary ENI) -> table 2
		// deviceNumber 2 (tertiary ENI) -> table 3, etc.
		return deviceNumber + 1
	} else {
		// Secondary network cards: use offset-based numbering
		return BaseNumber + deviceNumber + (100 * networkCardIndex)
	}
}

func CalculatePodIPv4GatewayIP(index int) net.IP {
	return net.IPv4(169, 254, 1, byte(index)+1)
}

func CalculatePodIPv6GatewayIP(index int) net.IP {
	return net.IP{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(index) + 1}
}

func IsIPv4(ip net.IP) bool {
	return ip.To4() != nil
}
