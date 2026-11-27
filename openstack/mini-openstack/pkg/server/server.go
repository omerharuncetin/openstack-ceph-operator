package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

// Config configures a mini-openstack control-plane Server instance.
type Config struct {
	// CephClient talks to either a live Ceph Squid MGR Dashboard REST API or
	// cephapi.MockServer for offline CI/testing. Required.
	CephClient *cephapi.Client
	// CephMode describes the backend ("live" or "mock") for /healthz reporting.
	CephMode string
	// CephImagesPool is the Ceph RBD pool for Glance base images (default "images").
	CephImagesPool string
	// CephVolumesPool is the Ceph RBD pool for Cinder volumes/clones (default "volumes").
	CephVolumesPool string

	// PublicURL optionally pins the external URL advertised in Keystone's
	// Service Catalog (e.g. "http://localhost:5000"). When empty, it is derived
	// dynamically from each request's scheme and Host header.
	PublicURL string

	// Keystone credentials and region defaults.
	Username string
	Password string
	Region   string

	// ComputeHosts is the simulated Nova hypervisor inventory for scheduling
	// and live-migration drills (default: compute-01, compute-02, compute-03).
	ComputeHosts []string

	// Logger receives structured request and lifecycle logs.
	Logger *slog.Logger
}

// Server implements the mini-openstack HTTP control plane across Keystone v3,
// Glance v2, Cinder v3, Neutron v2, and Nova v2.1.
type Server struct {
	ceph            *cephapi.Client
	cephMode        string
	cephImagesPool  string
	cephVolumesPool string
	publicURL       string
	username        string
	password        string
	region          string
	logger          *slog.Logger

	store *store
	mux   *http.ServeMux
}

type tokenCtxKey struct{}

// New constructs a configured mini-openstack Server ready to serve HTTP traffic.
func New(cfg Config) (*Server, error) {
	if cfg.CephClient == nil {
		return nil, errors.New("mini-openstack: Config.CephClient is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	s := &Server{
		ceph:            cfg.CephClient,
		cephMode:        cmp.Or(cfg.CephMode, "live"),
		cephImagesPool:  cmp.Or(cfg.CephImagesPool, DefaultCephImagesPool),
		cephVolumesPool: cmp.Or(cfg.CephVolumesPool, DefaultCephVolumesPool),
		publicURL:       strings.TrimRight(cfg.PublicURL, "/"),
		username:        cmp.Or(cfg.Username, DefaultUsername),
		password:        cmp.Or(cfg.Password, DefaultPassword),
		region:          cmp.Or(cfg.Region, DefaultRegion),
		logger:          logger,
		store:           newStore(cfg.ComputeHosts),
		mux:             http.NewServeMux(),
	}

	s.registerRoutes()
	return s, nil
}

func (s *Server) registerRoutes() {
	// Operational health endpoint.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Keystone v3 (Identity & Service Catalog).
	s.registerKeystoneRoutes()

	// Glance v2 (Image Service -> Ceph images pool).
	s.registerGlanceRoutes()

	// Cinder v3 (Block Storage -> Ceph volumes pool & CoW clones).
	s.registerCinderRoutes()

	// Neutron v2 (Networking, L3 Routers, Security Groups & OVN Ports).
	s.registerNeutronRoutes()

	// Nova v2.1 (Compute, Volume Attachments & Zero-Copy Live Migration).
	s.registerNovaRoutes()
}

// ServeHTTP normalizes trailing slashes (avoiding 301 redirects on POST/PUT)
// and dispatches requests through the configured ServeMux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Path) > 1 && strings.HasSuffix(r.URL.Path, "/") {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimRight(r.URL.Path, "/")
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		r = r2
	}
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(sw, r)
	s.logger.Debug("openstack request",
		"method", r.Method,
		"path", r.URL.Path,
		"status", sw.status,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	health, err := s.ceph.Health(r.Context())
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":    "degraded",
			"ceph_mode": s.cephMode,
			"error":     err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"ceph_mode":    s.cephMode,
		"ceph_status":  health.Health.Status,
		"ceph_osds":    health.NumOSDs(),
		"ceph_osds_up": health.NumUpOSDs(),
		"ceph_osds_in": health.NumInOSDs(),
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokenID := r.Header.Get("X-Auth-Token")
		if tokenID == "" {
			writeAPIError(w, http.StatusUnauthorized, "missing X-Auth-Token header")
			return
		}

		s.store.mu.RLock()
		tok, ok := s.store.tokens[tokenID]
		s.store.mu.RUnlock()

		if !ok || time.Now().UTC().After(tok.ExpiresAt) {
			writeAPIError(w, http.StatusUnauthorized, "invalid or expired Keystone token")
			return
		}

		ctx := context.WithValue(r.Context(), tokenCtxKey{}, tok)
		next(w, r.WithContext(ctx))
	}
}

func tokenFromContext(ctx context.Context) *TokenRecord {
	tok, _ := ctx.Value(tokenCtxKey{}).(*TokenRecord)
	return tok
}

func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, r.Host)
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]any{
		"error": map[string]any{
			"code":    statusCode,
			"title":   http.StatusText(statusCode),
			"message": message,
		},
		"message": message,
	})
}

func writeCephError(w http.ResponseWriter, operation string, err error) {
	if apiErr, ok := errors.AsType[*cephapi.APIError](err); ok {
		status := apiErr.StatusCode
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		writeAPIError(w, status, fmt.Sprintf("%s: %s", operation, apiErr.Detail))
		return
	}
	writeAPIError(w, http.StatusBadGateway, fmt.Sprintf("%s: %v", operation, err))
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
}
