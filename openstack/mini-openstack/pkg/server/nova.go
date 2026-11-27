package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/omerharuncetin/openstack-ceph-operator/ceph/ceph-client/pkg/cephapi"
)

func (s *Server) registerNovaRoutes() {
	// Flavors
	s.mux.HandleFunc("GET /v2.1/flavors", s.requireAuth(s.handleNovaListFlavors))
	s.mux.HandleFunc("GET /v2.1/flavors/detail", s.requireAuth(s.handleNovaListFlavors))
	s.mux.HandleFunc("GET /v2.1/flavors/{id}", s.requireAuth(s.handleNovaGetFlavor))

	// Servers
	s.mux.HandleFunc("GET /v2.1/servers", s.requireAuth(s.handleNovaListServers))
	s.mux.HandleFunc("GET /v2.1/servers/detail", s.requireAuth(s.handleNovaListServers))
	s.mux.HandleFunc("POST /v2.1/servers", s.requireAuth(s.handleNovaCreateServer))
	s.mux.HandleFunc("GET /v2.1/servers/{id}", s.requireAuth(s.handleNovaGetServer))
	s.mux.HandleFunc("PUT /v2.1/servers/{id}", s.requireAuth(s.handleNovaUpdateServer))
	s.mux.HandleFunc("DELETE /v2.1/servers/{id}", s.requireAuth(s.handleNovaDeleteServer))
	s.mux.HandleFunc("POST /v2.1/servers/{id}/action", s.requireAuth(s.handleNovaServerAction))

	// Volume Attachments
	s.mux.HandleFunc("GET /v2.1/servers/{id}/os-volume_attachments", s.requireAuth(s.handleNovaListVolumeAttachments))
	s.mux.HandleFunc("POST /v2.1/servers/{id}/os-volume_attachments", s.requireAuth(s.handleNovaAttachVolume))
	s.mux.HandleFunc("GET /v2.1/servers/{id}/os-volume_attachments/{volID}", s.requireAuth(s.handleNovaGetVolumeAttachment))
	s.mux.HandleFunc("DELETE /v2.1/servers/{id}/os-volume_attachments/{volID}", s.requireAuth(s.handleNovaDetachVolume))
}

// --- Flavors ---

func (s *Server) handleNovaListFlavors(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.flavors))
	out := make([]*FlavorRecord, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.store.flavors[id])
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"flavors": out})
}

func (s *Server) handleNovaGetFlavor(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	flv := s.findFlavorLocked(id)
	s.store.mu.RUnlock()
	if flv == nil {
		writeAPIError(w, http.StatusNotFound, "flavor not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flavor": flv})
}

func (s *Server) findFlavorLocked(ref string) *FlavorRecord {
	if flv, ok := s.store.flavors[ref]; ok {
		return flv
	}
	for _, flv := range s.store.flavors {
		if flv.Name == ref {
			return flv
		}
	}
	return nil
}

// --- Servers ---

func (s *Server) handleNovaListServers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nameFilter := q.Get("name")
	statusFilter := q.Get("status")
	hostFilter := q.Get("host")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.servers))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		srv := s.store.servers[id]
		if nameFilter != "" && !strings.Contains(srv.Name, nameFilter) {
			continue
		}
		if statusFilter != "" && srv.Status != statusFilter {
			continue
		}
		if hostFilter != "" && srv.Host != hostFilter {
			continue
		}
		out = append(out, s.novaServerResponseLocked(srv))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"servers": out})
}

type novaBlockDeviceMapping struct {
	UUID                string `json:"uuid"`
	SourceType          string `json:"source_type"`
	DestinationType     string `json:"destination_type"`
	BootIndex           int    `json:"boot_index"`
	VolumeSize          int    `json:"volume_size"`
	DeleteOnTermination bool   `json:"delete_on_termination"`
}

type novaNetworkReq struct {
	UUID    string `json:"uuid"`
	Port    string `json:"port"`
	FixedIP string `json:"fixed_ip"`
}

func (s *Server) handleNovaCreateServer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server struct {
			Name               string                   `json:"name"`
			FlavorRef          string                   `json:"flavorRef"`
			ImageRef           string                   `json:"imageRef"`
			AvailabilityZone   string                   `json:"availability_zone"`
			HypervisorHostname string                   `json:"hypervisor_hostname"`
			Metadata           map[string]string        `json:"metadata"`
			SecurityGroups     []map[string]string      `json:"security_groups"`
			Networks           json.RawMessage          `json:"networks"`
			BlockDevices       []novaBlockDeviceMapping `json:"block_device_mapping_v2"`
		} `json:"server"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Server.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "server.name is required")
		return
	}

	tok := tokenFromContext(r.Context())

	// Validate flavor upfront.
	s.store.mu.RLock()
	flv := s.findFlavorLocked(cmp.Or(body.Server.FlavorRef, "1"))
	s.store.mu.RUnlock()
	if flv == nil {
		writeAPIError(w, http.StatusBadRequest, "flavor not found: "+body.Server.FlavorRef)
		return
	}

	// Parse requested networks (supports list of {uuid, port, fixed_ip} or "none"/"auto").
	var netReqs []novaNetworkReq
	if len(body.Server.Networks) > 0 && body.Server.Networks[0] == '[' {
		if err := json.Unmarshal(body.Server.Networks, &netReqs); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid server.networks format")
			return
		}
	}

	// Pre-provision any Cinder Ceph CoW volumes requested via block_device_mapping_v2
	// with source_type=image -> destination_type=volume BEFORE acquiring the store lock
	// (because Ceph API calls perform network I/O).
	type resolvedBDM struct {
		volID               string
		deleteOnTermination bool
		createdHere         bool
	}
	resolvedBDMs := make([]resolvedBDM, 0, len(body.Server.BlockDevices))
	rollbackCreatedVolumes := func() {
		for _, item := range resolvedBDMs {
			if !item.createdHere {
				continue
			}
			s.store.mu.Lock()
			v := s.store.volumes[item.volID]
			delete(s.store.volumes, item.volID)
			s.store.mu.Unlock()
			if v != nil {
				_ = s.ceph.DeleteRBDImage(r.Context(), v.RBDPool, v.RBDImage)
			}
		}
	}

	for _, bdm := range body.Server.BlockDevices {
		switch bdm.SourceType {
		case "volume":
			if bdm.UUID == "" {
				rollbackCreatedVolumes()
				writeAPIError(w, http.StatusBadRequest, "block_device_mapping_v2 volume source requires uuid")
				return
			}
			resolvedBDMs = append(resolvedBDMs, resolvedBDM{
				volID:               bdm.UUID,
				deleteOnTermination: bdm.DeleteOnTermination,
			})
		case "image":
			volSize := cmp.Or(bdm.VolumeSize, flv.Disk, 1)
			vol, code, err := s.provisionCinderVolume(r.Context(), tok.ProjectID, tok.UserID, cinderCreateVolumeParams{
				Name:     body.Server.Name + "-boot",
				SizeGiB:  volSize,
				ImageRef: bdm.UUID,
			})
			if err != nil {
				rollbackCreatedVolumes()
				if code == http.StatusBadGateway {
					writeCephError(w, "nova boot-from-volume ceph clone", err)
					return
				}
				writeAPIError(w, code, err.Error())
				return
			}
			resolvedBDMs = append(resolvedBDMs, resolvedBDM{
				volID:               vol.ID,
				deleteOnTermination: bdm.DeleteOnTermination,
				createdHere:         true,
			})
		}
	}

	// Acquire store lock to schedule compute host, attach volumes, and bind OVN ports atomically.
	s.store.mu.Lock()
	host, err := s.store.nextComputeHostLocked(body.Server.HypervisorHostname)
	if err != nil {
		s.store.mu.Unlock()
		rollbackCreatedVolumes()
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate all referenced existing volumes are present and available.
	for _, item := range resolvedBDMs {
		vol, exists := s.store.volumes[item.volID]
		if !exists {
			s.store.mu.Unlock()
			rollbackCreatedVolumes()
			writeAPIError(w, http.StatusNotFound, "boot volume not found: "+item.volID)
			return
		}
		if vol.Status != "available" {
			s.store.mu.Unlock()
			rollbackCreatedVolumes()
			writeAPIError(w, http.StatusConflict, fmt.Sprintf("volume %s is in status %q, expected available", item.volID, vol.Status))
			return
		}
	}

	// Resolve requested security group names -> IDs.
	sgNames := make([]string, 0, len(body.Server.SecurityGroups))
	sgIDs := make([]string, 0, len(body.Server.SecurityGroups))
	for _, sgRef := range body.Server.SecurityGroups {
		name := sgRef["name"]
		if name == "" {
			continue
		}
		sgNames = append(sgNames, name)
		for _, sg := range s.store.securityGroups {
			if sg.Name == name || sg.ID == name {
				sgIDs = append(sgIDs, sg.ID)
				break
			}
		}
	}

	serverID := newUUID()
	now := time.Now().UTC()

	// Bind or create Neutron ports on the scheduled compute host (OVN chassis).
	portIDs := make([]string, 0, len(netReqs))
	createdPortIDs := make([]string, 0)
	cleanupPortsLocked := func() {
		for _, pid := range createdPortIDs {
			delete(s.store.ports, pid)
		}
	}

	for _, nr := range netReqs {
		if nr.Port != "" {
			p, exists := s.store.ports[nr.Port]
			if !exists {
				cleanupPortsLocked()
				s.store.mu.Unlock()
				rollbackCreatedVolumes()
				writeAPIError(w, http.StatusNotFound, "Neutron port not found: "+nr.Port)
				return
			}
			if p.DeviceID != "" {
				cleanupPortsLocked()
				s.store.mu.Unlock()
				rollbackCreatedVolumes()
				writeAPIError(w, http.StatusConflict, fmt.Sprintf("port %s is already bound to %s", p.ID, p.DeviceID))
				return
			}
			p.DeviceID = serverID
			p.DeviceOwner = "compute:nova"
			p.HostID = host
			p.VIFType = "ovs"
			p.VIFDetails = map[string]any{
				"port_filter":     true,
				"ovs_hybrid_plug": false,
				"connectivity":    "l2",
				"datapath_type":   "system",
				"ovn_chassis":     host,
			}
			p.UpdatedAt = now
			portIDs = append(portIDs, p.ID)
		} else if nr.UUID != "" {
			if _, exists := s.store.networks[nr.UUID]; !exists {
				cleanupPortsLocked()
				s.store.mu.Unlock()
				rollbackCreatedVolumes()
				writeAPIError(w, http.StatusNotFound, "Neutron network not found: "+nr.UUID)
				return
			}
			var fixedIPs []FixedIP
			if nr.FixedIP != "" {
				fixedIPs = []FixedIP{{IPAddress: nr.FixedIP}}
			}
			p, err := s.createPortLocked(portCreateParams{
				Name:           body.Server.Name + "-port",
				NetworkID:      nr.UUID,
				ProjectID:      tok.ProjectID,
				AdminStateUp:   true,
				DeviceID:       serverID,
				DeviceOwner:    "compute:nova",
				FixedIPs:       fixedIPs,
				SecurityGroups: sgIDs,
				HostID:         host,
				VNICType:       "normal",
				ManagedByNova:  true,
			})
			if err != nil {
				cleanupPortsLocked()
				s.store.mu.Unlock()
				rollbackCreatedVolumes()
				writeAPIError(w, http.StatusConflict, err.Error())
				return
			}
			createdPortIDs = append(createdPortIDs, p.ID)
			portIDs = append(portIDs, p.ID)
		}
	}

	// Attach resolved Cinder volumes to the server.
	attachedRefs := make([]ServerVolumeRef, 0, len(resolvedBDMs))
	for i, item := range resolvedBDMs {
		device := fmt.Sprintf("/dev/vd%c", 'a'+i)
		vol := s.store.volumes[item.volID]
		vol.Status = "in-use"
		vol.UpdatedAt = now
		vol.Attachments = append(vol.Attachments, VolumeAttachmentRecord{
			ID:           vol.ID,
			AttachmentID: newUUID(),
			VolumeID:     vol.ID,
			ServerID:     serverID,
			HostName:     host,
			Device:       device,
			AttachedAt:   now,
		})
		attachedRefs = append(attachedRefs, ServerVolumeRef{
			VolumeID:            vol.ID,
			Device:              device,
			DeleteOnTermination: item.deleteOnTermination,
		})
	}

	srv := &ServerRecord{
		ID:               serverID,
		Name:             body.Server.Name,
		ProjectID:        tok.ProjectID,
		UserID:           tok.UserID,
		FlavorID:         flv.ID,
		ImageID:          body.Server.ImageRef,
		Status:           "ACTIVE",
		Host:             host,
		InstanceName:     "instance-" + serverID[:8],
		AvailabilityZone: cmp.Or(body.Server.AvailabilityZone, DefaultAZ),
		PortIDs:          portIDs,
		Volumes:          attachedRefs,
		SecurityGroups:   sgNames,
		Metadata:         maps.Clone(body.Server.Metadata),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	s.store.servers[serverID] = srv
	resp := s.novaServerResponseLocked(srv)
	s.store.mu.Unlock()

	s.logger.Info("nova server scheduled",
		"server_id", serverID,
		"name", srv.Name,
		"host", host,
		"ports", len(portIDs),
		"volumes", len(attachedRefs),
	)

	writeJSON(w, http.StatusAccepted, map[string]any{"server": resp})
}

func (s *Server) handleNovaGetServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	srv, ok := s.store.servers[id]
	var resp map[string]any
	if ok {
		resp = s.novaServerResponseLocked(srv)
	}
	s.store.mu.RUnlock()

	if !ok {
		writeAPIError(w, http.StatusNotFound, "server not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": resp})
}

func (s *Server) handleNovaUpdateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Server struct {
			Name *string `json:"name"`
		} `json:"server"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Nova server update payload")
		return
	}

	s.store.mu.Lock()
	srv, ok := s.store.servers[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "server not found: "+id)
		return
	}
	if body.Server.Name != nil {
		srv.Name = *body.Server.Name
	}
	srv.UpdatedAt = time.Now().UTC()
	resp := s.novaServerResponseLocked(srv)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"server": resp})
}

func (s *Server) handleNovaDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	type rbdToDelete struct {
		pool  string
		image string
	}
	var toDeleteInCeph []rbdToDelete

	s.store.mu.Lock()
	srv, ok := s.store.servers[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "server not found: "+id)
		return
	}

	// Detach Cinder volumes (and delete those flagged DeleteOnTermination).
	for _, ref := range srv.Volumes {
		vol, exists := s.store.volumes[ref.VolumeID]
		if !exists {
			continue
		}
		vol.Attachments = slices.DeleteFunc(vol.Attachments, func(a VolumeAttachmentRecord) bool {
			return a.ServerID == srv.ID
		})
		if len(vol.Attachments) == 0 {
			vol.Status = "available"
		}
		vol.UpdatedAt = time.Now().UTC()
		if ref.DeleteOnTermination && len(vol.Attachments) == 0 {
			toDeleteInCeph = append(toDeleteInCeph, rbdToDelete{pool: vol.RBDPool, image: vol.RBDImage})
			delete(s.store.volumes, vol.ID)
		}
	}

	// Release or delete attached Neutron ports.
	for _, pid := range srv.PortIDs {
		p, exists := s.store.ports[pid]
		if !exists {
			continue
		}
		if p.ManagedByNova {
			delete(s.store.ports, pid)
		} else {
			p.DeviceID = ""
			p.DeviceOwner = ""
			p.HostID = ""
			p.VIFType = "unbound"
			p.VIFDetails = map[string]any{}
			p.UpdatedAt = time.Now().UTC()
		}
	}

	delete(s.store.servers, id)
	s.store.mu.Unlock()

	for _, item := range toDeleteInCeph {
		if err := s.ceph.DeleteRBDImage(r.Context(), item.pool, item.image); err != nil && !cephapi.IsNotFound(err) {
			s.logger.Warn("failed cleaning up delete_on_termination rbd image",
				"pool", item.pool, "image", item.image, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- Volume Attachments ---

func (s *Server) handleNovaListVolumeAttachments(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	s.store.mu.RLock()
	srv, ok := s.store.servers[serverID]
	if !ok {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusNotFound, "server not found: "+serverID)
		return
	}
	atts := make([]map[string]any, 0, len(srv.Volumes))
	for _, ref := range srv.Volumes {
		atts = append(atts, novaVolumeAttachmentResponse(serverID, ref))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"volumeAttachments": atts})
}

func (s *Server) handleNovaAttachVolume(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	var body struct {
		VolumeAttachment struct {
			VolumeID            string `json:"volumeId"`
			Device              string `json:"device"`
			DeleteOnTermination bool   `json:"delete_on_termination"`
		} `json:"volumeAttachment"`
	}
	if err := decodeJSON(r, &body); err != nil || body.VolumeAttachment.VolumeID == "" {
		writeAPIError(w, http.StatusBadRequest, "volumeAttachment.volumeId is required")
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	srv, ok := s.store.servers[serverID]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "server not found: "+serverID)
		return
	}
	vol, ok := s.store.volumes[body.VolumeAttachment.VolumeID]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "volume not found: "+body.VolumeAttachment.VolumeID)
		return
	}
	if vol.Status != "available" {
		writeAPIError(w, http.StatusConflict, fmt.Sprintf("volume %s is in status %q, expected available", vol.ID, vol.Status))
		return
	}

	device := body.VolumeAttachment.Device
	if device == "" {
		device = fmt.Sprintf("/dev/vd%c", 'a'+len(srv.Volumes))
	}
	now := time.Now().UTC()
	vol.Status = "in-use"
	vol.UpdatedAt = now
	vol.Attachments = append(vol.Attachments, VolumeAttachmentRecord{
		ID:           vol.ID,
		AttachmentID: newUUID(),
		VolumeID:     vol.ID,
		ServerID:     srv.ID,
		HostName:     srv.Host,
		Device:       device,
		AttachedAt:   now,
	})

	ref := ServerVolumeRef{
		VolumeID:            vol.ID,
		Device:              device,
		DeleteOnTermination: body.VolumeAttachment.DeleteOnTermination,
	}
	srv.Volumes = append(srv.Volumes, ref)
	srv.UpdatedAt = now

	writeJSON(w, http.StatusOK, map[string]any{
		"volumeAttachment": novaVolumeAttachmentResponse(srv.ID, ref),
	})
}

func (s *Server) handleNovaGetVolumeAttachment(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	volID := r.PathValue("volID")

	s.store.mu.RLock()
	srv, ok := s.store.servers[serverID]
	if !ok {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusNotFound, "server not found: "+serverID)
		return
	}
	idx := slices.IndexFunc(srv.Volumes, func(ref ServerVolumeRef) bool { return ref.VolumeID == volID })
	if idx < 0 {
		s.store.mu.RUnlock()
		writeAPIError(w, http.StatusNotFound, "volume attachment not found: "+volID)
		return
	}
	resp := novaVolumeAttachmentResponse(serverID, srv.Volumes[idx])
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"volumeAttachment": resp})
}

func (s *Server) handleNovaDetachVolume(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	volID := r.PathValue("volID")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	srv, ok := s.store.servers[serverID]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "server not found: "+serverID)
		return
	}
	idx := slices.IndexFunc(srv.Volumes, func(ref ServerVolumeRef) bool { return ref.VolumeID == volID })
	if idx < 0 {
		writeAPIError(w, http.StatusNotFound, "volume attachment not found: "+volID)
		return
	}
	srv.Volumes = slices.Delete(srv.Volumes, idx, idx+1)
	srv.UpdatedAt = time.Now().UTC()

	if vol, exists := s.store.volumes[volID]; exists {
		vol.Attachments = slices.DeleteFunc(vol.Attachments, func(a VolumeAttachmentRecord) bool {
			return a.ServerID == serverID
		})
		if len(vol.Attachments) == 0 {
			vol.Status = "available"
		}
		vol.UpdatedAt = time.Now().UTC()
	}

	w.WriteHeader(http.StatusAccepted)
}

// --- Server Actions ---

func (s *Server) handleNovaServerAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var actions map[string]json.RawMessage
	if err := decodeJSON(r, &actions); err != nil || len(actions) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid Nova server action payload")
		return
	}

	if rawMigrate, ok := actions["os-migrateLive"]; ok {
		var req struct {
			Host           *string `json:"host"`
			BlockMigration *bool   `json:"block_migration"`
			DiskOverCommit *bool   `json:"disk_over_commit"`
		}
		if err := json.Unmarshal(rawMigrate, &req); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid os-migrateLive payload")
			return
		}

		s.store.mu.Lock()
		defer s.store.mu.Unlock()

		srv, exists := s.store.servers[id]
		if !exists {
			writeAPIError(w, http.StatusNotFound, "server not found: "+id)
			return
		}
		if srv.Status != "ACTIVE" {
			writeAPIError(w, http.StatusConflict, fmt.Sprintf("server %s must be ACTIVE to live migrate (status=%s)", id, srv.Status))
			return
		}

		var targetHost string
		if req.Host != nil && *req.Host != "" {
			if !slices.Contains(s.store.computeHosts, *req.Host) {
				writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("target compute host %q does not exist", *req.Host))
				return
			}
			if *req.Host == srv.Host {
				writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("server is already running on host %q", srv.Host))
				return
			}
			targetHost = *req.Host
		} else {
			for _, h := range s.store.computeHosts {
				if h != srv.Host {
					targetHost = h
					break
				}
			}
			if targetHost == "" {
				writeAPIError(w, http.StatusConflict, "no alternate compute host available for live migration")
				return
			}
		}

		// Reject block_migration=true on shared Ceph RBD volumes.
		hasSharedCeph := false
		for _, ref := range srv.Volumes {
			if vol, exists := s.store.volumes[ref.VolumeID]; exists && vol.RBDPool != "" {
				hasSharedCeph = true
				break
			}
		}
		if req.BlockMigration != nil && *req.BlockMigration && hasSharedCeph {
			writeAPIError(w, http.StatusBadRequest, "block_migration=true is invalid for shared Ceph RBD volumes; set block_migration=false for zero-copy live migration")
			return
		}

		prevHost := srv.Host
		now := time.Now().UTC()

		// 1. Re-point Nova compute host (zero disk copy over shared Ceph RBD).
		srv.Host = targetHost
		srv.UpdatedAt = now

		// 2. Update Cinder volume attachment host metadata (RBD stays untouched in Ceph).
		for _, ref := range srv.Volumes {
			if vol, exists := s.store.volumes[ref.VolumeID]; exists {
				for i := range vol.Attachments {
					if vol.Attachments[i].ServerID == srv.ID {
						vol.Attachments[i].HostName = targetHost
					}
				}
				vol.UpdatedAt = now
			}
		}

		// 3. Rebind Neutron OVN port chassis (binding:host_id -> targetHost).
		for _, pid := range srv.PortIDs {
			if p, exists := s.store.ports[pid]; exists {
				p.HostID = targetHost
				p.VIFType = "ovs"
				p.VIFDetails = map[string]any{
					"port_filter":     true,
					"ovs_hybrid_plug": false,
					"connectivity":    "l2",
					"datapath_type":   "system",
					"ovn_chassis":     targetHost,
				}
				p.UpdatedAt = now
			}
		}

		s.logger.Info("nova zero-copy live migration completed",
			"server_id", srv.ID,
			"from_host", prevHost,
			"to_host", targetHost,
			"shared_ceph_rbd", hasSharedCeph,
			"ovn_ports_rebound", len(srv.PortIDs),
		)

		w.WriteHeader(http.StatusAccepted)
		return
	}

	if _, ok := actions["os-stop"]; ok {
		s.store.mu.Lock()
		srv, exists := s.store.servers[id]
		if !exists {
			s.store.mu.Unlock()
			writeAPIError(w, http.StatusNotFound, "server not found: "+id)
			return
		}
		srv.Status = "SHUTOFF"
		srv.UpdatedAt = time.Now().UTC()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if _, ok := actions["os-start"]; ok {
		s.store.mu.Lock()
		srv, exists := s.store.servers[id]
		if !exists {
			s.store.mu.Unlock()
			writeAPIError(w, http.StatusNotFound, "server not found: "+id)
			return
		}
		srv.Status = "ACTIVE"
		srv.UpdatedAt = time.Now().UTC()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if _, ok := actions["reboot"]; ok {
		s.store.mu.Lock()
		srv, exists := s.store.servers[id]
		if !exists {
			s.store.mu.Unlock()
			writeAPIError(w, http.StatusNotFound, "server not found: "+id)
			return
		}
		srv.Status = "ACTIVE"
		srv.UpdatedAt = time.Now().UTC()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	writeAPIError(w, http.StatusBadRequest, "unsupported Nova server action")
}

func (s *Server) novaServerResponseLocked(srv *ServerRecord) map[string]any {
	addresses := make(map[string][]map[string]any)
	for _, pid := range srv.PortIDs {
		p, ok := s.store.ports[pid]
		if !ok {
			continue
		}
		netName := p.NetworkID
		if n, exists := s.store.networks[p.NetworkID]; exists && n.Name != "" {
			netName = n.Name
		}
		for _, fip := range p.FixedIPs {
			addresses[netName] = append(addresses[netName], map[string]any{
				"version":                 4,
				"addr":                    fip.IPAddress,
				"OS-EXT-IPS:type":         "fixed",
				"OS-EXT-IPS-MAC:mac_addr": p.MACAddress,
			})
		}
	}

	attached := make([]map[string]string, 0, len(srv.Volumes))
	for _, ref := range srv.Volumes {
		attached = append(attached, map[string]string{"id": ref.VolumeID})
	}

	sgs := make([]map[string]any, 0, len(srv.SecurityGroups))
	for _, name := range srv.SecurityGroups {
		sgs = append(sgs, map[string]any{"name": name})
	}

	meta := srv.Metadata
	if meta == nil {
		meta = map[string]string{}
	}

	var imageField any = ""
	if srv.ImageID != "" {
		imageField = map[string]any{"id": srv.ImageID}
	}

	return map[string]any{
		"id":                                   srv.ID,
		"name":                                 srv.Name,
		"tenant_id":                            srv.ProjectID,
		"user_id":                              srv.UserID,
		"status":                               srv.Status,
		"progress":                             100,
		"hostid":                               srv.Host,
		"OS-EXT-SRV-ATTR:host":                 srv.Host,
		"OS-EXT-SRV-ATTR:hypervisor_hostname":  srv.Host,
		"OS-EXT-SRV-ATTR:instance_name":        srv.InstanceName,
		"OS-EXT-AZ:availability_zone":          srv.AvailabilityZone,
		"flavor":                               map[string]any{"id": srv.FlavorID},
		"image":                                imageField,
		"addresses":                            addresses,
		"security_groups":                      sgs,
		"os-extended-volumes:volumes_attached": attached,
		"metadata":                             meta,
		"created":                              srv.CreatedAt.Format(time.RFC3339),
		"updated":                              srv.UpdatedAt.Format(time.RFC3339),
	}
}

func novaVolumeAttachmentResponse(serverID string, ref ServerVolumeRef) map[string]any {
	return map[string]any{
		"id":                    ref.VolumeID,
		"volumeId":              ref.VolumeID,
		"serverId":              serverID,
		"device":                ref.Device,
		"delete_on_termination": ref.DeleteOnTermination,
	}
}
