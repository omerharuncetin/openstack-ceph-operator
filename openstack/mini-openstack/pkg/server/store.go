// Package server implements Keystone v3, Glance v2, Cinder v3, Neutron v2/OVN,
// and Nova v2.1 HTTP APIs backed by Ceph RBD via pkg/cephapi.
package server

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	DefaultProjectID   = "00000000-0000-4000-8000-000000000001"
	DefaultProjectName = "admin"
	DefaultUserID      = "00000000-0000-4000-8000-000000000002"
	DefaultUsername    = "admin"
	DefaultPassword    = "admin12345!"
	DefaultDomainID    = "default"
	DefaultDomainName  = "Default"
	DefaultRegion      = "RegionOne"
	DefaultAZ          = "nova"

	// DefaultGlanceImageBytes is the thin-provisioned virtual RBD size used when
	// a Glance image is uploaded without min_disk or with a tiny payload.
	DefaultGlanceImageBytes = int64(128) << 20 // 128 MiB

	// DefaultCephImagesPool is the Ceph RBD pool storing Glance base images.
	DefaultCephImagesPool = "images"
	// DefaultCephVolumesPool is the Ceph RBD pool storing Cinder volumes & CoW clones.
	DefaultCephVolumesPool = "volumes"

	// cinderTimeLayout matches gophercloud.JSONRFC3339MilliNoZ.
	cinderTimeLayout = "2006-01-02T15:04:05.000000"
)

// TokenRecord represents an issued Keystone v3 authentication token.
type TokenRecord struct {
	Token       string
	UserID      string
	Username    string
	ProjectID   string
	ProjectName string
	IssuedAt    time.Time
	ExpiresAt   time.Time
}

// ProjectRecord represents a Keystone v3 project (tenant).
type ProjectRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DomainID    string `json:"domain_id"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// ImageRecord stores Glance v2 metadata and its backing Ceph RBD snapshot lineage.
type ImageRecord struct {
	ID              string
	Name            string
	Status          string // "queued", "active"
	Visibility      string
	ContainerFormat string
	DiskFormat      string
	MinDisk         int
	MinRAM          int
	Protected       bool
	Hidden          bool
	Owner           string
	Tags            []string
	Checksum        string
	SizeBytes       int64
	VirtualSize     int64
	Properties      map[string]string
	CreatedAt       time.Time
	UpdatedAt       time.Time

	// Backing Ceph RBD identifiers (e.g. images/glance-<id>@base).
	RBDPool  string
	RBDImage string
	RBDSnap  string
}

// VolumeAttachmentRecord represents a Nova server attachment recorded on a Cinder volume.
type VolumeAttachmentRecord struct {
	ID           string
	AttachmentID string
	VolumeID     string
	ServerID     string
	HostName     string
	Device       string
	AttachedAt   time.Time
}

// VolumeRecord stores Cinder v3 metadata and its backing Ceph RBD volume/clone.
type VolumeRecord struct {
	ID                  string
	Name                string
	Description         string
	Status              string // "available", "in-use"
	SizeGiB             int
	AvailabilityZone    string
	VolumeType          string
	Bootable            bool
	Encrypted           bool
	Multiattach         bool
	ProjectID           string
	UserID              string
	ImageID             string
	SnapshotID          string
	SourceVolID         string
	Host                string
	Metadata            map[string]string
	VolumeImageMetadata map[string]string
	Attachments         []VolumeAttachmentRecord
	CreatedAt           time.Time
	UpdatedAt           time.Time

	// Backing Ceph RBD location (e.g. volumes/volume-<id>).
	RBDPool   string
	RBDImage  string
	RBDParent string // "images/glance-<id>@base" when CoW-cloned
}

// NetworkRecord stores a Neutron v2 virtual network (backed by OVN Geneve).
type NetworkRecord struct {
	ID           string
	Name         string
	Description  string
	ProjectID    string
	AdminStateUp bool
	Shared       bool
	External     bool
	Status       string
	NetworkType  string
	MTU          int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AllocationPool defines a start..end IP range inside a Neutron subnet.
type AllocationPool struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// SubnetRecord stores a Neutron v2 subnet with CIDR & gateway metadata.
type SubnetRecord struct {
	ID              string
	Name            string
	Description     string
	NetworkID       string
	ProjectID       string
	IPVersion       int
	CIDR            string
	GatewayIP       string
	EnableDHCP      bool
	AllocationPools []AllocationPool
	DNSNameservers  []string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ExternalGatewayInfo stores a router's optional external gateway network.
type ExternalGatewayInfo struct {
	NetworkID  string `json:"network_id"`
	EnableSNAT *bool  `json:"enable_snat,omitzero"`
}

// RouterRecord stores a Neutron L3 virtual router and its attached subnet ports.
type RouterRecord struct {
	ID                  string
	Name                string
	Description         string
	ProjectID           string
	AdminStateUp        bool
	Status              string
	Distributed         bool
	ExternalGatewayInfo *ExternalGatewayInfo
	// SubnetPorts maps subnetID -> router interface portID.
	SubnetPorts map[string]string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SecurityGroupRecord stores a Neutron security group.
type SecurityGroupRecord struct {
	ID          string
	Name        string
	Description string
	ProjectID   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// SecurityGroupRuleRecord stores a single ingress/egress firewall rule.
type SecurityGroupRuleRecord struct {
	ID             string
	SecGroupID     string
	ProjectID      string
	Direction      string // "ingress" or "egress"
	EtherType      string // "IPv4" or "IPv6"
	Protocol       string // "tcp", "udp", "icmp", ""
	PortRangeMin   int
	PortRangeMax   int
	RemoteIPPrefix string
	RemoteGroupID  string
	Description    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// FixedIP binds a Neutron port to a specific subnet and IP address.
type FixedIP struct {
	SubnetID  string `json:"subnet_id"`
	IPAddress string `json:"ip_address"`
}

// PortRecord stores a Neutron port including OVN chassis binding attributes.
type PortRecord struct {
	ID             string
	Name           string
	Description    string
	NetworkID      string
	ProjectID      string
	MACAddress     string
	AdminStateUp   bool
	Status         string
	DeviceID       string
	DeviceOwner    string
	FixedIPs       []FixedIP
	SecurityGroups []string
	// OVN / ML2 port-binding attributes (binding:*).
	HostID        string
	VIFType       string
	VNICType      string
	VIFDetails    map[string]any
	ManagedByNova bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FlavorRecord stores a Nova compute flavor.
type FlavorRecord struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	VCPUs int    `json:"vcpus"`
	RAM   int    `json:"ram"`
	Disk  int    `json:"disk"`
}

// ServerVolumeRef tracks a Cinder volume attached to a Nova server.
type ServerVolumeRef struct {
	VolumeID            string
	Device              string
	DeleteOnTermination bool
}

// ServerRecord stores a Nova v2.1 VM instance scheduled on a compute host.
type ServerRecord struct {
	ID               string
	Name             string
	ProjectID        string
	UserID           string
	FlavorID         string
	ImageID          string
	Status           string // "ACTIVE", "SHUTOFF"
	Host             string // OS-EXT-SRV-ATTR:host (e.g. "compute-01")
	InstanceName     string // OS-EXT-SRV-ATTR:instance_name
	AvailabilityZone string
	PortIDs          []string
	Volumes          []ServerVolumeRef
	SecurityGroups   []string
	Metadata         map[string]string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// store holds all OpenStack control-plane metadata protected by a single RWMutex.
type store struct {
	mu sync.RWMutex

	projects       map[string]*ProjectRecord
	tokens         map[string]*TokenRecord
	images         map[string]*ImageRecord
	volumes        map[string]*VolumeRecord
	networks       map[string]*NetworkRecord
	subnets        map[string]*SubnetRecord
	routers        map[string]*RouterRecord
	securityGroups map[string]*SecurityGroupRecord
	secGroupRules  map[string]*SecurityGroupRuleRecord
	ports          map[string]*PortRecord
	flavors        map[string]*FlavorRecord
	servers        map[string]*ServerRecord

	computeHosts []string
	hostCursor   int
}

func newStore(computeHosts []string) *store {
	if len(computeHosts) == 0 {
		computeHosts = []string{"compute-01", "compute-02", "compute-03"}
	}
	now := time.Now().UTC()
	defaultSGID := newUUID()

	s := &store{
		projects: map[string]*ProjectRecord{
			DefaultProjectID: {
				ID:          DefaultProjectID,
				Name:        DefaultProjectName,
				DomainID:    DefaultDomainID,
				Description: "Default administrative project",
				Enabled:     true,
			},
		},
		tokens:         make(map[string]*TokenRecord),
		images:         make(map[string]*ImageRecord),
		volumes:        make(map[string]*VolumeRecord),
		networks:       make(map[string]*NetworkRecord),
		subnets:        make(map[string]*SubnetRecord),
		routers:        make(map[string]*RouterRecord),
		securityGroups: make(map[string]*SecurityGroupRecord),
		secGroupRules:  make(map[string]*SecurityGroupRuleRecord),
		ports:          make(map[string]*PortRecord),
		flavors: map[string]*FlavorRecord{
			"1": {ID: "1", Name: "m1.tiny", VCPUs: 1, RAM: 512, Disk: 1},
			"2": {ID: "2", Name: "m1.small", VCPUs: 1, RAM: 2048, Disk: 20},
			"3": {ID: "3", Name: "m1.medium", VCPUs: 2, RAM: 4096, Disk: 40},
		},
		servers:      make(map[string]*ServerRecord),
		computeHosts: slices.Clone(computeHosts),
	}

	// Seed default security group (matches standard Neutron tenant bootstrap).
	s.securityGroups[defaultSGID] = &SecurityGroupRecord{
		ID:          defaultSGID,
		Name:        "default",
		Description: "Default security group",
		ProjectID:   DefaultProjectID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	egressID := newUUID()
	s.secGroupRules[egressID] = &SecurityGroupRuleRecord{
		ID:         egressID,
		SecGroupID: defaultSGID,
		ProjectID:  DefaultProjectID,
		Direction:  "egress",
		EtherType:  "IPv4",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	return s
}

func (s *store) nextComputeHostLocked(requested string) (string, error) {
	if requested != "" {
		if !slices.Contains(s.computeHosts, requested) {
			return "", fmt.Errorf("compute host %q not found", requested)
		}
		return requested, nil
	}
	host := s.computeHosts[s.hostCursor%len(s.computeHosts)]
	s.hostCursor++
	return host, nil
}

func (s *store) allocateIPFromSubnetLocked(sub *SubnetRecord, requestedIP string) (string, error) {
	prefix, err := netip.ParsePrefix(sub.CIDR)
	if err != nil {
		return "", fmt.Errorf("invalid subnet CIDR %q: %w", sub.CIDR, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return "", fmt.Errorf("only IPv4 subnets are currently supported: %q", sub.CIDR)
	}

	used := make(map[netip.Addr]struct{})
	if sub.GatewayIP != "" {
		if gw, err := netip.ParseAddr(sub.GatewayIP); err == nil {
			used[gw] = struct{}{}
		}
	}
	for _, p := range s.ports {
		for _, fip := range p.FixedIPs {
			if fip.SubnetID == sub.ID {
				if addr, err := netip.ParseAddr(fip.IPAddress); err == nil {
					used[addr] = struct{}{}
				}
			}
		}
	}

	if requestedIP != "" {
		addr, err := netip.ParseAddr(requestedIP)
		if err != nil || !prefix.Contains(addr) {
			return "", fmt.Errorf("requested IP %q is outside subnet %s", requestedIP, sub.CIDR)
		}
		if _, taken := used[addr]; taken {
			return "", fmt.Errorf("IP %q is already allocated in subnet %s", requestedIP, sub.ID)
		}
		return addr.String(), nil
	}

	start, end, err := subnetHostRange(prefix, sub.AllocationPools)
	if err != nil {
		return "", err
	}
	for cur := start; cur.Compare(end) <= 0; cur = cur.Next() {
		if _, taken := used[cur]; !taken {
			return cur.String(), nil
		}
	}
	return "", fmt.Errorf("no available IP addresses in subnet %s (%s)", sub.ID, sub.CIDR)
}

func computeSubnetDefaults(cidr, gatewayIP string, pools []AllocationPool) (string, []AllocationPool, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return "", nil, fmt.Errorf("subnet CIDR %q must be an IPv4 prefix with mask /30 or larger", cidr)
	}

	netAddr := prefix.Addr()
	broadcast := ipv4Broadcast(prefix)
	firstHost := netAddr.Next()
	lastHost := broadcast.Prev()

	if gatewayIP == "" {
		gatewayIP = firstHost.String()
	} else {
		gw, err := netip.ParseAddr(gatewayIP)
		if err != nil || !prefix.Contains(gw) {
			return "", nil, fmt.Errorf("gateway_ip %q must belong to %s", gatewayIP, cidr)
		}
	}

	if len(pools) == 0 {
		poolStart := firstHost
		if gw, err := netip.ParseAddr(gatewayIP); err == nil && gw == firstHost && firstHost.Next().Compare(lastHost) <= 0 {
			poolStart = firstHost.Next()
		}
		pools = []AllocationPool{{Start: poolStart.String(), End: lastHost.String()}}
	}
	return gatewayIP, pools, nil
}

func subnetHostRange(prefix netip.Prefix, pools []AllocationPool) (netip.Addr, netip.Addr, error) {
	if len(pools) > 0 {
		start, err1 := netip.ParseAddr(pools[0].Start)
		end, err2 := netip.ParseAddr(pools[0].End)
		if err1 == nil && err2 == nil && prefix.Contains(start) && prefix.Contains(end) {
			return start, end, nil
		}
	}
	return prefix.Addr().Next(), ipv4Broadcast(prefix).Prev(), nil
}

func ipv4Broadcast(prefix netip.Prefix) netip.Addr {
	b := prefix.Addr().As4()
	hostBits := uint32(32 - prefix.Bits())
	ip := binary.BigEndian.Uint32(b[:])
	mask := (uint32(1) << hostBits) - 1
	bcast := ip | mask
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], bcast)
	return netip.AddrFrom4(out)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // RFC 4122 version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newMAC() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	// fa:16:3e is the standard OpenStack Neutron OUI prefix.
	return fmt.Sprintf("fa:16:3e:%02x:%02x:%02x", b[0], b[1], b[2])
}

func formatCinderTime(t time.Time) string {
	return t.UTC().Format(cinderTimeLayout)
}
