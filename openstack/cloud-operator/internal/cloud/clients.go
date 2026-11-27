// Package cloud builds authenticated OpenStack (gophercloud/v2) and
// Ceph Dashboard REST API (pkg/cephapi) clients for the operator.
package cloud

import (
	"cmp"
	"context"
	"fmt"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

// Config configures both Ceph Dashboard REST and OpenStack Keystone v3 clients.
type Config struct {
	// Ceph MGR Dashboard REST API settings.
	CephAPIURL             string
	CephUsername           string
	CephPassword           string
	CephInsecureSkipVerify bool

	// OpenStack Keystone v3 settings.
	OSAuthURL    string
	OSUsername   string
	OSPassword   string
	OSProject    string
	OSDomain     string
	OSRegionName string
}

// Clients bundles authenticated clients for all managed IaaS services.
type Clients struct {
	Ceph    *cephapi.Client
	Image   *gophercloud.ServiceClient
	Cinder  *gophercloud.ServiceClient
	Neutron *gophercloud.ServiceClient
	Nova    *gophercloud.ServiceClient
}

// NewClients creates the Ceph client and authenticates OpenStack clients via gophercloud/v2.
func NewClients(ctx context.Context, cfg Config) (*Clients, error) {
	cephClient, err := cephapi.New(cephapi.Config{
		BaseURL:            cfg.CephAPIURL,
		Username:           cmp.Or(cfg.CephUsername, "admin"),
		Password:           cmp.Or(cfg.CephPassword, "admin12345!"),
		InsecureSkipVerify: cfg.CephInsecureSkipVerify,
	})
	if err != nil {
		return nil, fmt.Errorf("init ceph client: %w", err)
	}

	provider, err := openstack.AuthenticatedClient(ctx, gophercloud.AuthOptions{
		IdentityEndpoint: cfg.OSAuthURL,
		Username:         cmp.Or(cfg.OSUsername, "admin"),
		Password:         cmp.Or(cfg.OSPassword, "admin12345!"),
		TenantName:       cmp.Or(cfg.OSProject, "admin"),
		DomainName:       cmp.Or(cfg.OSDomain, "Default"),
		AllowReauth:      true,
	})
	if err != nil {
		return nil, fmt.Errorf("authenticate openstack keystone (%s): %w", cfg.OSAuthURL, err)
	}

	eo := gophercloud.EndpointOpts{
		Region:       cmp.Or(cfg.OSRegionName, "RegionOne"),
		Availability: gophercloud.AvailabilityPublic,
	}

	imageClient, err := openstack.NewImageV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("init glance v2 client: %w", err)
	}
	cinderClient, err := openstack.NewBlockStorageV3(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("init cinder v3 client: %w", err)
	}
	neutronClient, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("init neutron v2 client: %w", err)
	}
	novaClient, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		return nil, fmt.Errorf("init nova v2.1 client: %w", err)
	}

	return &Clients{
		Ceph:    cephClient,
		Image:   imageClient,
		Cinder:  cinderClient,
		Neutron: neutronClient,
		Nova:    novaClient,
	}, nil
}
