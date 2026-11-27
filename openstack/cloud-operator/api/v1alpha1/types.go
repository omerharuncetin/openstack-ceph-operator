package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ForceDeleteAnnotation skips external cleanup when the backing API is unreachable.
const ForceDeleteAnnotation = "iaas.cloud.io/force-delete"

// TenantStoragePoolSpec defines a Ceph RADOS pool.
type TenantStoragePoolSpec struct {
	// PoolName defaults to metadata.name if empty.
	PoolName string `json:"poolName,omitempty"`
	// PoolType is "replicated" or "erasure" (default: "replicated").
	PoolType string `json:"poolType,omitempty"`
	// PGNum is the placement group count (default: 16).
	PGNum int `json:"pgNum,omitzero"`
	// Size is the replica count for replicated pools.
	Size int `json:"size,omitzero"`
	// MinSize is the minimum active OSDs required for I/O.
	MinSize int `json:"minSize,omitzero"`
	// CrushRule is the CRUSH rule name.
	CrushRule string `json:"crushRule,omitempty"`
	// Application is the Ceph pool application tag (default: "rbd").
	Application string `json:"application,omitempty"`
	// QuotaMaxBytes sets a raw byte quota on the pool.
	QuotaMaxBytes int64 `json:"quotaMaxBytes,omitzero"`
}

// TenantStoragePoolStatus reports Ceph state for the pool.
type TenantStoragePoolStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitzero"`
	Phase              string             `json:"phase,omitempty"`
	PoolID             int                `json:"poolID,omitzero"`
	PoolName           string             `json:"poolName,omitempty"`
	CephHealth         string             `json:"cephHealth,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// TenantStoragePool manages a Ceph RADOS pool.
type TenantStoragePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantStoragePoolSpec   `json:"spec,omitempty"`
	Status TenantStoragePoolStatus `json:"status,omitempty"`
}

// TenantStoragePoolList holds a list of TenantStoragePool objects.
type TenantStoragePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantStoragePool `json:"items"`
}

// SecurityRuleSpec defines a Neutron security group rule.
type SecurityRuleSpec struct {
	// Direction is "ingress" or "egress" (default: "ingress").
	Direction string `json:"direction,omitempty"`
	// Protocol is "tcp", "udp", "icmp", or "" (any).
	Protocol string `json:"protocol,omitempty"`
	PortMin  int    `json:"portMin,omitzero"`
	PortMax  int    `json:"portMax,omitzero"`
	// RemoteCIDR is the CIDR prefix (for example "10.0.0.0/8").
	RemoteCIDR  string `json:"remoteCIDR,omitempty"`
	Description string `json:"description,omitempty"`
}

// VirtualNetworkSpec defines a Neutron network, subnet, router, and security rules.
type VirtualNetworkSpec struct {
	CIDR         string `json:"cidr"`
	GatewayIP    string `json:"gatewayIP,omitempty"`
	EnableDHCP   *bool  `json:"enableDHCP,omitempty"`
	EnableRouter bool   `json:"enableRouter,omitzero"`
	// SecurityRules are reconciled on each requeue to revert out-of-band changes.
	SecurityRules []SecurityRuleSpec `json:"securityRules,omitempty"`
}

// VirtualNetworkStatus reports Neutron resource IDs and drift counts.
type VirtualNetworkStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitzero"`
	Phase              string             `json:"phase,omitempty"`
	NetworkID          string             `json:"networkID,omitempty"`
	SubnetID           string             `json:"subnetID,omitempty"`
	RouterID           string             `json:"routerID,omitempty"`
	SecurityGroupID    string             `json:"securityGroupID,omitempty"`
	ActiveRuleCount    int                `json:"activeRuleCount,omitzero"`
	DriftHealsTotal    int64              `json:"driftHealsTotal,omitzero"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// VirtualNetwork manages a Neutron network, subnet, router, and security group.
type VirtualNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VirtualNetworkSpec   `json:"spec,omitempty"`
	Status VirtualNetworkStatus `json:"status,omitempty"`
}

// VirtualNetworkList holds a list of VirtualNetwork objects.
type VirtualNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualNetwork `json:"items"`
}

// CloudWorkspaceSpec defines a VM backed by a Cinder RBD boot volume and Neutron network.
type CloudWorkspaceSpec struct {
	// ImageName is the Glance image name (uploaded if missing).
	ImageName string `json:"imageName"`
	// FlavorRef is the Nova flavor ID or name (default: "m1.small").
	FlavorRef string `json:"flavorRef,omitempty"`
	// StorageGiB is the Cinder boot volume size in GiB.
	StorageGiB int `json:"storageGiB"`
	// VirtualNetworkRef is a VirtualNetwork name in the same namespace.
	VirtualNetworkRef string `json:"virtualNetworkRef"`
	// DesiredHost schedules or live-migrates the VM to this compute host.
	DesiredHost string `json:"desiredHost,omitempty"`
}

// CloudWorkspaceStatus reports Glance, Cinder, Neutron, and Nova state.
type CloudWorkspaceStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitzero"`
	Phase              string             `json:"phase,omitempty"`
	ImageID            string             `json:"imageID,omitempty"`
	VolumeID           string             `json:"volumeID,omitempty"`
	VolumeSizeGiB      int                `json:"volumeSizeGiB,omitzero"`
	CephRBDParent      string             `json:"cephRBDParent,omitempty"`
	PortID             string             `json:"portID,omitempty"`
	IPAddress          string             `json:"ipAddress,omitempty"`
	ServerID           string             `json:"serverID,omitempty"`
	ComputeHost        string             `json:"computeHost,omitempty"`
	OVNPortHost        string             `json:"ovnPortHost,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// CloudWorkspace manages a Nova VM booted from a Cinder RBD clone on a VirtualNetwork.
type CloudWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudWorkspaceSpec   `json:"spec,omitempty"`
	Status CloudWorkspaceStatus `json:"status,omitempty"`
}

// CloudWorkspaceList holds a list of CloudWorkspace objects.
type CloudWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudWorkspace `json:"items"`
}
