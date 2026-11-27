package cephapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// MockServer is an in-memory simulator of the Ceph MGR Dashboard REST API v1.0.
// It enforces header versioning, JWT auth, snapshot protection rules, and CoW
// clone parent-child checks so tests run without Docker.
type MockServer struct {
	Server *httptest.Server

	mu       sync.RWMutex
	user     string
	pass     string
	token    string
	nextPool int
	nextSnap int64
	pools    map[string]Pool
	images   map[string]*RBDImage // key: "pool/name"
}

// NewMockServer launches an in-memory HTTP server mimicking Ceph Dashboard v1.0.
func NewMockServer(user, pass string) *MockServer {
	ms := &MockServer{
		user:     cmp.Or(user, "admin"),
		pass:     cmp.Or(pass, "admin12345!"),
		token:    "mock-ceph-jwt-token-v1",
		nextPool: 4,
		nextSnap: 1,
		pools: map[string]Pool{
			"images":  {PoolID: 1, PoolName: "images", Type: "replicated", Size: 3, MinSize: 2, PGNum: 32, CrushRule: "replicated_rack", ApplicationMetadata: []string{"rbd"}},
			"volumes": {PoolID: 2, PoolName: "volumes", Type: "replicated", Size: 3, MinSize: 2, PGNum: 32, CrushRule: "replicated_rack", ApplicationMetadata: []string{"rbd"}},
			"vms":     {PoolID: 3, PoolName: "vms", Type: "replicated", Size: 3, MinSize: 2, PGNum: 32, CrushRule: "replicated_rack", ApplicationMetadata: []string{"rbd"}},
		},
		images: make(map[string]*RBDImage),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth", ms.handleAuth)
	mux.HandleFunc("GET /api/health/minimal", ms.requireAuth(ms.handleHealth))
	mux.HandleFunc("GET /api/osd", ms.requireAuth(ms.handleListOSDs))
	mux.HandleFunc("GET /api/crush_rule", ms.requireAuth(ms.handleListCrushRules))

	mux.HandleFunc("GET /api/pool", ms.requireAuth(ms.handleListPools))
	mux.HandleFunc("POST /api/pool", ms.requireAuth(ms.handleCreatePool))
	mux.HandleFunc("GET /api/pool/{name}", ms.requireAuth(ms.handleGetPool))
	mux.HandleFunc("DELETE /api/pool/{name}", ms.requireAuth(ms.handleDeletePool))

	mux.HandleFunc("GET /api/block/image", ms.requireAuth(ms.handleListImages))
	mux.HandleFunc("POST /api/block/image", ms.requireAuth(ms.handleCreateImage))
	// Ceph Dashboard encodes pool/image with %2F; route /api/block/image/... manually
	// to preserve exact %2F path segments just like CherryPy in ceph-mgr.
	mux.HandleFunc("/api/block/image/", ms.requireAuth(ms.handleBlockImageSubroutes))

	ms.Server = httptest.NewServer(mux)
	return ms
}

// URL returns the base URL of the mock Ceph Dashboard API.
func (m *MockServer) URL() string { return m.Server.URL }

// Close shuts down the underlying httptest server.
func (m *MockServer) Close() { m.Server.Close() }

// RotateToken invalidates the previous JWT to test automatic client re-authentication.
func (m *MockServer) RotateToken(newToken string) {
	m.mu.Lock()
	m.token = newToken
	m.mu.Unlock()
}

func (m *MockServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != AcceptHeader {
			writeErr(w, http.StatusNotAcceptable, "missing required header Accept: "+AcceptHeader)
			return
		}
		m.mu.RLock()
		expected := "Bearer " + m.token
		m.mu.RUnlock()
		if r.Header.Get("Authorization") != expected {
			writeErr(w, http.StatusUnauthorized, "invalid or expired JWT")
			return
		}
		next(w, r)
	}
}

func (m *MockServer) handleAuth(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	m.mu.RLock()
	ok := body["username"] == m.user && body["password"] == m.pass
	tok := m.token
	m.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	writeJSON(w, http.StatusCreated, AuthResponse{Token: tok, Username: m.user})
}

func (m *MockServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	var h HealthMinimal
	h.Health.Status = "HEALTH_OK"
	h.OSDMap.OSDs = []OSD{
		{OSDID: 0, Up: 1, In: 1, State: []string{"exists", "up"}},
		{OSDID: 1, Up: 1, In: 1, State: []string{"exists", "up"}},
		{OSDID: 2, Up: 1, In: 1, State: []string{"exists", "up"}},
	}
	h.PGInfo.Statuses = map[string]int{"active+clean": 96}
	h.MgrMap.ActiveName = "mgr-a"
	h.DF.Stats.TotalBytes = 6 << 30
	h.DF.Stats.TotalAvailBytes = 6 << 30
	h.Hosts = 1
	writeJSON(w, http.StatusOK, h)
}

func (m *MockServer) handleListOSDs(w http.ResponseWriter, _ *http.Request) {
	osds := []OSD{
		{OSDID: 0, Up: 1, In: 1, Weight: 1.0, Host: "host-a1", State: []string{"exists", "up"}},
		{OSDID: 1, Up: 1, In: 1, Weight: 1.0, Host: "host-b1", State: []string{"exists", "up"}},
		{OSDID: 2, Up: 1, In: 1, Weight: 1.0, Host: "host-c1", State: []string{"exists", "up"}},
	}
	writeJSON(w, http.StatusOK, osds)
}

func (m *MockServer) handleListCrushRules(w http.ResponseWriter, _ *http.Request) {
	rules := []CrushRule{
		{RuleID: 0, RuleName: "replicated_rack", Type: 1},
		{RuleID: 1, RuleName: "erasure_host", Type: 3},
	}
	writeJSON(w, http.StatusOK, rules)
}

func (m *MockServer) handleListPools(w http.ResponseWriter, _ *http.Request) {
	m.mu.RLock()
	names := slices.Sorted(maps.Keys(m.pools))
	out := make([]Pool, 0, len(names))
	for _, n := range names {
		out = append(out, m.pools[n])
	}
	m.mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (m *MockServer) handleGetPool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	m.mu.RLock()
	p, ok := m.pools[name]
	m.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "pool not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (m *MockServer) handleCreatePool(w http.ResponseWriter, r *http.Request) {
	var req CreatePoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pool == "" {
		writeErr(w, http.StatusBadRequest, "pool name is required")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.pools[req.Pool]; exists {
		writeErr(w, http.StatusConflict, "pool already exists")
		return
	}
	id := m.nextPool
	m.nextPool++
	m.pools[req.Pool] = Pool{
		PoolID:              id,
		PoolName:            req.Pool,
		Type:                cmp.Or(req.PoolType, "replicated"),
		Size:                cmp.Or(req.Size, 3),
		MinSize:             cmp.Or(req.MinSize, 2),
		PGNum:               cmp.Or(req.PGNum, 32),
		CrushRule:           cmp.Or(req.RuleName, "replicated_rack"),
		ApplicationMetadata: slices.Clone(req.ApplicationMetadata),
		PGAutoscaleMode:     cmp.Or(req.PGAutoscaleMode, "on"),
		QuotaMaxBytes:       req.QuotaMaxBytes,
	}
	writeJSON(w, http.StatusCreated, m.pools[req.Pool])
}

func (m *MockServer) handleDeletePool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[name]; !ok {
		writeErr(w, http.StatusNotFound, "pool not found")
		return
	}
	delete(m.pools, name)
	w.WriteHeader(http.StatusNoContent)
}

func (m *MockServer) handleListImages(w http.ResponseWriter, r *http.Request) {
	poolFilter := r.URL.Query().Get("pool_name")
	m.mu.RLock()
	byPool := make(map[string][]RBDImage)
	for _, img := range m.images {
		if poolFilter != "" && img.PoolName != poolFilter {
			continue
		}
		byPool[img.PoolName] = append(byPool[img.PoolName], *img)
	}
	m.mu.RUnlock()

	poolNames := slices.Sorted(maps.Keys(byPool))
	resp := make([]PoolRBDListResponse, 0, len(poolNames))
	for _, pn := range poolNames {
		resp = append(resp, PoolRBDListResponse{PoolName: pn, Value: byPool[pn]})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (m *MockServer) handleCreateImage(w http.ResponseWriter, r *http.Request) {
	var req CreateRBDImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.PoolName == "" {
		writeErr(w, http.StatusBadRequest, "name and pool_name are required")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[req.PoolName]; !ok {
		writeErr(w, http.StatusNotFound, "pool not found: "+req.PoolName)
		return
	}
	key := req.PoolName + "/" + req.Name
	if _, exists := m.images[key]; exists {
		writeErr(w, http.StatusConflict, "image already exists: "+key)
		return
	}
	img := &RBDImage{
		ID:           fmt.Sprintf("rbd-%s-%s", req.PoolName, req.Name),
		Name:         req.Name,
		PoolName:     req.PoolName,
		Size:         req.Size,
		DiskUsage:    min(req.Size, 8<<20), // Simulate thin-provisioned base allocation
		FeaturesName: slices.Clone(req.Features),
	}
	m.images[key] = img
	writeJSON(w, http.StatusCreated, img)
}

func (m *MockServer) handleBlockImageSubroutes(w http.ResponseWriter, r *http.Request) {
	// Parse RawPath (preserving %2F inside image_spec) after "/api/block/image/".
	raw := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(raw, "/api/block/image/")
	if !ok || rest == "" {
		writeErr(w, http.StatusNotFound, "invalid block image route")
		return
	}
	parts := strings.Split(rest, "/")
	spec, err := url.PathUnescape(parts[0])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid image spec")
		return
	}

	// Route 1: /api/block/image/{spec}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			m.mu.RLock()
			img, exists := m.images[spec]
			m.mu.RUnlock()
			if !exists {
				writeErr(w, http.StatusNotFound, "image not found: "+spec)
				return
			}
			writeJSON(w, http.StatusOK, img)
		case http.MethodPut:
			var req ResizeRBDImageRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Size <= 0 {
				writeErr(w, http.StatusBadRequest, "positive size is required")
				return
			}
			m.mu.Lock()
			img, exists := m.images[spec]
			if exists {
				img.Size = req.Size
			}
			m.mu.Unlock()
			if !exists {
				writeErr(w, http.StatusNotFound, "image not found: "+spec)
				return
			}
			writeJSON(w, http.StatusOK, img)
		case http.MethodDelete:
			m.mu.Lock()
			defer m.mu.Unlock()
			img, exists := m.images[spec]
			if !exists {
				writeErr(w, http.StatusNotFound, "image not found: "+spec)
				return
			}
			if len(img.Snapshots) > 0 {
				writeErr(w, http.StatusConflict, "image has snapshots; remove snapshots first")
				return
			}
			delete(m.images, spec)
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// Route 2: /api/block/image/{spec}/snap
	if len(parts) == 2 && parts[1] == "snap" && r.Method == http.MethodPost {
		var req CreateSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SnapshotName == "" {
			writeErr(w, http.StatusBadRequest, "snapshot_name is required")
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		img, exists := m.images[spec]
		if !exists {
			writeErr(w, http.StatusNotFound, "image not found: "+spec)
			return
		}
		snap := RBDSnapshot{
			ID:   m.nextSnap,
			Name: req.SnapshotName,
			Size: img.Size,
		}
		m.nextSnap++
		img.Snapshots = append(img.Snapshots, snap)
		writeJSON(w, http.StatusCreated, snap)
		return
	}

	// Route 3: /api/block/image/{spec}/snap/{snap_name}
	if len(parts) == 3 && parts[1] == "snap" {
		snapName, _ := url.PathUnescape(parts[2])
		m.mu.Lock()
		defer m.mu.Unlock()
		img, exists := m.images[spec]
		if !exists {
			writeErr(w, http.StatusNotFound, "image not found")
			return
		}
		idx := slices.IndexFunc(img.Snapshots, func(s RBDSnapshot) bool { return s.Name == snapName })
		if idx < 0 {
			writeErr(w, http.StatusNotFound, "snapshot not found")
			return
		}

		switch r.Method {
		case http.MethodPut:
			var req ProtectSnapshotRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid json")
				return
			}
			if !req.IsProtected && m.hasChildClonesLocked(img.PoolName, img.Name, snapName) {
				writeErr(w, http.StatusConflict, "cannot unprotect snapshot with active CoW child clones")
				return
			}
			img.Snapshots[idx].IsProtected = req.IsProtected
			writeJSON(w, http.StatusOK, img.Snapshots[idx])
		case http.MethodDelete:
			if img.Snapshots[idx].IsProtected {
				writeErr(w, http.StatusConflict, "cannot delete protected snapshot; unprotect first")
				return
			}
			img.Snapshots = slices.Delete(img.Snapshots, idx, idx+1)
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}

	// Route 4: /api/block/image/{spec}/snap/{snap_name}/clone
	if len(parts) == 4 && parts[1] == "snap" && parts[3] == "clone" && r.Method == http.MethodPost {
		snapName, _ := url.PathUnescape(parts[2])
		var req CloneSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChildPoolName == "" || req.ChildImageName == "" {
			writeErr(w, http.StatusBadRequest, "child_pool_name and child_image_name are required")
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		srcImg, exists := m.images[spec]
		if !exists {
			writeErr(w, http.StatusNotFound, "source image not found")
			return
		}
		if _, ok := m.pools[req.ChildPoolName]; !ok {
			writeErr(w, http.StatusNotFound, "child pool not found: "+req.ChildPoolName)
			return
		}
		idx := slices.IndexFunc(srcImg.Snapshots, func(s RBDSnapshot) bool { return s.Name == snapName })
		if idx < 0 {
			writeErr(w, http.StatusNotFound, "snapshot not found")
			return
		}
		if !srcImg.Snapshots[idx].IsProtected {
			writeErr(w, http.StatusBadRequest, "snapshot must be protected before cloning (Ceph CoW invariant)")
			return
		}
		childKey := req.ChildPoolName + "/" + req.ChildImageName
		if _, exists := m.images[childKey]; exists {
			writeErr(w, http.StatusConflict, "child image already exists")
			return
		}
		child := &RBDImage{
			ID:           fmt.Sprintf("rbd-%s-%s", req.ChildPoolName, req.ChildImageName),
			Name:         req.ChildImageName,
			PoolName:     req.ChildPoolName,
			Size:         srcImg.Snapshots[idx].Size,
			DiskUsage:    0, // CoW clone starts at 0 bytes allocated
			FeaturesName: slices.Clone(req.Features),
			Parent: &RBDParent{
				PoolName:  srcImg.PoolName,
				ImageName: srcImg.Name,
				SnapName:  snapName,
			},
		}
		m.images[childKey] = child
		writeJSON(w, http.StatusCreated, child)
		return
	}

	writeErr(w, http.StatusNotFound, "unknown block image endpoint")
}

func (m *MockServer) hasChildClonesLocked(pool, image, snap string) bool {
	for _, img := range m.images {
		if img.Parent != nil && img.Parent.PoolName == pool && img.Parent.ImageName == image && img.Parent.SnapName == snap {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, statusCode int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, statusCode int, detail string) {
	writeJSON(w, statusCode, map[string]string{"detail": detail})
}
