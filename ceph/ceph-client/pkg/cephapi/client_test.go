package cephapi_test

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

func TestCephDashboardContractAndOpenStackRBDCoWFlow(t *testing.T) {
	ctx := t.Context()
	mock := cephapi.NewMockServer("admin", "admin12345!")
	defer mock.Close()

	c, err := cephapi.New(cephapi.Config{
		BaseURL:  mock.URL(),
		Username: "admin",
		Password: "admin12345!",
	})
	if err != nil {
		t.Fatalf("cephapi.New: %v", err)
	}

	// 1. Health, OSDs, CRUSH rules & Pools
	health, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Health.Status != "HEALTH_OK" || health.NumUpOSDs() != 3 {
		t.Fatalf("unexpected health: %+v", health)
	}

	osds, err := c.ListOSDs(ctx)
	if err != nil || len(osds) != 3 {
		t.Fatalf("ListOSDs: got %d err=%v", len(osds), err)
	}

	rules, err := c.ListCrushRules(ctx)
	if err != nil || len(rules) < 2 {
		t.Fatalf("ListCrushRules: got %v err=%v", rules, err)
	}

	// Verify automatic JWT re-authentication on token rotation (401 retry)
	mock.RotateToken("mock-ceph-jwt-token-v2")
	if err := c.CreatePool(ctx, cephapi.CreatePoolRequest{
		Pool:                "tenant-gold",
		PoolType:            "replicated",
		PGNum:               32,
		Size:                3,
		MinSize:             2,
		ApplicationMetadata: []string{"rbd"},
	}); err != nil {
		t.Fatalf("CreatePool after JWT rotation: %v", err)
	}

	pool, err := c.GetPool(ctx, "tenant-gold")
	if err != nil || pool.PoolName != "tenant-gold" {
		t.Fatalf("GetPool: got %+v err=%v", pool, err)
	}

	// 2. OpenStack Glance -> Cinder RBD CoW Clone Lifecycle
	const size10GiB = int64(10) << 30
	if err := c.CreateRBDImage(ctx, cephapi.CreateRBDImageRequest{
		Name:     "ubuntu-24.04",
		PoolName: "images",
		Size:     size10GiB,
	}); err != nil {
		t.Fatalf("CreateRBDImage: %v", err)
	}

	if err := c.CreateSnapshot(ctx, "images", "ubuntu-24.04", "base"); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Ceph invariant: cloning an unprotected snapshot must fail.
	err = c.CloneSnapshot(ctx, "images", "ubuntu-24.04", "base", cephapi.CloneSnapshotRequest{
		ChildPoolName:  "volumes",
		ChildImageName: "cinder-vol-01",
	})
	var apiErr *cephapi.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 cloning unprotected snapshot, got %v", err)
	}

	// Protect snapshot and clone into volumes pool (0 bytes allocated initially).
	if err := c.ProtectSnapshot(ctx, "images", "ubuntu-24.04", "base", true); err != nil {
		t.Fatalf("ProtectSnapshot: %v", err)
	}
	if err := c.CloneSnapshot(ctx, "images", "ubuntu-24.04", "base", cephapi.CloneSnapshotRequest{
		ChildPoolName:  "volumes",
		ChildImageName: "cinder-vol-01",
	}); err != nil {
		t.Fatalf("CloneSnapshot: %v", err)
	}

	clone, err := c.GetRBDImage(ctx, "volumes", "cinder-vol-01")
	if err != nil {
		t.Fatalf("GetRBDImage clone: %v", err)
	}
	if clone.DiskUsage != 0 {
		t.Errorf("expected 0 initial DiskUsage for CoW clone, got %d", clone.DiskUsage)
	}
	if clone.Parent == nil || clone.Parent.PoolName != "images" || clone.Parent.ImageName != "ubuntu-24.04" || clone.Parent.SnapName != "base" {
		t.Fatalf("unexpected clone parent lineage: %+v", clone.Parent)
	}

	// Online Cinder volume resize (10 GiB -> 20 GiB)
	if err := c.ResizeRBDImage(ctx, "volumes", "cinder-vol-01", 20<<30); err != nil {
		t.Fatalf("ResizeRBDImage: %v", err)
	}
	clone, _ = c.GetRBDImage(ctx, "volumes", "cinder-vol-01")
	if clone.Size != 20<<30 {
		t.Fatalf("expected resized volume 20 GiB, got %d", clone.Size)
	}

	// Ceph invariant: cannot unprotect a snapshot while a child clone exists.
	err = c.ProtectSnapshot(ctx, "images", "ubuntu-24.04", "base", false)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 unprotecting snapshot with active child clone, got %v", err)
	}

	// Ordered teardown: delete child clone -> unprotect -> delete snapshot -> delete base image -> delete pool
	if err := c.DeleteRBDImage(ctx, "volumes", "cinder-vol-01"); err != nil {
		t.Fatalf("DeleteRBDImage child: %v", err)
	}
	if err := c.ProtectSnapshot(ctx, "images", "ubuntu-24.04", "base", false); err != nil {
		t.Fatalf("unprotect snapshot: %v", err)
	}
	if err := c.DeleteSnapshot(ctx, "images", "ubuntu-24.04", "base"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if err := c.DeleteRBDImage(ctx, "images", "ubuntu-24.04"); err != nil {
		t.Fatalf("DeleteRBDImage base: %v", err)
	}
	if err := c.DeletePool(ctx, "tenant-gold"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}
	if _, err := c.GetPool(ctx, "tenant-gold"); !cephapi.IsNotFound(err) {
		t.Fatalf("expected IsNotFound after DeletePool, got %v", err)
	}
}

// TestLiveCephSquidIntegration runs against a Ceph Squid (v19) container
// when CEPH_API_URL is set (e.g. CEPH_API_URL=https://localhost:8443).
func TestLiveCephSquidIntegration(t *testing.T) {
	apiURL := os.Getenv("CEPH_API_URL")
	if apiURL == "" {
		t.Skip("CEPH_API_URL not set; skipping live Ceph Squid integration test")
	}
	user := os.Getenv("CEPH_API_USER")
	if user == "" {
		user = "admin"
	}
	pass := os.Getenv("CEPH_API_PASS")
	if pass == "" {
		pass = "admin12345!"
	}

	ctx := t.Context()
	c, err := cephapi.New(cephapi.Config{
		BaseURL:            apiURL,
		Username:           user,
		Password:           pass,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("cephapi.New: %v", err)
	}

	health, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("live Health failed: %v", err)
	}
	t.Logf("Live Ceph cluster status=%s osds=%d up=%d in=%d pgs=%v total_bytes=%d", health.Health.Status, health.NumOSDs(), health.NumUpOSDs(), health.NumInOSDs(), health.PGInfo.Statuses, health.DF.Stats.TotalBytes)

	pools, err := c.ListPools(ctx)
	if err != nil {
		t.Fatalf("live ListPools failed: %v", err)
	}
	t.Logf("Live Ceph pools (%d): %+v", len(pools), pools)
}
