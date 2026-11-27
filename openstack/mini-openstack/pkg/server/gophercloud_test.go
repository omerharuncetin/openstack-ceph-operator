package server_test

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/volumeattach"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/imagedata"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/routers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/portsbinding"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/mini-openstack/pkg/server"
)

type portWithBinding struct {
	ports.Port
	portsbinding.PortsBindingExt
}

func TestGophercloudEndToEndWithMockCeph(t *testing.T) {
	mockCeph := cephapi.NewMockServer("admin", "admin12345!")
	defer mockCeph.Close()

	cephClient, err := cephapi.New(cephapi.Config{
		BaseURL:  mockCeph.URL(),
		Username: "admin",
		Password: "admin12345!",
	})
	if err != nil {
		t.Fatalf("cephapi.New: %v", err)
	}

	runFullOpenStackLifecycle(t, cephClient, "mock", "ubuntu-mock")
}

// TestGophercloudEndToEndWithLiveCephSquid runs the full OpenStack workflow
// (Keystone -> Glance RBD -> Cinder CoW clone & resize -> Neutron OVN ->
// Nova boot, volume attach & zero-copy live migration -> reverse teardown)
// against Ceph Squid (v19) when CEPH_API_URL is set.
func TestGophercloudEndToEndWithLiveCephSquid(t *testing.T) {
	apiURL := os.Getenv("CEPH_API_URL")
	if apiURL == "" {
		t.Skip("CEPH_API_URL not set; skipping live Ceph Squid OpenStack integration test")
	}

	cephClient, err := cephapi.New(cephapi.Config{
		BaseURL:            apiURL,
		Username:           cmp.Or(os.Getenv("CEPH_API_USER"), "admin"),
		Password:           cmp.Or(os.Getenv("CEPH_API_PASS"), "admin12345!"),
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("cephapi.New: %v", err)
	}

	tag := fmt.Sprintf("ubuntu-live-%d", time.Now().Unix())
	runFullOpenStackLifecycle(t, cephClient, "live", tag)
}

func runFullOpenStackLifecycle(t *testing.T, cephClient *cephapi.Client, cephMode, imageName string) {
	t.Helper()
	ctx := t.Context()

	srv, err := server.New(server.Config{
		CephClient: cephClient,
		CephMode:   cephMode,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 0. Verify /healthz reports healthy Ceph backend.
	healthResp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /healthz, got %d", healthResp.StatusCode)
	}
	var hz map[string]any
	if err := json.NewDecoder(healthResp.Body).Decode(&hz); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if hz["ceph_status"] != "HEALTH_OK" || hz["ceph_mode"] != cephMode {
		t.Fatalf("unexpected /healthz payload: %+v", hz)
	}

	// 1. Keystone v3 auth.
	provider, err := openstack.AuthenticatedClient(ctx, gophercloud.AuthOptions{
		IdentityEndpoint: ts.URL + "/v3",
		Username:         server.DefaultUsername,
		Password:         server.DefaultPassword,
		DomainName:       server.DefaultDomainName,
		TenantName:       server.DefaultProjectName,
		AllowReauth:      true,
	})
	if err != nil {
		t.Fatalf("openstack.AuthenticatedClient: %v", err)
	}

	eo := gophercloud.EndpointOpts{
		Region:       server.DefaultRegion,
		Availability: gophercloud.AvailabilityPublic,
	}

	imageClient, err := openstack.NewImageV2(provider, eo)
	if err != nil {
		t.Fatalf("NewImageV2: %v", err)
	}
	cinderClient, err := openstack.NewBlockStorageV3(provider, eo)
	if err != nil {
		t.Fatalf("NewBlockStorageV3: %v", err)
	}
	neutronClient, err := openstack.NewNetworkV2(provider, eo)
	if err != nil {
		t.Fatalf("NewNetworkV2: %v", err)
	}
	novaClient, err := openstack.NewComputeV2(provider, eo)
	if err != nil {
		t.Fatalf("NewComputeV2: %v", err)
	}

	// 2. Glance v2: create image metadata + upload payload -> Ceph RBD + protected @base snapshot.
	vis := images.ImageVisibilityPublic
	img, err := images.Create(ctx, imageClient, images.CreateOpts{
		Name:            imageName,
		Visibility:      &vis,
		ContainerFormat: "bare",
		DiskFormat:      "raw",
		Properties:      map[string]string{"os_distro": "ubuntu"},
	}).Extract()
	if err != nil {
		t.Fatalf("Glance images.Create: %v", err)
	}
	if img.Status != images.ImageStatusQueued {
		t.Fatalf("expected queued image status, got %q", img.Status)
	}

	payload := bytes.Repeat([]byte("cloud-image-raw-block-"), 128)
	if err := imagedata.Upload(ctx, imageClient, img.ID, bytes.NewReader(payload)).ExtractErr(); err != nil {
		t.Fatalf("Glance imagedata.Upload: %v", err)
	}

	activeImg, err := images.Get(ctx, imageClient, img.ID).Extract()
	if err != nil {
		t.Fatalf("Glance images.Get: %v", err)
	}
	if activeImg.Status != images.ImageStatusActive {
		t.Fatalf("expected active Glance image after upload, got %q", activeImg.Status)
	}
	if activeImg.Properties["rbd_pool"] != "images" || activeImg.Properties["rbd_snap"] != "base" {
		t.Fatalf("expected Ceph RBD properties on Glance image, got %+v", activeImg.Properties)
	}

	// Verify Ceph RBD base image + protected @base snapshot directly in Ceph.
	rbdImgName := "glance-" + img.ID
	cephBase, err := cephClient.GetRBDImage(ctx, "images", rbdImgName)
	if err != nil {
		t.Fatalf("cephClient.GetRBDImage(%s): %v", rbdImgName, err)
	}
	if len(cephBase.Snapshots) != 1 || cephBase.Snapshots[0].Name != "base" || !cephBase.Snapshots[0].IsProtected {
		t.Fatalf("expected protected @base snapshot in Ceph, got %+v", cephBase.Snapshots)
	}

	// 3. Cinder v3: clone bootable volume from Glance image.
	bootVol, err := volumes.Create(ctx, cinderClient, volumes.CreateOpts{
		Name:     "boot-vol-01",
		Size:     1, // 1 GiB thin-provisioned CoW clone
		ImageID:  img.ID,
		Metadata: map[string]string{"tier": "gold"},
	}, nil).Extract()
	if err != nil {
		t.Fatalf("Cinder volumes.Create (CoW): %v", err)
	}
	if bootVol.Status != "available" || bootVol.Bootable != "true" {
		t.Fatalf("unexpected boot volume state: status=%q bootable=%q", bootVol.Status, bootVol.Bootable)
	}

	// Check CoW parent in Ceph ("images/glance-<id>@base").
	rbdVolName := "volume-" + bootVol.ID
	cephClone, err := cephClient.GetRBDImage(ctx, "volumes", rbdVolName)
	if err != nil {
		t.Fatalf("cephClient.GetRBDImage(volumes/%s): %v", rbdVolName, err)
	}
	if cephClone.Parent == nil || cephClone.Parent.PoolName != "images" || cephClone.Parent.ImageName != rbdImgName || cephClone.Parent.SnapName != "base" {
		t.Fatalf("expected Ceph CoW parent images/%s@base, got %+v", rbdImgName, cephClone.Parent)
	}

	// Verify Ceph CoW invariant: deleting Glance base image fails while Cinder clone exists.
	if err := images.Delete(ctx, imageClient, img.ID).ExtractErr(); err == nil {
		t.Fatal("expected Glance image delete to fail while active Cinder CoW clone exists")
	}

	// Online Cinder volume resize (1 GiB -> 2 GiB) -> Ceph RBD resize.
	if err := volumes.ExtendSize(ctx, cinderClient, bootVol.ID, volumes.ExtendSizeOpts{NewSize: 2}).ExtractErr(); err != nil {
		t.Fatalf("Cinder volumes.ExtendSize: %v", err)
	}
	cephCloneAfterResize, err := cephClient.GetRBDImage(ctx, "volumes", rbdVolName)
	if err != nil {
		t.Fatalf("cephClient.GetRBDImage after resize: %v", err)
	}
	if cephCloneAfterResize.Size != int64(2)<<30 {
		t.Fatalf("expected resized Ceph RBD image 2 GiB, got %d bytes", cephCloneAfterResize.Size)
	}

	// Also create a blank data volume to test dynamic Nova volume attachment.
	dataVol, err := volumes.Create(ctx, cinderClient, volumes.CreateOpts{
		Name: "data-vol-01",
		Size: 1,
	}, nil).Extract()
	if err != nil {
		t.Fatalf("Cinder blank volumes.Create: %v", err)
	}

	// 4. Neutron v2: Network -> Subnet -> Router (+interface) -> Security Group + Rules -> Port.
	net, err := networks.Create(ctx, neutronClient, networks.CreateOpts{
		Name:         "tenant-app-net",
		AdminStateUp: new(true),
	}).Extract()
	if err != nil {
		t.Fatalf("Neutron networks.Create: %v", err)
	}

	sub, err := subnets.Create(ctx, neutronClient, subnets.CreateOpts{
		Name:       "tenant-app-subnet",
		NetworkID:  net.ID,
		IPVersion:  gophercloud.IPv4,
		CIDR:       "10.42.0.0/24",
		EnableDHCP: new(true),
	}).Extract()
	if err != nil {
		t.Fatalf("Neutron subnets.Create: %v", err)
	}
	if sub.GatewayIP != "10.42.0.1" {
		t.Fatalf("expected default gateway 10.42.0.1, got %q", sub.GatewayIP)
	}

	rtr, err := routers.Create(ctx, neutronClient, routers.CreateOpts{
		Name:         "tenant-router",
		AdminStateUp: new(true),
	}).Extract()
	if err != nil {
		t.Fatalf("Neutron routers.Create: %v", err)
	}
	if _, err := routers.AddInterface(ctx, neutronClient, rtr.ID, routers.AddInterfaceOpts{SubnetID: sub.ID}).Extract(); err != nil {
		t.Fatalf("Neutron routers.AddInterface: %v", err)
	}

	sg, err := groups.Create(ctx, neutronClient, groups.CreateOpts{
		Name:        "app-sg",
		Description: "Allow SSH and HTTPS",
	}).Extract()
	if err != nil {
		t.Fatalf("Neutron secgroup Create: %v", err)
	}
	sshRule, err := rules.Create(ctx, neutronClient, rules.CreateOpts{
		Direction:      rules.DirIngress,
		EtherType:      rules.EtherType4,
		SecGroupID:     sg.ID,
		Protocol:       rules.ProtocolTCP,
		PortRangeMin:   22,
		PortRangeMax:   22,
		RemoteIPPrefix: "10.0.0.0/8",
	}).Extract()
	if err != nil {
		t.Fatalf("Neutron secgroup rule Create: %v", err)
	}

	// Pre-create a Neutron port with the security group so we can inspect OVN binding:host_id.
	vmPort, err := ports.Create(ctx, neutronClient, ports.CreateOpts{
		Name:           "vm-nic-0",
		NetworkID:      net.ID,
		AdminStateUp:   new(true),
		SecurityGroups: &[]string{sg.ID},
	}).Extract()
	if err != nil {
		t.Fatalf("Neutron ports.Create: %v", err)
	}
	if len(vmPort.FixedIPs) != 1 || !strings.HasPrefix(vmPort.FixedIPs[0].IPAddress, "10.42.0.") {
		t.Fatalf("expected allocated IP in 10.42.0.0/24, got %+v", vmPort.FixedIPs)
	}

	// 5. Nova v2.1: list flavors, boot VM from Cinder CoW volume on our Neutron port, attach data volume.
	flvPages, err := flavors.ListDetail(novaClient, nil).AllPages(ctx)
	if err != nil {
		t.Fatalf("Nova flavors.ListDetail: %v", err)
	}
	allFlavors, err := flavors.ExtractFlavors(flvPages)
	if err != nil || len(allFlavors) < 3 {
		t.Fatalf("expected seeded flavors, got %d err=%v", len(allFlavors), err)
	}

	vm, err := servers.Create(ctx, novaClient, servers.CreateOpts{
		Name:      "web-node-01",
		FlavorRef: allFlavors[1].ID,
		Networks:  []servers.Network{{Port: vmPort.ID}},
		BlockDevice: []servers.BlockDevice{
			{
				UUID:            bootVol.ID,
				SourceType:      servers.SourceVolume,
				DestinationType: servers.DestinationVolume,
				BootIndex:       0,
			},
		},
	}, nil).Extract()
	if err != nil {
		t.Fatalf("Nova servers.Create: %v", err)
	}
	if vm.Status != "ACTIVE" || vm.Host != "compute-01" {
		t.Fatalf("expected ACTIVE server on compute-01, got status=%q host=%q", vm.Status, vm.Host)
	}

	// Verify Neutron port is bound to compute-01 in OVN.
	var boundPort portWithBinding
	if err := ports.Get(ctx, neutronClient, vmPort.ID).ExtractInto(&boundPort); err != nil {
		t.Fatalf("Neutron ports.Get with binding: %v", err)
	}
	if boundPort.HostID != "compute-01" || boundPort.VIFType != "ovs" {
		t.Fatalf("expected OVN port bound to compute-01/ovs, got host=%q vif=%q", boundPort.HostID, boundPort.VIFType)
	}

	// Dynamically attach second Cinder data volume to Nova VM.
	att, err := volumeattach.Create(ctx, novaClient, vm.ID, volumeattach.CreateOpts{
		VolumeID: dataVol.ID,
		Device:   "/dev/vdb",
	}).Extract()
	if err != nil {
		t.Fatalf("Nova volumeattach.Create: %v", err)
	}
	if att.VolumeID != dataVol.ID || att.Device != "/dev/vdb" {
		t.Fatalf("unexpected volume attachment: %+v", att)
	}

	// Verify Cinder blocks deleting an in-use volume.
	if err := volumes.Delete(ctx, cinderClient, bootVol.ID, nil).ExtractErr(); err == nil {
		t.Fatal("expected deleting in-use Cinder volume to fail")
	}

	// 6. Live migration over shared Ceph RBD (compute-01 -> compute-02).
	// block_migration=true should fail on shared Ceph RBD.
	if err := servers.LiveMigrate(ctx, novaClient, vm.ID, servers.LiveMigrateOpts{
		Host:           new("compute-02"),
		BlockMigration: new(true),
	}).ExtractErr(); err == nil {
		t.Fatal("expected block_migration=true to fail on shared Ceph RBD volumes")
	}

	// Live migrate with block_migration=false.
	if err := servers.LiveMigrate(ctx, novaClient, vm.ID, servers.LiveMigrateOpts{
		Host:           new("compute-02"),
		BlockMigration: new(false),
	}).ExtractErr(); err != nil {
		t.Fatalf("Nova servers.LiveMigrate: %v", err)
	}

	migratedVM, err := servers.Get(ctx, novaClient, vm.ID).Extract()
	if err != nil {
		t.Fatalf("Nova servers.Get after migrate: %v", err)
	}
	if migratedVM.Host != "compute-02" {
		t.Fatalf("expected migrated VM on compute-02, got %q", migratedVM.Host)
	}

	// Check Neutron OVN port chassis moved to compute-02.
	var migratedPort portWithBinding
	if err := ports.Get(ctx, neutronClient, vmPort.ID).ExtractInto(&migratedPort); err != nil {
		t.Fatalf("Neutron ports.Get after live migrate: %v", err)
	}
	if migratedPort.HostID != "compute-02" {
		t.Fatalf("expected OVN port binding:host_id=compute-02 after live migration, got %q", migratedPort.HostID)
	}

	// 7. Reverse teardown: Nova -> Cinder -> Glance -> Neutron.
	if err := volumeattach.Delete(ctx, novaClient, vm.ID, dataVol.ID).ExtractErr(); err != nil {
		t.Fatalf("Nova volumeattach.Delete: %v", err)
	}
	if err := servers.Delete(ctx, novaClient, vm.ID).ExtractErr(); err != nil {
		t.Fatalf("Nova servers.Delete: %v", err)
	}
	if err := volumes.Delete(ctx, cinderClient, bootVol.ID, nil).ExtractErr(); err != nil {
		t.Fatalf("Cinder volumes.Delete (boot): %v", err)
	}
	if err := volumes.Delete(ctx, cinderClient, dataVol.ID, nil).ExtractErr(); err != nil {
		t.Fatalf("Cinder volumes.Delete (data): %v", err)
	}
	// Glance delete succeeds once child clones are gone.
	if err := images.Delete(ctx, imageClient, img.ID).ExtractErr(); err != nil {
		t.Fatalf("Glance images.Delete after clone cleanup: %v", err)
	}
	if _, err := cephClient.GetRBDImage(ctx, "images", rbdImgName); !cephapi.IsNotFound(err) {
		t.Fatalf("expected Glance RBD image to be deleted in Ceph, got err=%v", err)
	}

	// Neutron teardown: Port -> Security Group/Rule -> Router Interface -> Router -> Subnet -> Network.
	if err := ports.Delete(ctx, neutronClient, vmPort.ID).ExtractErr(); err != nil {
		t.Fatalf("Neutron ports.Delete: %v", err)
	}
	if err := rules.Delete(ctx, neutronClient, sshRule.ID).ExtractErr(); err != nil {
		t.Fatalf("Neutron rules.Delete: %v", err)
	}
	if err := groups.Delete(ctx, neutronClient, sg.ID).ExtractErr(); err != nil {
		t.Fatalf("Neutron groups.Delete: %v", err)
	}
	if _, err := routers.RemoveInterface(ctx, neutronClient, rtr.ID, routers.RemoveInterfaceOpts{SubnetID: sub.ID}).Extract(); err != nil {
		t.Fatalf("Neutron routers.RemoveInterface: %v", err)
	}
	if err := routers.Delete(ctx, neutronClient, rtr.ID).ExtractErr(); err != nil {
		t.Fatalf("Neutron routers.Delete: %v", err)
	}
	if err := subnets.Delete(ctx, neutronClient, sub.ID).ExtractErr(); err != nil {
		t.Fatalf("Neutron subnets.Delete: %v", err)
	}
	if err := networks.Delete(ctx, neutronClient, net.ID).ExtractErr(); err != nil {
		t.Fatalf("Neutron networks.Delete: %v", err)
	}

	t.Logf("PASS: Full OpenStack + Ceph (%s) lifecycle verified with official gophercloud/v2", cephMode)
}

func TestSecurityInvariantsAndBootFromImageVolume(t *testing.T) {
	ctx := t.Context()
	mockCeph := cephapi.NewMockServer("admin", "admin12345!")
	defer mockCeph.Close()

	cephClient, err := cephapi.New(cephapi.Config{
		BaseURL:  mockCeph.URL(),
		Username: "admin",
		Password: "admin12345!",
	})
	if err != nil {
		t.Fatalf("cephapi.New: %v", err)
	}

	srv, err := server.New(server.Config{
		CephClient: cephClient,
		CephMode:   "mock",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 1. Invalid credentials must be rejected with 401.
	if _, err := openstack.AuthenticatedClient(ctx, gophercloud.AuthOptions{
		IdentityEndpoint: ts.URL + "/v3",
		Username:         "admin",
		Password:         "wrong-password",
		DomainName:       server.DefaultDomainName,
		TenantName:       server.DefaultProjectName,
	}); err == nil {
		t.Fatal("expected Keystone auth failure for invalid password")
	}

	// 2. Unauthenticated API call must return 401.
	resp, err := http.Get(ts.URL + "/v2/images")
	if err != nil {
		t.Fatalf("GET /v2/images: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without X-Auth-Token, got %d", resp.StatusCode)
	}

	// 3. Authenticate and verify Neutron referential integrity + Nova image->volume CoW auto-clone & DeleteOnTermination.
	provider, err := openstack.AuthenticatedClient(ctx, gophercloud.AuthOptions{
		IdentityEndpoint: ts.URL + "/v3",
		Username:         server.DefaultUsername,
		Password:         server.DefaultPassword,
		DomainName:       server.DefaultDomainName,
		TenantName:       server.DefaultProjectName,
	})
	if err != nil {
		t.Fatalf("AuthenticatedClient: %v", err)
	}
	eo := gophercloud.EndpointOpts{Region: server.DefaultRegion, Availability: gophercloud.AvailabilityPublic}
	imageClient, _ := openstack.NewImageV2(provider, eo)
	neutronClient, _ := openstack.NewNetworkV2(provider, eo)
	novaClient, _ := openstack.NewComputeV2(provider, eo)

	net, err := networks.Create(ctx, neutronClient, networks.CreateOpts{Name: "inv-net", AdminStateUp: new(true)}).Extract()
	if err != nil {
		t.Fatalf("networks.Create: %v", err)
	}
	sub, err := subnets.Create(ctx, neutronClient, subnets.CreateOpts{Name: "inv-sub", NetworkID: net.ID, IPVersion: gophercloud.IPv4, CIDR: "10.99.0.0/24"}).Extract()
	if err != nil {
		t.Fatalf("subnets.Create: %v", err)
	}
	rtr, err := routers.Create(ctx, neutronClient, routers.CreateOpts{Name: "inv-rtr", AdminStateUp: new(true)}).Extract()
	if err != nil {
		t.Fatalf("routers.Create: %v", err)
	}
	if _, err := routers.AddInterface(ctx, neutronClient, rtr.ID, routers.AddInterfaceOpts{SubnetID: sub.ID}).Extract(); err != nil {
		t.Fatalf("routers.AddInterface: %v", err)
	}

	// Cannot delete subnet or router while interface port is attached.
	if err := subnets.Delete(ctx, neutronClient, sub.ID).ExtractErr(); err == nil {
		t.Fatal("expected subnet delete to fail while router interface is attached")
	}
	if err := routers.Delete(ctx, neutronClient, rtr.ID).ExtractErr(); err == nil {
		t.Fatal("expected router delete to fail while interface is attached")
	}

	// Upload Glance image and boot Nova VM using SourceImage -> DestinationVolume with DeleteOnTermination=true.
	img, err := images.Create(ctx, imageClient, images.CreateOpts{Name: "alpine-base", ContainerFormat: "bare", DiskFormat: "raw"}).Extract()
	if err != nil {
		t.Fatalf("images.Create: %v", err)
	}
	if err := imagedata.Upload(ctx, imageClient, img.ID, bytes.NewReader([]byte("alpine-root"))).ExtractErr(); err != nil {
		t.Fatalf("imagedata.Upload: %v", err)
	}

	vm, err := servers.Create(ctx, novaClient, servers.CreateOpts{
		Name:      "ephemeral-vm",
		FlavorRef: "m1.small",
		Networks:  []servers.Network{{UUID: net.ID}},
		BlockDevice: []servers.BlockDevice{
			{
				UUID:                img.ID,
				SourceType:          servers.SourceImage,
				DestinationType:     servers.DestinationVolume,
				BootIndex:           0,
				VolumeSize:          2,
				DeleteOnTermination: true,
			},
		},
	}, nil).Extract()
	if err != nil {
		t.Fatalf("servers.Create (SourceImage -> DestinationVolume): %v", err)
	}
	if len(vm.AttachedVolumes) != 1 {
		t.Fatalf("expected 1 auto-cloned boot volume attached, got %+v", vm.AttachedVolumes)
	}
	autoVolID := vm.AttachedVolumes[0].ID

	// Test Stop -> Start power actions.
	if err := servers.Stop(ctx, novaClient, vm.ID).ExtractErr(); err != nil {
		t.Fatalf("servers.Stop: %v", err)
	}
	stopped, _ := servers.Get(ctx, novaClient, vm.ID).Extract()
	if stopped.Status != "SHUTOFF" {
		t.Fatalf("expected SHUTOFF after Stop, got %q", stopped.Status)
	}
	if err := servers.Start(ctx, novaClient, vm.ID).ExtractErr(); err != nil {
		t.Fatalf("servers.Start: %v", err)
	}
	started, _ := servers.Get(ctx, novaClient, vm.ID).Extract()
	if started.Status != "ACTIVE" {
		t.Fatalf("expected ACTIVE after Start, got %q", started.Status)
	}

	// Deleting VM with DeleteOnTermination=true must delete the Cinder RBD clone so Glance image can be deleted cleanly.
	if err := servers.Delete(ctx, novaClient, vm.ID).ExtractErr(); err != nil {
		t.Fatalf("servers.Delete: %v", err)
	}
	if _, err := cephClient.GetRBDImage(ctx, "volumes", "volume-"+autoVolID); !cephapi.IsNotFound(err) {
		t.Fatalf("expected DeleteOnTermination volume to be removed from Ceph, got err=%v", err)
	}
	if err := images.Delete(ctx, imageClient, img.ID).ExtractErr(); err != nil {
		t.Fatalf("images.Delete after DeleteOnTermination: %v", err)
	}
}
