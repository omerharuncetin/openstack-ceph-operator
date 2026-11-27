package server

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

func (s *Server) registerGlanceRoutes() {
	s.mux.HandleFunc("GET /v2/images", s.requireAuth(s.handleGlanceListImages))
	s.mux.HandleFunc("POST /v2/images", s.requireAuth(s.handleGlanceCreateImage))
	s.mux.HandleFunc("GET /v2/images/{id}", s.requireAuth(s.handleGlanceGetImage))
	s.mux.HandleFunc("PUT /v2/images/{id}/file", s.requireAuth(s.handleGlanceUploadImage))
	s.mux.HandleFunc("DELETE /v2/images/{id}", s.requireAuth(s.handleGlanceDeleteImage))
}

func (s *Server) handleGlanceListImages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nameFilter := q.Get("name")
	statusFilter := q.Get("status")
	visFilter := q.Get("visibility")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.images))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		img := s.store.images[id]
		if nameFilter != "" && img.Name != nameFilter {
			continue
		}
		if statusFilter != "" && img.Status != statusFilter {
			continue
		}
		if visFilter != "" && img.Visibility != visFilter {
			continue
		}
		out = append(out, glanceImageResponse(img))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"images": out,
		"schema": "/v2/schemas/images",
	})
}

func (s *Server) handleGlanceCreateImage(w http.ResponseWriter, r *http.Request) {
	var raw map[string]any
	if err := decodeJSON(r, &raw); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Glance image payload")
		return
	}

	tok := tokenFromContext(r.Context())
	id := cmp.Or(stringField(raw, "id"), newUUID())
	now := time.Now().UTC()

	props := make(map[string]string)
	standardKeys := map[string]struct{}{
		"id": {}, "name": {}, "visibility": {}, "container_format": {},
		"disk_format": {}, "min_disk": {}, "min_ram": {}, "protected": {},
		"os_hidden": {}, "tags": {}, "owner": {},
	}
	for k, v := range raw {
		if _, isStd := standardKeys[k]; isStd {
			continue
		}
		if sv, ok := v.(string); ok {
			props[k] = sv
		}
	}
	// Also accept nested "properties" map if a caller sends one.
	if nested, ok := raw["properties"].(map[string]any); ok {
		for k, v := range nested {
			if sv, ok := v.(string); ok {
				props[k] = sv
			}
		}
	}

	var tags []string
	if rawTags, ok := raw["tags"].([]any); ok {
		for _, item := range rawTags {
			if str, ok := item.(string); ok {
				tags = append(tags, str)
			}
		}
	}

	img := &ImageRecord{
		ID:              id,
		Name:            cmp.Or(stringField(raw, "name"), "image-"+id[:8]),
		Status:          "queued",
		Visibility:      cmp.Or(stringField(raw, "visibility"), "private"),
		ContainerFormat: cmp.Or(stringField(raw, "container_format"), "bare"),
		DiskFormat:      cmp.Or(stringField(raw, "disk_format"), "raw"),
		MinDisk:         intField(raw, "min_disk"),
		MinRAM:          intField(raw, "min_ram"),
		Protected:       boolField(raw, "protected"),
		Hidden:          boolField(raw, "os_hidden"),
		Owner:           cmp.Or(stringField(raw, "owner"), tok.ProjectID),
		Tags:            tags,
		Properties:      props,
		CreatedAt:       now,
		UpdatedAt:       now,
		RBDPool:         s.cephImagesPool,
		RBDImage:        "glance-" + id,
		RBDSnap:         "base",
	}

	s.store.mu.Lock()
	if _, exists := s.store.images[id]; exists {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusConflict, "image already exists: "+id)
		return
	}
	s.store.images[id] = img
	s.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, glanceImageResponse(img))
}

func (s *Server) handleGlanceGetImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	img, ok := s.store.images[id]
	var resp map[string]any
	if ok {
		resp = glanceImageResponse(img)
	}
	s.store.mu.RUnlock()

	if !ok {
		writeAPIError(w, http.StatusNotFound, "image not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGlanceUploadImage implements PUT /v2/images/{id}/file.
// It creates a thin-provisioned RBD image in the "images" pool, snapshots it
// at @base, and protects the snapshot so Cinder can create CoW clones.
func (s *Server) handleGlanceUploadImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	defer r.Body.Close()

	s.store.mu.RLock()
	img, ok := s.store.images[id]
	if !ok {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusNotFound, "image not found: "+id)
		return
	}
	if img.Status != "queued" {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusConflict, fmt.Sprintf("image %s is in status %q, expected queued", id, img.Status))
		return
	}
	rbdPool := img.RBDPool
	rbdImage := img.RBDImage
	rbdSnap := img.RBDSnap
	minDisk := img.MinDisk
	s.store.mu.RUnlock()

	hasher := sha256.New()
	uploadedBytes, err := io.Copy(hasher, r.Body)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "failed to read image payload: "+err.Error())
		return
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	virtualSize := max(int64(minDisk)<<30, uploadedBytes)
	if virtualSize < DefaultGlanceImageBytes {
		virtualSize = DefaultGlanceImageBytes
	}

	ctx := r.Context()
	if err := s.ceph.CreateRBDImage(ctx, cephapi.CreateRBDImageRequest{
		Name:     rbdImage,
		PoolName: rbdPool,
		Size:     virtualSize,
	}); err != nil {
		writeCephError(w, "ceph CreateRBDImage for glance image", err)
		return
	}

	if err := s.ceph.CreateSnapshot(ctx, rbdPool, rbdImage, rbdSnap); err != nil {
		_ = s.ceph.DeleteRBDImage(ctx, rbdPool, rbdImage)
		writeCephError(w, "ceph CreateSnapshot for glance image", err)
		return
	}

	if err := s.ceph.ProtectSnapshot(ctx, rbdPool, rbdImage, rbdSnap, true); err != nil {
		_ = s.ceph.DeleteSnapshot(ctx, rbdPool, rbdImage, rbdSnap)
		_ = s.ceph.DeleteRBDImage(ctx, rbdPool, rbdImage)
		writeCephError(w, "ceph ProtectSnapshot for glance image", err)
		return
	}

	s.store.mu.Lock()
	if cur, exists := s.store.images[id]; exists {
		cur.Status = "active"
		cur.Checksum = checksum
		cur.SizeBytes = uploadedBytes
		cur.VirtualSize = virtualSize
		cur.UpdatedAt = time.Now().UTC()
		if cur.Properties == nil {
			cur.Properties = make(map[string]string)
		}
		cur.Properties["rbd_pool"] = rbdPool
		cur.Properties["rbd_image"] = rbdImage
		cur.Properties["rbd_snap"] = rbdSnap
	}
	s.store.mu.Unlock()

	s.logger.Info("glance image uploaded to ceph rbd",
		"image_id", id,
		"rbd", fmt.Sprintf("%s/%s@%s", rbdPool, rbdImage, rbdSnap),
		"virtual_bytes", virtualSize,
	)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGlanceDeleteImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.store.mu.RLock()
	img, ok := s.store.images[id]
	if !ok {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusNotFound, "image not found: "+id)
		return
	}
	if img.Protected {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusForbidden, "image is protected and cannot be deleted")
		return
	}
	isActive := img.Status == "active"
	rbdPool := img.RBDPool
	rbdImage := img.RBDImage
	rbdSnap := img.RBDSnap
	s.store.mu.RUnlock()

	if isActive {
		ctx := r.Context()
		// Ceph invariant: unprotecting fails with 409 Conflict if any
		// Cinder CoW child volumes still depend on this Glance @base snapshot.
		if err := s.ceph.ProtectSnapshot(ctx, rbdPool, rbdImage, rbdSnap, false); err != nil && !cephapi.IsNotFound(err) {
			writeCephError(w, "ceph unprotect glance snapshot (check active Cinder CoW clones)", err)
			return
		}
		if err := s.ceph.DeleteSnapshot(ctx, rbdPool, rbdImage, rbdSnap); err != nil && !cephapi.IsNotFound(err) {
			writeCephError(w, "ceph delete glance snapshot", err)
			return
		}
		if err := s.ceph.DeleteRBDImage(ctx, rbdPool, rbdImage); err != nil && !cephapi.IsNotFound(err) {
			writeCephError(w, "ceph delete glance rbd image", err)
			return
		}
	}

	s.store.mu.Lock()
	delete(s.store.images, id)
	s.store.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

func glanceImageResponse(img *ImageRecord) map[string]any {
	tags := img.Tags
	if tags == nil {
		tags = []string{}
	}
	m := map[string]any{
		"id":               img.ID,
		"name":             img.Name,
		"status":           img.Status,
		"visibility":       img.Visibility,
		"container_format": img.ContainerFormat,
		"disk_format":      img.DiskFormat,
		"min_disk":         img.MinDisk,
		"min_ram":          img.MinRAM,
		"protected":        img.Protected,
		"os_hidden":        img.Hidden,
		"owner":            img.Owner,
		"tags":             tags,
		"checksum":         img.Checksum,
		"size":             img.SizeBytes,
		"virtual_size":     img.VirtualSize,
		"created_at":       img.CreatedAt.Format(time.RFC3339),
		"updated_at":       img.UpdatedAt.Format(time.RFC3339),
		"file":             "/v2/images/" + img.ID + "/file",
		"self":             "/v2/images/" + img.ID,
		"schema":           "/v2/schemas/image",
	}
	// Glance v2 exposes custom properties as top-level JSON keys.
	for k, v := range img.Properties {
		if _, reserved := m[k]; !reserved {
			m[k] = v
		}
	}
	return m
}

func stringField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func boolField(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}
