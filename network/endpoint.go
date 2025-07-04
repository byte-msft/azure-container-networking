// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package network

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/Azure/azure-container-networking/cni/log"
	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/netio"
	"github.com/Azure/azure-container-networking/netlink"
	"github.com/Azure/azure-container-networking/network/policy"
	"github.com/Azure/azure-container-networking/platform"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	InfraVnet = 0
)

var logger = log.CNILogger.With(zap.String("component", "net"))

type AzureHNSEndpoint struct{}

// Endpoint represents a container network interface.
type endpoint struct {
	Id                       string
	HnsId                    string `json:",omitempty"`
	HNSNetworkID             string `json:",omitempty"`
	SandboxKey               string
	IfName                   string
	HostIfName               string
	MacAddress               net.HardwareAddr
	InfraVnetIP              net.IPNet
	LocalIP                  string
	IPAddresses              []net.IPNet
	Gateways                 []net.IP
	DNS                      DNSInfo
	Routes                   []RouteInfo
	VlanID                   int
	EnableSnatOnHost         bool
	EnableInfraVnet          bool
	EnableMultitenancy       bool
	AllowInboundFromHostToNC bool
	AllowInboundFromNCToHost bool
	NetworkContainerID       string
	NetworkNameSpace         string `json:",omitempty"`
	ContainerID              string
	PODName                  string `json:",omitempty"`
	PODNameSpace             string `json:",omitempty"`
	InfraVnetAddressSpace    string `json:",omitempty"`
	NetNs                    string `json:",omitempty"` // used in windows
	// SecondaryInterfaces is a map of interface name to InterfaceInfo
	SecondaryInterfaces map[string]*InterfaceInfo
	// Store nic type since we no longer populate SecondaryInterfaces
	NICType cns.NICType
}

// EndpointInfo contains read-only information about an endpoint.
type EndpointInfo struct {
	EndpointID               string
	ContainerID              string
	NetNsPath                string
	IfName                   string // value differs during creation vs. deletion flow; used in statefile, not necessarily the nic name
	SandboxKey               string
	IfIndex                  int
	MacAddress               net.HardwareAddr
	EndpointDNS              DNSInfo
	IPAddresses              []net.IPNet
	IPsToRouteViaHost        []string
	InfraVnetIP              net.IPNet
	Routes                   []RouteInfo
	EndpointPolicies         []policy.Policy // used in windows
	NetworkPolicies          []policy.Policy // used in windows
	Gateways                 []net.IP
	EnableSnatOnHost         bool
	EnableInfraVnet          bool
	EnableMultiTenancy       bool
	EnableSnatForDns         bool
	AllowInboundFromHostToNC bool
	AllowInboundFromNCToHost bool
	NetworkContainerID       string
	PODName                  string
	PODNameSpace             string
	Data                     map[string]interface{}
	InfraVnetAddressSpace    string
	SkipHotAttachEp          bool
	IPV6Mode                 string
	VnetCidrs                string
	ServiceCidrs             string
	NATInfo                  []policy.NATInfo // windows only
	NICType                  cns.NICType
	SkipDefaultRoutes        bool
	HNSEndpointID            string
	HNSNetworkID             string
	HostIfName               string // unused in windows, and in linux
	// Fields related to the network are below
	MasterIfName                  string
	AdapterName                   string
	NetworkID                     string
	Mode                          string
	Subnets                       []SubnetInfo
	BridgeName                    string
	NetNs                         string // used in windows
	Options                       map[string]interface{}
	DisableHairpinOnHostInterface bool
	IsIPv6Enabled                 bool
	HostSubnetPrefix              string // can be used later to add an external interface
	PnPID                         string
}

// RouteInfo contains information about an IP route.
type RouteInfo struct {
	Dst      net.IPNet
	Src      net.IP
	Gw       net.IP
	Protocol int
	DevName  string
	Scope    int
	Priority int
	Table    int
}

// InterfaceInfo contains information for secondary interfaces
type InterfaceInfo struct {
	Name              string
	MacAddress        net.HardwareAddr
	IPConfigs         []*IPConfig
	Routes            []RouteInfo
	DNS               DNSInfo
	NICType           cns.NICType
	SkipDefaultRoutes bool
	HostSubnetPrefix  net.IPNet // Move this field from ipamAddResult
	NCResponse        *cns.GetNetworkContainerResponse
	PnPID             string
	EndpointPolicies  []policy.Policy
}

type IPConfig struct {
	Address net.IPNet
	Gateway net.IP
}

type apipaClient interface {
	DeleteHostNCApipaEndpoint(ctx context.Context, networkContainerID string) error
	CreateHostNCApipaEndpoint(ctx context.Context, networkContainerID string) (string, error)
}

// FormatSliceOfPointersToString takes in a slice of pointers, and for each pointer, dereferences the pointer if not nil
// and then formats it to its string representation, returning a string where each line is a separate item in the slice.
// This is used for convenience to get a string representation of the actual structs and their fields
// in slices of pointers since the default string representation of a slice of pointers is a list of memory addresses.
func FormatSliceOfPointersToString[T any](slice []*T) string {
	var builder strings.Builder
	for _, ptr := range slice {
		if ptr != nil {
			fmt.Fprintf(&builder, "%+v \n", *ptr)
		}
	}
	return builder.String()
}

func (epInfo *EndpointInfo) PrettyString() string {
	return fmt.Sprintf("EndpointID:%s ContainerID:%s NetNsPath:%s IfName:%s IfIndex:%d MacAddr:%s IPAddrs:%v Gateways:%v Data:%+v NICType: %s "+
		"NetworkContainerID: %s HostIfName: %s NetNs: %s Options: %v MasterIfName: %s HNSEndpointID: %s HNSNetworkID: %s",
		epInfo.EndpointID, epInfo.ContainerID, epInfo.NetNsPath, epInfo.IfName, epInfo.IfIndex, epInfo.MacAddress.String(), epInfo.IPAddresses,
		epInfo.Gateways, epInfo.Data, epInfo.NICType, epInfo.NetworkContainerID, epInfo.HostIfName, epInfo.NetNs, epInfo.Options, epInfo.MasterIfName,
		epInfo.HNSEndpointID, epInfo.HNSNetworkID)
}

func (ifInfo *InterfaceInfo) PrettyString() string {
	var ncresponse string
	if ifInfo.NCResponse != nil {
		ncresponse = fmt.Sprintf("%+v", *ifInfo.NCResponse)
	}
	return fmt.Sprintf("Name:%s NICType:%v MacAddr:%s IPConfigs:%s Routes:%+v DNSInfo:%+v NCResponse: %s",
		ifInfo.Name, ifInfo.NICType, ifInfo.MacAddress.String(), FormatSliceOfPointersToString(ifInfo.IPConfigs), ifInfo.Routes, ifInfo.DNS, ncresponse)
}

// NewEndpoint creates a new endpoint in the network.
func (nw *network) newEndpoint(
	apipaCli apipaClient,
	nl netlink.NetlinkInterface,
	plc platform.ExecClient,
	netioCli netio.NetIOInterface,
	nsc NamespaceClientInterface,
	iptc ipTablesClient,
	dhcpc dhcpClient,
	epInfo *EndpointInfo,
) (*endpoint, error) {
	var ep *endpoint
	var err error

	defer func() {
		if err != nil {
			logger.Error("Failed to create endpoint with err", zap.String("id", epInfo.EndpointID), zap.Error(err))
		}
	}()

	// Call the platform implementation.
	// Pass nil for epClient and will be initialized in newendpointImpl
	ep, err = nw.newEndpointImpl(apipaCli, nl, plc, netioCli, nil, nsc, iptc, dhcpc, epInfo)
	if err != nil {
		return nil, err
	}

	nw.Endpoints[ep.Id] = ep
	logger.Info("Created endpoint. Num of endpoints", zap.Any("ep", ep), zap.Int("numEndpoints", len(nw.Endpoints)))

	return ep, nil
}

// DeleteEndpoint deletes an existing endpoint from the network.
func (nw *network) deleteEndpoint(nl netlink.NetlinkInterface, plc platform.ExecClient, nioc netio.NetIOInterface, nsc NamespaceClientInterface,
	iptc ipTablesClient, dhcpc dhcpClient, endpointID string,
) error {
	var err error

	logger.Info("Deleting endpoint from network", zap.String("endpointID", endpointID), zap.String("id", nw.Id))
	defer func() {
		if err != nil {
			logger.Error("Failed to delete endpoint with", zap.String("endpointID", endpointID), zap.Error(err))
		}
	}()

	// Look up the endpoint.
	ep, err := nw.getEndpoint(endpointID)
	if err != nil {
		logger.Error("Endpoint not found. Not Returning error", zap.String("endpointID", endpointID), zap.Error(err))
		return nil
	}

	// Call the platform implementation.
	// Pass nil for epClient and will be initialized in deleteEndpointImpl
	err = nw.deleteEndpointImpl(nl, plc, nil, nioc, nsc, iptc, dhcpc, ep)
	if err != nil {
		return err
	}

	// Remove the endpoint object.
	delete(nw.Endpoints, endpointID)
	logger.Info("Deleted endpoint. Num of endpoints", zap.Any("ep", ep), zap.Int("numEndpoints", len(nw.Endpoints)))
	return nil
}

// GetEndpoint returns the endpoint with the given ID.
func (nw *network) getEndpoint(endpointId string) (*endpoint, error) {
	ep := nw.Endpoints[endpointId]

	if ep == nil {
		return nil, errEndpointNotFound
	}

	return ep, nil
}

// GetEndpointByPOD returns the endpoint with the given ID.
func (nw *network) getEndpointByPOD(podName string, podNameSpace string, doExactMatchForPodName bool) (*endpoint, error) {
	logger.Info("Trying to retrieve endpoint for pod name in namespace", zap.String("podName", podName), zap.String("podNameSpace", podNameSpace))

	var ep *endpoint

	for _, endpoint := range nw.Endpoints {
		if podNameMatches(endpoint.PODName, podName, doExactMatchForPodName) && endpoint.PODNameSpace == podNameSpace {
			if ep == nil {
				ep = endpoint
			} else {
				return nil, errMultipleEndpointsFound
			}
		}
	}

	if ep == nil {
		return nil, errEndpointNotFound
	}

	return ep, nil
}

func podNameMatches(source string, actualValue string, doExactMatch bool) bool {
	if doExactMatch {
		return source == actualValue
	} else {
		// If exact match flag is disabled we just check if the existing podname field for an endpoint
		// starts with passed podname string.
		return actualValue == GetPodNameWithoutSuffix(source)
	}
}

//
// Endpoint
//

// GetInfo returns information about the endpoint.
func (ep *endpoint) getInfo() *EndpointInfo {
	info := &EndpointInfo{
		EndpointID:               ep.Id,
		IPAddresses:              ep.IPAddresses,
		InfraVnetIP:              ep.InfraVnetIP,
		Data:                     make(map[string]interface{}),
		MacAddress:               ep.MacAddress,
		SandboxKey:               ep.SandboxKey,
		IfIndex:                  0, // Azure CNI supports only one interface
		EndpointDNS:              ep.DNS,
		EnableSnatOnHost:         ep.EnableSnatOnHost,
		EnableInfraVnet:          ep.EnableInfraVnet,
		EnableMultiTenancy:       ep.EnableMultitenancy,
		AllowInboundFromHostToNC: ep.AllowInboundFromHostToNC,
		AllowInboundFromNCToHost: ep.AllowInboundFromNCToHost,
		IfName:                   ep.IfName,
		ContainerID:              ep.ContainerID,
		NetNsPath:                ep.NetworkNameSpace,
		PODName:                  ep.PODName,
		PODNameSpace:             ep.PODNameSpace,
		NetworkContainerID:       ep.NetworkContainerID,
		HNSEndpointID:            ep.HnsId,
		HostIfName:               ep.HostIfName,
		NICType:                  ep.NICType,
	}

	info.Routes = append(info.Routes, ep.Routes...)

	info.Gateways = append(info.Gateways, ep.Gateways...)

	// Call the platform implementation.
	ep.getInfoImpl(info)

	return info
}

// Attach attaches an endpoint to a sandbox.
func (ep *endpoint) attach(sandboxKey string) error {
	if ep.SandboxKey != "" {
		return errEndpointInUse
	}

	ep.SandboxKey = sandboxKey

	logger.Info("Attached endpoint to sandbox", zap.String("id", ep.Id), zap.String("sandboxKey", sandboxKey))

	return nil
}

// Detach detaches an endpoint from its sandbox.
func (ep *endpoint) detach() error {
	if ep.SandboxKey == "" {
		return errEndpointNotInUse
	}

	logger.Info("Detached endpoint from sandbox", zap.String("id", ep.Id), zap.String("sandboxKey", ep.SandboxKey))

	ep.SandboxKey = ""

	return nil
}

// updateEndpoint updates an existing endpoint in the network.
func (nm *networkManager) updateEndpoint(nw *network, existingEpInfo, targetEpInfo *EndpointInfo) error {
	var err error

	logger.Info("Updating existing endpoint in network to target", zap.Any("existingEpInfo", existingEpInfo),
		zap.String("id", nw.Id), zap.Any("targetEpInfo", targetEpInfo))
	defer func() {
		if err != nil {
			logger.Error("Failed to update endpoint with err", zap.String("id", existingEpInfo.EndpointID), zap.Error(err))
		}
	}()

	logger.Info("Trying to retrieve endpoint id", zap.String("id", existingEpInfo.EndpointID))

	ep := nw.Endpoints[existingEpInfo.EndpointID]
	if ep == nil {
		return errEndpointNotFound
	}

	logger.Info("Retrieved endpoint to update", zap.Any("ep", ep))

	// Call the platform implementation.
	ep, err = nm.updateEndpointImpl(nw, existingEpInfo, targetEpInfo)
	if err != nil {
		return err
	}

	// Update routes for existing endpoint
	nw.Endpoints[existingEpInfo.EndpointID].Routes = ep.Routes

	return nil
}

func GetPodNameWithoutSuffix(podName string) string {
	nameSplit := strings.Split(podName, "-")
	if len(nameSplit) > 2 {
		nameSplit = nameSplit[:len(nameSplit)-2]
	} else {
		return podName
	}
	return strings.Join(nameSplit, "-")
}

// IsEndpointStateInComplete returns true if both HNSEndpointID and HostVethName are missing.
func (epInfo *EndpointInfo) IsEndpointStateIncomplete() bool {
	if epInfo.HNSEndpointID == "" && epInfo.HostIfName == "" {
		return true
	}
	return false
}

func (ep *endpoint) validateEndpoint() error {
	if ep.ContainerID == "" || ep.NICType == "" {
		return errors.New("endpoint struct must contain a container id and nic type")
	}
	return nil
}

func validateEndpoints(eps []*endpoint) error {
	containerIDs := map[string]bool{}
	for _, ep := range eps {
		if err := ep.validateEndpoint(); err != nil {
			return errors.Wrap(err, "failed to validate endpoint struct")
		}
		containerIDs[ep.ContainerID] = true

		if len(containerIDs) != 1 {
			return errors.New("multiple distinct container ids detected")
		}
	}
	return nil
}
