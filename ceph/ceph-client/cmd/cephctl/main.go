// Command cephctl is a Go CLI for inspecting and automating a Ceph
// cluster over the ceph-mgr Dashboard REST API v1.0.
//
// Example against a local Ceph Squid container:
//
//	cephctl --url https://localhost:8443 --insecure health
//	cephctl --url https://localhost:8443 --insecure pools
//	cephctl --url https://localhost:8443 --insecure rbd-clone-demo
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

func main() {
	var (
		apiURL   = flag.String("url", envOr("CEPH_API_URL", "https://localhost:8443"), "Ceph Dashboard API base URL")
		user     = flag.String("user", envOr("CEPH_API_USER", "admin"), "Ceph Dashboard username")
		pass     = flag.String("pass", envOr("CEPH_API_PASS", "admin12345!"), "Ceph Dashboard password")
		insecure = flag.Bool("insecure", true, "Skip TLS verification for self-signed dev certs")
	)
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Println("Usage: cephctl [flags] <health|osds|pools|create-pool <name>|rbd-clone-demo>")
		os.Exit(2)
	}

	client, err := cephapi.New(cephapi.Config{
		BaseURL:            *apiURL,
		Username:           *user,
		Password:           *pass,
		InsecureSkipVerify: *insecure,
	})
	must(err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cmd := flag.Arg(0); cmd {
	case "health":
		h, err := client.Health(ctx)
		must(err)
		printJSON(h)

	case "osds":
		osds, err := client.ListOSDs(ctx)
		must(err)
		printJSON(osds)

	case "pools":
		pools, err := client.ListPools(ctx)
		must(err)
		printJSON(pools)

	case "create-pool":
		if flag.NArg() < 2 {
			must(fmt.Errorf("create-pool requires <pool-name>"))
		}
		name := flag.Arg(1)
		must(client.CreatePool(ctx, cephapi.CreatePoolRequest{
			Pool:                name,
			PoolType:            "replicated",
			PGNum:               16,
			ApplicationMetadata: []string{"rbd"},
		}))
		fmt.Printf("Created RBD pool %q\n", name)

	case "rbd-clone-demo":
		runRBDCloneDemo(ctx, client)

	default:
		must(fmt.Errorf("unknown command %q", cmd))
	}
}

func runRBDCloneDemo(ctx context.Context, c *cephapi.Client) {
	imgName := fmt.Sprintf("ubuntu-base-%d", time.Now().Unix())
	volName := fmt.Sprintf("cinder-vol-%d", time.Now().Unix())

	fmt.Printf("1. [Glance] Creating 128 MiB base RBD image images/%s...\n", imgName)
	must(c.CreateRBDImage(ctx, cephapi.CreateRBDImageRequest{
		Name:     imgName,
		PoolName: "images",
		Size:     128 << 20,
	}))

	fmt.Printf("2. [Glance] Creating & protecting snapshot images/%s@snap1...\n", imgName)
	must(c.CreateSnapshot(ctx, "images", imgName, "snap1"))
	must(c.ProtectSnapshot(ctx, "images", imgName, "snap1", true))

	fmt.Printf("3. [Cinder] Instant CoW cloning images/%s@snap1 -> volumes/%s...\n", imgName, volName)
	must(c.CloneSnapshot(ctx, "images", imgName, "snap1", cephapi.CloneSnapshotRequest{
		ChildPoolName:  "volumes",
		ChildImageName: volName,
	}))

	vol, err := c.GetRBDImage(ctx, "volumes", volName)
	must(err)
	fmt.Println("4. [Verify] Resulting Cinder RBD CoW Volume:")
	printJSON(vol)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
