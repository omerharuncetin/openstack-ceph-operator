// Command mini-openstack runs an OpenStack API server (Keystone v3, Glance v2,
// Cinder v3, Neutron v2/OVN, Nova v2.1) backed by Ceph via pkg/cephapi.
// If CEPH_API_URL is empty, it starts an in-memory cephapi.MockServer.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
	"github.com/omerharuncetin/openstack-ceph-operator/openstack/mini-openstack/pkg/server"
)

func main() {
	var (
		addr         = flag.String("addr", cmp.Or(os.Getenv("OPENSTACK_ADDR"), ":5000"), "HTTP listen address")
		publicURL    = flag.String("public-url", os.Getenv("OPENSTACK_PUBLIC_URL"), "Optional public URL advertised in Keystone catalog")
		osUser       = flag.String("os-user", cmp.Or(os.Getenv("OS_USERNAME"), server.DefaultUsername), "Keystone username")
		osPass       = flag.String("os-pass", cmp.Or(os.Getenv("OS_PASSWORD"), server.DefaultPassword), "Keystone password")
		osRegion     = flag.String("os-region", cmp.Or(os.Getenv("OS_REGION_NAME"), server.DefaultRegion), "Keystone region")
		cephURL      = flag.String("ceph-url", os.Getenv("CEPH_API_URL"), "Ceph MGR Dashboard API URL (if empty, starts in-memory cephapi.MockServer)")
		cephUser     = flag.String("ceph-user", cmp.Or(os.Getenv("CEPH_API_USER"), "admin"), "Ceph Dashboard username")
		cephPass     = flag.String("ceph-pass", cmp.Or(os.Getenv("CEPH_API_PASS"), "admin12345!"), "Ceph Dashboard password")
		cephInsecure = flag.Bool("ceph-insecure", true, "Skip TLS verification for Ceph self-signed certs")
		debug        = flag.Bool("debug", false, "Enable verbose request logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	var (
		mockCeph *cephapi.MockServer
		cephMode = "live"
		apiURL   = *cephURL
	)
	if apiURL == "" {
		cephMode = "mock"
		mockCeph = cephapi.NewMockServer(*cephUser, *cephPass)
		defer mockCeph.Close()
		apiURL = mockCeph.URL()
		logger.Info("CEPH_API_URL not set; using in-process cephapi.MockServer", "mock_url", apiURL)
	}

	cephClient, err := cephapi.New(cephapi.Config{
		BaseURL:            apiURL,
		Username:           *cephUser,
		Password:           *cephPass,
		InsecureSkipVerify: *cephInsecure,
	})
	if err != nil {
		logger.Error("failed to initialize cephapi client", "error", err)
		os.Exit(1)
	}

	srv, err := server.New(server.Config{
		CephClient: cephClient,
		CephMode:   cephMode,
		PublicURL:  *publicURL,
		Username:   *osUser,
		Password:   *osPass,
		Region:     *osRegion,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("failed to initialize mini-openstack server", "error", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		logger.Info("shutting down mini-openstack")
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("mini-openstack control plane listening",
		"addr", *addr,
		"keystone_endpoint", "http://localhost"+*addr+"/v3",
		"ceph_mode", cephMode,
		"ceph_url", apiURL,
		"region", *osRegion,
	)

	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server exited with error", "error", err)
		os.Exit(1)
	}
}
