package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

func (s *Server) registerCinderRoutes() {
	s.mux.HandleFunc("GET /v3/{project}/volumes", s.requireAuth(s.handleCinderListVolumes))
	s.mux.HandleFunc("GET /v3/{project}/volumes/detail", s.requireAuth(s.handleCinderListVolumes))
	s.mux.HandleFunc("POST /v3/{project}/volumes", s.requireAuth(s.handleCinderCreateVolume))
	s.mux.HandleFunc("GET /v3/{project}/volumes/{id}", s.requireAuth(s.handleCinderGetVolume))
	s.mux.HandleFunc("PUT /v3/{project}/volumes/{id}", s.requireAuth(s.handleCinderUpdateVolume))
	s.mux.HandleFunc("DELETE /v3/{project}/volumes/{id}", s.requireAuth(s.handleCinderDeleteVolume))
	s.mux.HandleFunc("POST /v3/{project}/volumes/{id}/action", s.requireAuth(s.handleCinderVolumeAction))
}

type cinderCreateVolumeParams struct {
	Name             string
	Description      string
	SizeGiB          int
	ImageRef         string
	AvailabilityZone string
	VolumeType       string
	Metadata         map[string]string
}

func (s *Server) handleCinderListVolumes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nameFilter := q.Get("name")
	statusFilter := q.Get("status")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.volumes))
	vols := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		v := s.store.volumes[id]
		if nameFilter != "" && v.Name != nameFilter {
			continue
		}
		if statusFilter != "" && v.Status != statusFilter {
			continue
		}
		vols = append(vols, cinderVolumeResponse(v))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

func (s *Server) handleCinderCreateVolume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Volume struct {
			Name             string            `json:"name"`
			Description      string            `json:"description"`
			Size             int               `json:"size"`
			ImageRef         string            `json:"imageRef"`
			AvailabilityZone string            `json:"availability_zone"`
			VolumeType       string            `json:"volume_type"`
			Metadata         map[string]string `json:"metadata"`
		} `json:"volume"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Cinder volume create payload")
		return
	}

	tok := tokenFromContext(r.Context())
	projectID := cmp.Or(r.PathValue("project"), tok.ProjectID)

	vol, statusCode, err := s.provisionCinderVolume(r.Context(), projectID, tok.UserID, cinderCreateVolumeParams{
		Name:             body.Volume.Name,
		Description:      body.Volume.Description,
		SizeGiB:          body.Volume.Size,
		ImageRef:         body.Volume.ImageRef,
		AvailabilityZone: body.Volume.AvailabilityZone,
		VolumeType:       body.Volume.VolumeType,
		Metadata:         body.Volume.Metadata,
	})
	if err != nil {
		if statusCode == http.StatusBadGateway {
			writeCephError(w, "cinder ceph provisioning", err)
			return
		}
		writeAPIError(w, statusCode, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"volume": cinderVolumeResponse(vol)})
}

// provisionCinderVolume backs a Cinder volume with a Ceph RBD image in
// the "volumes" pool. When ImageRef is set, it runs a Copy-on-Write
// RBD clone from the Glance image's protected @base snapshot.
func (s *Server) provisionCinderVolume(ctx context.Context, projectID, userID string, p cinderCreateVolumeParams) (*VolumeRecord, int, error) {
	sizeGiB := cmp.Or(p.SizeGiB, 1)
	if sizeGiB <= 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("volume size must be positive, got %d", sizeGiB)
	}

	volID := newUUID()
	rbdPool := s.cephVolumesPool
	rbdImage := "volume-" + volID
	targetBytes := int64(sizeGiB) << 30

	meta := maps.Clone(p.Metadata)
	if meta == nil {
		meta = make(map[string]string)
	}
	meta["rbd_pool"] = rbdPool
	meta["rbd_image"] = rbdImage

	var (
		bootable     bool
		rbdParent    string
		imgMeta      map[string]string
		sourceImgRec *ImageRecord
	)

	if p.ImageRef != "" {
		s.store.mu.RLock()
		img, ok := s.store.images[p.ImageRef]
		if ok {
			imgCopy := *img
			sourceImgRec = &imgCopy
		}
		s.store.mu.RUnlock()

		if !ok {
			return nil, http.StatusNotFound, fmt.Errorf("source Glance image %q not found", p.ImageRef)
		}
		if sourceImgRec.Status != "active" {
			return nil, http.StatusBadRequest, fmt.Errorf("source Glance image %q is in status %q, expected active", p.ImageRef, sourceImgRec.Status)
		}

		// CoW clone: images/glance-<imgID>@base -> volumes/volume-<volID>
		if err := s.ceph.CloneSnapshot(ctx, sourceImgRec.RBDPool, sourceImgRec.RBDImage, sourceImgRec.RBDSnap, cephapi.CloneSnapshotRequest{
			ChildPoolName:  rbdPool,
			ChildImageName: rbdImage,
		}); err != nil {
			return nil, http.StatusBadGateway, err
		}

		// Expand cloned RBD volume if requested GiB exceeds base image virtual size.
		if targetBytes > sourceImgRec.VirtualSize {
			if err := s.ceph.ResizeRBDImage(ctx, rbdPool, rbdImage, targetBytes); err != nil {
				_ = s.ceph.DeleteRBDImage(ctx, rbdPool, rbdImage)
				return nil, http.StatusBadGateway, err
			}
		}

		bootable = true
		rbdParent = fmt.Sprintf("%s/%s@%s", sourceImgRec.RBDPool, sourceImgRec.RBDImage, sourceImgRec.RBDSnap)
		meta["rbd_parent"] = rbdParent
		imgMeta = map[string]string{
			"image_id":         sourceImgRec.ID,
			"image_name":       sourceImgRec.Name,
			"disk_format":      sourceImgRec.DiskFormat,
			"container_format": sourceImgRec.ContainerFormat,
			"checksum":         sourceImgRec.Checksum,
		}
	} else {
		if err := s.ceph.CreateRBDImage(ctx, cephapi.CreateRBDImageRequest{
			Name:     rbdImage,
			PoolName: rbdPool,
			Size:     targetBytes,
		}); err != nil {
			return nil, http.StatusBadGateway, err
		}
	}

	now := time.Now().UTC()
	vol := &VolumeRecord{
		ID:                  volID,
		Name:                cmp.Or(p.Name, "volume-"+volID[:8]),
		Description:         p.Description,
		Status:              "available",
		SizeGiB:             sizeGiB,
		AvailabilityZone:    cmp.Or(p.AvailabilityZone, DefaultAZ),
		VolumeType:          cmp.Or(p.VolumeType, "ceph-rbd"),
		Bootable:            bootable,
		ProjectID:           projectID,
		UserID:              userID,
		ImageID:             p.ImageRef,
		Host:                "cinder-ceph@rbd#" + rbdPool,
		Metadata:            meta,
		VolumeImageMetadata: imgMeta,
		CreatedAt:           now,
		UpdatedAt:           now,
		RBDPool:             rbdPool,
		RBDImage:            rbdImage,
		RBDParent:           rbdParent,
	}

	s.store.mu.Lock()
	s.store.volumes[volID] = vol
	s.store.mu.Unlock()

	s.logger.Info("cinder volume provisioned in ceph",
		"volume_id", volID,
		"size_gib", sizeGiB,
		"rbd", rbdPool+"/"+rbdImage,
		"cow_parent", rbdParent,
	)

	return vol, http.StatusAccepted, nil
}

func (s *Server) handleCinderGetVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	vol, ok := s.store.volumes[id]
	var resp map[string]any
	if ok {
		resp = cinderVolumeResponse(vol)
	}
	s.store.mu.RUnlock()

	if !ok {
		writeAPIError(w, http.StatusNotFound, "volume not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"volume": resp})
}

func (s *Server) handleCinderUpdateVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Volume struct {
			Name        *string           `json:"name"`
			Description *string           `json:"description"`
			Metadata    map[string]string `json:"metadata"`
		} `json:"volume"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Cinder volume update payload")
		return
	}

	s.store.mu.Lock()
	vol, ok := s.store.volumes[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "volume not found: "+id)
		return
	}
	if body.Volume.Name != nil {
		vol.Name = *body.Volume.Name
	}
	if body.Volume.Description != nil {
		vol.Description = *body.Volume.Description
	}
	if body.Volume.Metadata != nil {
		if vol.Metadata == nil {
			vol.Metadata = make(map[string]string)
		}
		maps.Copy(vol.Metadata, body.Volume.Metadata)
	}
	vol.UpdatedAt = time.Now().UTC()
	resp := cinderVolumeResponse(vol)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"volume": resp})
}

func (s *Server) handleCinderDeleteVolume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.store.mu.RLock()
	vol, ok := s.store.volumes[id]
	if !ok {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusNotFound, "volume not found: "+id)
		return
	}
	if vol.Status == "in-use" || len(vol.Attachments) > 0 {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusBadRequest, "volume is in-use; detach from Nova server before deleting")
		return
	}
	rbdPool := vol.RBDPool
	rbdImage := vol.RBDImage
	s.store.mu.RUnlock()

	if err := s.ceph.DeleteRBDImage(r.Context(), rbdPool, rbdImage); err != nil && !cephapi.IsNotFound(err) {
		writeCephError(w, "ceph DeleteRBDImage for cinder volume", err)
		return
	}

	s.store.mu.Lock()
	delete(s.store.volumes, id)
	s.store.mu.Unlock()

	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleCinderVolumeAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var actions map[string]json.RawMessage
	if err := decodeJSON(r, &actions); err != nil || len(actions) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid Cinder volume action payload")
		return
	}

	if rawExtend, ok := actions["os-extend"]; ok {
		var req struct {
			NewSize int `json:"new_size"`
		}
		if err := json.Unmarshal(rawExtend, &req); err != nil || req.NewSize <= 0 {
			writeAPIError(w, http.StatusBadRequest, "os-extend requires positive new_size")
			return
		}

		s.store.mu.RLock()
		vol, exists := s.store.volumes[id]
		if !exists {
			s.store.mu.RUnlock()
			writeAPIError(w, http.StatusNotFound, "volume not found: "+id)
			return
		}
		curSize := vol.SizeGiB
		rbdPool := vol.RBDPool
		rbdImage := vol.RBDImage
		s.store.mu.RUnlock()

		if req.NewSize <= curSize {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("new_size (%d GiB) must be greater than current size (%d GiB)", req.NewSize, curSize))
			return
		}

		// Run Ceph online RBD resize.
		if err := s.ceph.ResizeRBDImage(r.Context(), rbdPool, rbdImage, int64(req.NewSize)<<30); err != nil {
			writeCephError(w, "ceph ResizeRBDImage for os-extend", err)
			return
		}

		s.store.mu.Lock()
		if cur, exists := s.store.volumes[id]; exists {
			cur.SizeGiB = req.NewSize
			cur.UpdatedAt = time.Now().UTC()
		}
		s.store.mu.Unlock()

		w.WriteHeader(http.StatusAccepted)
		return
	}

	if rawBootable, ok := actions["os-set_bootable"]; ok {
		var req struct {
			Bootable bool `json:"bootable"`
		}
		if err := json.Unmarshal(rawBootable, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid os-set_bootable payload")
			return
		}
		s.store.mu.Lock()
		vol, exists := s.store.volumes[id]
		if !exists {
			s.store.mu.Unlock()
			writeAPIError(w, http.StatusNotFound, "volume not found: "+id)
			return
		}
		vol.Bootable = req.Bootable
		vol.UpdatedAt = time.Now().UTC()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}

	if rawReset, ok := actions["os-reset_status"]; ok {
		var req struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rawReset, &req); err != nil || req.Status == "" {
			writeAPIError(w, http.StatusBadRequest, "invalid os-reset_status payload")
			return
		}
		s.store.mu.Lock()
		vol, exists := s.store.volumes[id]
		if !exists {
			s.store.mu.Unlock()
			writeAPIError(w, http.StatusNotFound, "volume not found: "+id)
			return
		}
		vol.Status = req.Status
		vol.UpdatedAt = time.Now().UTC()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	writeAPIError(w, http.StatusBadRequest, "unsupported Cinder volume action")
}

func cinderVolumeResponse(v *VolumeRecord) map[string]any {
	atts := make([]map[string]any, 0, len(v.Attachments))
	for _, a := range v.Attachments {
		atts = append(atts, map[string]any{
			"id":            a.ID,
			"attachment_id": a.AttachmentID,
			"volume_id":     a.VolumeID,
			"server_id":     a.ServerID,
			"host_name":     a.HostName,
			"device":        a.Device,
			"attached_at":   formatCinderTime(a.AttachedAt),
		})
	}

	meta := v.Metadata
	if meta == nil {
		meta = map[string]string{}
	}

	resp := map[string]any{
		"id":                           v.ID,
		"name":                         v.Name,
		"description":                  v.Description,
		"status":                       v.Status,
		"size":                         v.SizeGiB,
		"availability_zone":            v.AvailabilityZone,
		"volume_type":                  v.VolumeType,
		"bootable":                     strconv.FormatBool(v.Bootable),
		"encrypted":                    v.Encrypted,
		"multiattach":                  v.Multiattach,
		"snapshot_id":                  v.SnapshotID,
		"source_volid":                 v.SourceVolID,
		"user_id":                      v.UserID,
		"os-vol-host-attr:host":        v.Host,
		"os-vol-tenant-attr:tenant_id": v.ProjectID,
		"metadata":                     meta,
		"attachments":                  atts,
		"created_at":                   formatCinderTime(v.CreatedAt),
		"updated_at":                   formatCinderTime(v.UpdatedAt),
	}
	if len(v.VolumeImageMetadata) > 0 {
		resp["volume_image_metadata"] = v.VolumeImageMetadata
	}
	return resp
}
