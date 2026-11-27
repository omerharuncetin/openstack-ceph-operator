package controller_test

import (
	"cmp"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
	iaasv1alpha1 "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/api/v1alpha1"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/cloud"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/controller"
	opmetrics "github.com/omerharuncetin/openstack-ceph-operator/openstack/cloud-operator/internal/metrics"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/mini-openstack/pkg/server"
)

func TestOperatorFullStackWithMockCeph(t *testing.T) {
	mockCeph := cephapi.NewMockServer("admin", "admin12345!")
	defer mockCeph.Close()

	runOperatorSuite(t, cloud.Config{
		CephAPIURL:             mockCeph.URL(),
		CephUsername:           "admin",
		CephPassword:           "admin12345!",
		CephInsecureSkipVerify: true,
	}, "mock", "tenant-gold", 3, 2, "ubuntu-24.04-mock")
}

// TestOperatorFullStackWithLiveCephSquid runs all 3 controllers against Ceph Squid
// when CEPH_API_URL is set.
func TestOperatorFullStackWithLiveCephSquid(t *testing.T) {
	apiURL := os.Getenv("CEPH_API_URL")
	if apiURL == "" {
		t.Skip("CEPH_API_URL not set; skipping live Ceph Squid Kubernetes Operator test")
	}

	ts := time.Now().Unix()
	// Leave size/minSize unset so Ceph uses osd_pool_default_size.
	runOperatorSuite(t, cloud.Config{
		CephAPIURL:             apiURL,
		CephUsername:           cmp.Or(os.Getenv("CEPH_API_USER"), "admin"),
		CephPassword:           cmp.Or(os.Getenv("CEPH_API_PASS"), "admin12345!"),
		CephInsecureSkipVerify: true,
	}, "live", fmt.Sprintf("k8s-pool-%d", ts), 0, 0, fmt.Sprintf("ubuntu-k8s-%d", ts))
}

func runOperatorSuite(t *testing.T, baseCfg cloud.Config, cephMode, poolName string, poolSize, poolMinSize int, imageName string) {
	t.Helper()
	ctx := t.Context()
	opmetrics.Register()

	// Start mini-openstack backed by MockServer or Ceph Squid.
	cephClient, err := cephapi.New(cephapi.Config{
		BaseURL:            baseCfg.CephAPIURL,
		Username:           baseCfg.CephUsername,
		Password:           baseCfg.CephPassword,
		InsecureSkipVerify: baseCfg.CephInsecureSkipVerify,
	})
	if err != nil {
		t.Fatalf("cephapi.New: %v", err)
	}

	osSrv, err := server.New(server.Config{
		CephClient: cephClient,
		CephMode:   cephMode,
	})
	if err != nil {
		t.Fatalf("mini-openstack server.New: %v", err)
	}
	ts := httptest.NewServer(osSrv)
	defer ts.Close()

	baseCfg.OSAuthURL = ts.URL + "/v3"
	cloudClients, err := cloud.NewClients(ctx, baseCfg)
	if err != nil {
		t.Fatalf("cloud.NewClients: %v", err)
	}

	// Build fake Kubernetes API server with proper Status subresource isolation.
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = iaasv1alpha1.AddToScheme(sch)

	k8sClient := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(
			&iaasv1alpha1.TenantStoragePool{},
			&iaasv1alpha1.VirtualNetwork{},
			&iaasv1alpha1.CloudWorkspace{},
		).
		Build()
	recorder := events.NewFakeRecorder(64)

	poolRec := &controller.TenantStoragePoolReconciler{
		Client:   k8sClient,
		Scheme:   sch,
		Cloud:    cloudClients,
		Recorder: recorder,
	}
	vnRec := &controller.VirtualNetworkReconciler{
		Client:               k8sClient,
		Scheme:               sch,
		Cloud:                cloudClients,
		Recorder:             recorder,
		DriftRequeueInterval: 30 * time.Second,
	}
	wsRec := &controller.CloudWorkspaceReconciler{
		Client:   k8sClient,
		Scheme:   sch,
		Cloud:    cloudClients,
		Recorder: recorder,
	}

	// 1. TenantStoragePool: create, verify, delete.
	poolCR := &iaasv1alpha1.TenantStoragePool{
		ObjectMeta: metav1.ObjectMeta{Name: "gold-tier", Namespace: "default", Generation: 1},
		Spec: iaasv1alpha1.TenantStoragePoolSpec{
			PoolName:    poolName,
			PoolType:    "replicated",
			PGNum:       8,
			Size:        poolSize,
			MinSize:     poolMinSize,
			Application: "rbd",
		},
	}
	if err := k8sClient.Create(ctx, poolCR); err != nil {
		t.Fatalf("create TenantStoragePool CR: %v", err)
	}

	poolReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gold-tier", Namespace: "default"}}
	if _, err := poolRec.Reconcile(ctx, poolReq); err != nil {
		t.Fatalf("TenantStoragePool Reconcile: %v", err)
	}

	var gotPool iaasv1alpha1.TenantStoragePool
	if err := k8sClient.Get(ctx, poolReq.NamespacedName, &gotPool); err != nil {
		t.Fatalf("Get TenantStoragePool: %v", err)
	}
	if gotPool.Status.Phase != "Ready" || gotPool.Status.PoolName != poolName {
		t.Fatalf("unexpected TenantStoragePool status: %+v", gotPool.Status)
	}
	if !apimeta.IsStatusConditionTrue(gotPool.Status.Conditions, "Ready") {
		t.Fatalf("expected Ready=True condition, got %+v", gotPool.Status.Conditions)
	}

	// Delete CR and reconcile finalizer -> verifies Ceph pool is deleted cleanly.
	if err := k8sClient.Delete(ctx, &gotPool); err != nil {
		t.Fatalf("Delete TenantStoragePool CR: %v", err)
	}
	if _, err := poolRec.Reconcile(ctx, poolReq); err != nil {
		t.Fatalf("TenantStoragePool finalizer Reconcile: %v", err)
	}
	if _, err := cephClient.GetPool(ctx, poolName); !cephapi.IsNotFound(err) {
		t.Fatalf("expected Ceph pool %q to be deleted by finalizer, got err=%v", poolName, err)
	}

	// 2. VirtualNetwork: provision Neutron stack and heal out-of-band rule drift.
	vnCR := &iaasv1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-prod", Namespace: "default", Generation: 1},
		Spec: iaasv1alpha1.VirtualNetworkSpec{
			CIDR:         "10.77.0.0/24",
			EnableDHCP:   new(true),
			EnableRouter: true,
			SecurityRules: []iaasv1alpha1.SecurityRuleSpec{
				{Direction: "ingress", Protocol: "tcp", PortMin: 22, PortMax: 22, RemoteCIDR: "10.0.0.0/8", Description: "SSH"},
				{Direction: "ingress", Protocol: "tcp", PortMin: 443, PortMax: 443, RemoteCIDR: "0.0.0.0/0", Description: "HTTPS"},
			},
		},
	}
	if err := k8sClient.Create(ctx, vnCR); err != nil {
		t.Fatalf("create VirtualNetwork CR: %v", err)
	}
	vnReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "tenant-prod", Namespace: "default"}}
	res, err := vnRec.Reconcile(ctx, vnReq)
	if err != nil {
		t.Fatalf("VirtualNetwork Reconcile: %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("expected 30s drift requeue interval, got %v", res.RequeueAfter)
	}

	var gotVN iaasv1alpha1.VirtualNetwork
	if err := k8sClient.Get(ctx, vnReq.NamespacedName, &gotVN); err != nil {
		t.Fatalf("Get VirtualNetwork: %v", err)
	}
	if gotVN.Status.Phase != "Ready" || gotVN.Status.NetworkID == "" || gotVN.Status.RouterID == "" || gotVN.Status.SecurityGroupID == "" {
		t.Fatalf("unexpected VirtualNetwork status: %+v", gotVN.Status)
	}

	// Simulate drift: delete port 443 rule and add unauthorized port 3389 rule.
	pages, err := rules.List(cloudClients.Neutron, rules.ListOpts{SecGroupID: gotVN.Status.SecurityGroupID}).AllPages(ctx)
	if err != nil {
		t.Fatalf("rules.List before drift: %v", err)
	}
	liveRules, _ := rules.ExtractRules(pages)
	for _, r := range liveRules {
		if r.Direction == "ingress" && r.PortRangeMin == 443 {
			if err := rules.Delete(ctx, cloudClients.Neutron, r.ID).ExtractErr(); err != nil {
				t.Fatalf("delete 443 rule to simulate drift: %v", err)
			}
		}
	}
	if _, err := rules.Create(ctx, cloudClients.Neutron, rules.CreateOpts{
		Direction:      rules.DirIngress,
		EtherType:      rules.EtherType4,
		SecGroupID:     gotVN.Status.SecurityGroupID,
		Protocol:       rules.ProtocolTCP,
		PortRangeMin:   3389,
		PortRangeMax:   3389,
		RemoteIPPrefix: "0.0.0.0/0",
	}).Extract(); err != nil {
		t.Fatalf("inject unauthorized RDP rule: %v", err)
	}

	// Reconcile should restore 443 and remove 3389.
	if _, err := vnRec.Reconcile(ctx, vnReq); err != nil {
		t.Fatalf("VirtualNetwork drift-heal Reconcile: %v", err)
	}
	if err := k8sClient.Get(ctx, vnReq.NamespacedName, &gotVN); err != nil {
		t.Fatalf("Get VirtualNetwork after drift heal: %v", err)
	}
	if gotVN.Status.DriftHealsTotal != 2 {
		t.Fatalf("expected DriftHealsTotal=2 (1 restored + 1 removed), got %d", gotVN.Status.DriftHealsTotal)
	}
	if !apimeta.IsStatusConditionTrue(gotVN.Status.Conditions, "DriftHealed") {
		t.Fatalf("expected DriftHealed=True condition, got %+v", gotVN.Status.Conditions)
	}

	// 3. CloudWorkspace: boot volume clone, resize, live migrate, teardown.
	wsCR := &iaasv1alpha1.CloudWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "app-worker-01", Namespace: "default", Generation: 1},
		Spec: iaasv1alpha1.CloudWorkspaceSpec{
			ImageName:         imageName,
			FlavorRef:         "m1.small",
			StorageGiB:        1,
			VirtualNetworkRef: "tenant-prod",
		},
	}
	if err := k8sClient.Create(ctx, wsCR); err != nil {
		t.Fatalf("create CloudWorkspace CR: %v", err)
	}
	wsReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "app-worker-01", Namespace: "default"}}
	if _, err := wsRec.Reconcile(ctx, wsReq); err != nil {
		t.Fatalf("CloudWorkspace Reconcile: %v", err)
	}

	var gotWS iaasv1alpha1.CloudWorkspace
	if err := k8sClient.Get(ctx, wsReq.NamespacedName, &gotWS); err != nil {
		t.Fatalf("Get CloudWorkspace: %v", err)
	}
	if gotWS.Status.Phase != "Running" || gotWS.Status.ComputeHost != "compute-01" || gotWS.Status.OVNPortHost != "compute-01" {
		t.Fatalf("unexpected CloudWorkspace status: %+v", gotWS.Status)
	}
	if gotWS.Status.CephRBDParent == "" || gotWS.Status.VolumeSizeGiB != 1 {
		t.Fatalf("expected Ceph CoW parent and 1 GiB boot volume, got %+v", gotWS.Status)
	}

	// Resize boot volume 1 -> 2 GiB and live migrate compute-01 -> compute-02.
	gotWS.Spec.StorageGiB = 2
	gotWS.Spec.DesiredHost = "compute-02"
	gotWS.Generation = 2
	if err := k8sClient.Update(ctx, &gotWS); err != nil {
		t.Fatalf("Update CloudWorkspace spec: %v", err)
	}
	if _, err := wsRec.Reconcile(ctx, wsReq); err != nil {
		t.Fatalf("CloudWorkspace Day-2 Reconcile: %v", err)
	}

	if err := k8sClient.Get(ctx, wsReq.NamespacedName, &gotWS); err != nil {
		t.Fatalf("Get CloudWorkspace after Day-2 ops: %v", err)
	}
	if gotWS.Status.VolumeSizeGiB != 2 {
		t.Fatalf("expected resized VolumeSizeGiB=2, got %d", gotWS.Status.VolumeSizeGiB)
	}
	if gotWS.Status.ComputeHost != "compute-02" || gotWS.Status.OVNPortHost != "compute-02" {
		t.Fatalf("expected zero-copy live migration to compute-02 (Nova+OVN), got compute=%q ovn=%q",
			gotWS.Status.ComputeHost, gotWS.Status.OVNPortHost)
	}

	volID := gotWS.Status.VolumeID
	imgID := gotWS.Status.ImageID

	// Delete CloudWorkspace then VirtualNetwork via finalizers.
	if err := k8sClient.Delete(ctx, &gotWS); err != nil {
		t.Fatalf("Delete CloudWorkspace CR: %v", err)
	}
	if _, err := wsRec.Reconcile(ctx, wsReq); err != nil {
		t.Fatalf("CloudWorkspace finalizer Reconcile: %v", err)
	}
	if _, err := cephClient.GetRBDImage(ctx, "volumes", "volume-"+volID); !cephapi.IsNotFound(err) {
		t.Fatalf("expected Cinder RBD volume to be removed from Ceph after workspace delete, got err=%v", err)
	}

	if err := k8sClient.Delete(ctx, &gotVN); err != nil {
		t.Fatalf("Delete VirtualNetwork CR: %v", err)
	}
	if _, err := vnRec.Reconcile(ctx, vnReq); err != nil {
		t.Fatalf("VirtualNetwork finalizer Reconcile: %v", err)
	}

	// Remove test Glance base image from Ceph.
	_ = cephClient.ProtectSnapshot(ctx, "images", "glance-"+imgID, "base", false)
	_ = cephClient.DeleteSnapshot(ctx, "images", "glance-"+imgID, "base")
	_ = cephClient.DeleteRBDImage(ctx, "images", "glance-"+imgID)

	t.Logf("PASS: All 3 Kubernetes Operator controllers verified end-to-end (%s Ceph)", cephMode)
}
