package server

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"
)

func (s *Server) registerNeutronRoutes() {
	// Networks
	s.mux.HandleFunc("GET /v2.0/networks", s.requireAuth(s.handleNeutronListNetworks))
	s.mux.HandleFunc("POST /v2.0/networks", s.requireAuth(s.handleNeutronCreateNetwork))
	s.mux.HandleFunc("GET /v2.0/networks/{id}", s.requireAuth(s.handleNeutronGetNetwork))
	s.mux.HandleFunc("PUT /v2.0/networks/{id}", s.requireAuth(s.handleNeutronUpdateNetwork))
	s.mux.HandleFunc("DELETE /v2.0/networks/{id}", s.requireAuth(s.handleNeutronDeleteNetwork))

	// Subnets
	s.mux.HandleFunc("GET /v2.0/subnets", s.requireAuth(s.handleNeutronListSubnets))
	s.mux.HandleFunc("POST /v2.0/subnets", s.requireAuth(s.handleNeutronCreateSubnet))
	s.mux.HandleFunc("GET /v2.0/subnets/{id}", s.requireAuth(s.handleNeutronGetSubnet))
	s.mux.HandleFunc("PUT /v2.0/subnets/{id}", s.requireAuth(s.handleNeutronUpdateSubnet))
	s.mux.HandleFunc("DELETE /v2.0/subnets/{id}", s.requireAuth(s.handleNeutronDeleteSubnet))

	// Routers & L3 interfaces
	s.mux.HandleFunc("GET /v2.0/routers", s.requireAuth(s.handleNeutronListRouters))
	s.mux.HandleFunc("POST /v2.0/routers", s.requireAuth(s.handleNeutronCreateRouter))
	s.mux.HandleFunc("GET /v2.0/routers/{id}", s.requireAuth(s.handleNeutronGetRouter))
	s.mux.HandleFunc("PUT /v2.0/routers/{id}", s.requireAuth(s.handleNeutronUpdateRouter))
	s.mux.HandleFunc("DELETE /v2.0/routers/{id}", s.requireAuth(s.handleNeutronDeleteRouter))
	s.mux.HandleFunc("PUT /v2.0/routers/{id}/add_router_interface", s.requireAuth(s.handleNeutronAddRouterInterface))
	s.mux.HandleFunc("PUT /v2.0/routers/{id}/remove_router_interface", s.requireAuth(s.handleNeutronRemoveRouterInterface))

	// Security Groups & Rules
	s.mux.HandleFunc("GET /v2.0/security-groups", s.requireAuth(s.handleNeutronListSecGroups))
	s.mux.HandleFunc("POST /v2.0/security-groups", s.requireAuth(s.handleNeutronCreateSecGroup))
	s.mux.HandleFunc("GET /v2.0/security-groups/{id}", s.requireAuth(s.handleNeutronGetSecGroup))
	s.mux.HandleFunc("PUT /v2.0/security-groups/{id}", s.requireAuth(s.handleNeutronUpdateSecGroup))
	s.mux.HandleFunc("DELETE /v2.0/security-groups/{id}", s.requireAuth(s.handleNeutronDeleteSecGroup))

	s.mux.HandleFunc("GET /v2.0/security-group-rules", s.requireAuth(s.handleNeutronListSecGroupRules))
	s.mux.HandleFunc("POST /v2.0/security-group-rules", s.requireAuth(s.handleNeutronCreateSecGroupRule))
	s.mux.HandleFunc("GET /v2.0/security-group-rules/{id}", s.requireAuth(s.handleNeutronGetSecGroupRule))
	s.mux.HandleFunc("DELETE /v2.0/security-group-rules/{id}", s.requireAuth(s.handleNeutronDeleteSecGroupRule))

	// Ports (with OVN binding:host_id / binding:vif_type)
	s.mux.HandleFunc("GET /v2.0/ports", s.requireAuth(s.handleNeutronListPorts))
	s.mux.HandleFunc("POST /v2.0/ports", s.requireAuth(s.handleNeutronCreatePort))
	s.mux.HandleFunc("GET /v2.0/ports/{id}", s.requireAuth(s.handleNeutronGetPort))
	s.mux.HandleFunc("PUT /v2.0/ports/{id}", s.requireAuth(s.handleNeutronUpdatePort))
	s.mux.HandleFunc("DELETE /v2.0/ports/{id}", s.requireAuth(s.handleNeutronDeletePort))
}

// --- Networks ---

func (s *Server) handleNeutronListNetworks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nameFilter := q.Get("name")
	idFilter := q.Get("id")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.networks))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		n := s.store.networks[id]
		if nameFilter != "" && n.Name != nameFilter {
			continue
		}
		if idFilter != "" && n.ID != idFilter {
			continue
		}
		out = append(out, s.neutronNetworkResponseLocked(n))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"networks": out})
}

func (s *Server) handleNeutronCreateNetwork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Network struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			AdminUp     *bool  `json:"admin_state_up"`
			Shared      *bool  `json:"shared"`
			External    *bool  `json:"router:external"`
			NetworkType string `json:"provider:network_type"`
			MTU         int    `json:"mtu"`
			ProjectID   string `json:"project_id"`
			TenantID    string `json:"tenant_id"`
		} `json:"network"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron network payload")
		return
	}

	tok := tokenFromContext(r.Context())
	id := newUUID()
	now := time.Now().UTC()
	adminUp := true
	if body.Network.AdminUp != nil {
		adminUp = *body.Network.AdminUp
	}

	rec := &NetworkRecord{
		ID:           id,
		Name:         body.Network.Name,
		Description:  body.Network.Description,
		ProjectID:    cmp.Or(body.Network.ProjectID, body.Network.TenantID, tok.ProjectID),
		AdminStateUp: adminUp,
		Shared:       body.Network.Shared != nil && *body.Network.Shared,
		External:     body.Network.External != nil && *body.Network.External,
		Status:       "ACTIVE",
		NetworkType:  cmp.Or(body.Network.NetworkType, "geneve"),
		MTU:          cmp.Or(body.Network.MTU, 1442),
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	s.store.mu.Lock()
	s.store.networks[id] = rec
	resp := s.neutronNetworkResponseLocked(rec)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{"network": resp})
}

func (s *Server) handleNeutronGetNetwork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	n, ok := s.store.networks[id]
	var resp map[string]any
	if ok {
		resp = s.neutronNetworkResponseLocked(n)
	}
	s.store.mu.RUnlock()

	if !ok {
		writeAPIError(w, http.StatusNotFound, "network not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"network": resp})
}

func (s *Server) handleNeutronUpdateNetwork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Network struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			AdminUp     *bool   `json:"admin_state_up"`
			Shared      *bool   `json:"shared"`
		} `json:"network"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron network update payload")
		return
	}

	s.store.mu.Lock()
	n, ok := s.store.networks[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "network not found: "+id)
		return
	}
	if body.Network.Name != nil {
		n.Name = *body.Network.Name
	}
	if body.Network.Description != nil {
		n.Description = *body.Network.Description
	}
	if body.Network.AdminUp != nil {
		n.AdminStateUp = *body.Network.AdminUp
	}
	if body.Network.Shared != nil {
		n.Shared = *body.Network.Shared
	}
	n.UpdatedAt = time.Now().UTC()
	resp := s.neutronNetworkResponseLocked(n)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"network": resp})
}

func (s *Server) handleNeutronDeleteNetwork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	if _, ok := s.store.networks[id]; !ok {
		writeAPIError(w, http.StatusNotFound, "network not found: "+id)
		return
	}
	for _, p := range s.store.ports {
		if p.NetworkID == id {
			writeAPIError(w, http.StatusConflict, fmt.Sprintf("network %s still has active port %s", id, p.ID))
			return
		}
	}
	// Delete empty subnets attached to this network.
	maps.DeleteFunc(s.store.subnets, func(_ string, sub *SubnetRecord) bool {
		return sub.NetworkID == id
	})
	delete(s.store.networks, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) neutronNetworkResponseLocked(n *NetworkRecord) map[string]any {
	subnetIDs := make([]string, 0)
	for _, sub := range s.store.subnets {
		if sub.NetworkID == n.ID {
			subnetIDs = append(subnetIDs, sub.ID)
		}
	}
	slices.Sort(subnetIDs)

	return map[string]any{
		"id":                    n.ID,
		"name":                  n.Name,
		"description":           n.Description,
		"status":                n.Status,
		"admin_state_up":        n.AdminStateUp,
		"shared":                n.Shared,
		"router:external":       n.External,
		"provider:network_type": n.NetworkType,
		"mtu":                   n.MTU,
		"tenant_id":             n.ProjectID,
		"project_id":            n.ProjectID,
		"subnets":               subnetIDs,
		"tags":                  []string{},
		"created_at":            n.CreatedAt.Format(time.RFC3339),
		"updated_at":            n.UpdatedAt.Format(time.RFC3339),
		"revision_number":       1,
	}
}

// --- Subnets ---

func (s *Server) handleNeutronListSubnets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nameFilter := q.Get("name")
	netFilter := q.Get("network_id")
	cidrFilter := q.Get("cidr")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.subnets))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		sub := s.store.subnets[id]
		if nameFilter != "" && sub.Name != nameFilter {
			continue
		}
		if netFilter != "" && sub.NetworkID != netFilter {
			continue
		}
		if cidrFilter != "" && sub.CIDR != cidrFilter {
			continue
		}
		out = append(out, neutronSubnetResponse(sub))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"subnets": out})
}

func (s *Server) handleNeutronCreateSubnet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Subnet struct {
			Name            string           `json:"name"`
			Description     string           `json:"description"`
			NetworkID       string           `json:"network_id"`
			IPVersion       int              `json:"ip_version"`
			CIDR            string           `json:"cidr"`
			GatewayIP       string           `json:"gateway_ip"`
			EnableDHCP      *bool            `json:"enable_dhcp"`
			AllocationPools []AllocationPool `json:"allocation_pools"`
			DNSNameservers  []string         `json:"dns_nameservers"`
			ProjectID       string           `json:"project_id"`
			TenantID        string           `json:"tenant_id"`
		} `json:"subnet"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron subnet payload")
		return
	}
	if body.Subnet.NetworkID == "" || body.Subnet.CIDR == "" {
		writeAPIError(w, http.StatusBadRequest, "network_id and cidr are required")
		return
	}

	gw, pools, err := computeSubnetDefaults(body.Subnet.CIDR, body.Subnet.GatewayIP, body.Subnet.AllocationPools)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	tok := tokenFromContext(r.Context())
	enableDHCP := true
	if body.Subnet.EnableDHCP != nil {
		enableDHCP = *body.Subnet.EnableDHCP
	}
	now := time.Now().UTC()
	rec := &SubnetRecord{
		ID:              newUUID(),
		Name:            body.Subnet.Name,
		Description:     body.Subnet.Description,
		NetworkID:       body.Subnet.NetworkID,
		ProjectID:       cmp.Or(body.Subnet.ProjectID, body.Subnet.TenantID, tok.ProjectID),
		IPVersion:       cmp.Or(body.Subnet.IPVersion, 4),
		CIDR:            body.Subnet.CIDR,
		GatewayIP:       gw,
		EnableDHCP:      enableDHCP,
		AllocationPools: pools,
		DNSNameservers:  slices.Clone(body.Subnet.DNSNameservers),
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	s.store.mu.Lock()
	if _, ok := s.store.networks[rec.NetworkID]; !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "network not found: "+rec.NetworkID)
		return
	}
	s.store.subnets[rec.ID] = rec
	s.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{"subnet": neutronSubnetResponse(rec)})
}

func (s *Server) handleNeutronGetSubnet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	sub, ok := s.store.subnets[id]
	s.store.mu.RUnlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "subnet not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subnet": neutronSubnetResponse(sub)})
}

func (s *Server) handleNeutronUpdateSubnet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Subnet struct {
			Name           *string   `json:"name"`
			Description    *string   `json:"description"`
			GatewayIP      *string   `json:"gateway_ip"`
			EnableDHCP     *bool     `json:"enable_dhcp"`
			DNSNameservers *[]string `json:"dns_nameservers"`
		} `json:"subnet"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron subnet update payload")
		return
	}

	s.store.mu.Lock()
	sub, ok := s.store.subnets[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "subnet not found: "+id)
		return
	}
	if body.Subnet.Name != nil {
		sub.Name = *body.Subnet.Name
	}
	if body.Subnet.Description != nil {
		sub.Description = *body.Subnet.Description
	}
	if body.Subnet.GatewayIP != nil {
		sub.GatewayIP = *body.Subnet.GatewayIP
	}
	if body.Subnet.EnableDHCP != nil {
		sub.EnableDHCP = *body.Subnet.EnableDHCP
	}
	if body.Subnet.DNSNameservers != nil {
		sub.DNSNameservers = slices.Clone(*body.Subnet.DNSNameservers)
	}
	sub.UpdatedAt = time.Now().UTC()
	resp := neutronSubnetResponse(sub)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"subnet": resp})
}

func (s *Server) handleNeutronDeleteSubnet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	if _, ok := s.store.subnets[id]; !ok {
		writeAPIError(w, http.StatusNotFound, "subnet not found: "+id)
		return
	}
	for _, p := range s.store.ports {
		for _, fip := range p.FixedIPs {
			if fip.SubnetID == id {
				writeAPIError(w, http.StatusConflict, fmt.Sprintf("subnet %s is still in use by port %s", id, p.ID))
				return
			}
		}
	}
	delete(s.store.subnets, id)
	w.WriteHeader(http.StatusNoContent)
}

func neutronSubnetResponse(sub *SubnetRecord) map[string]any {
	dns := sub.DNSNameservers
	if dns == nil {
		dns = []string{}
	}
	pools := sub.AllocationPools
	if pools == nil {
		pools = []AllocationPool{}
	}
	return map[string]any{
		"id":               sub.ID,
		"name":             sub.Name,
		"description":      sub.Description,
		"network_id":       sub.NetworkID,
		"tenant_id":        sub.ProjectID,
		"project_id":       sub.ProjectID,
		"ip_version":       sub.IPVersion,
		"cidr":             sub.CIDR,
		"gateway_ip":       sub.GatewayIP,
		"enable_dhcp":      sub.EnableDHCP,
		"allocation_pools": pools,
		"dns_nameservers":  dns,
		"host_routes":      []any{},
		"tags":             []string{},
		"created_at":       sub.CreatedAt.Format(time.RFC3339),
		"updated_at":       sub.UpdatedAt.Format(time.RFC3339),
		"revision_number":  1,
	}
}

// --- Routers ---

func (s *Server) handleNeutronListRouters(w http.ResponseWriter, r *http.Request) {
	nameFilter := r.URL.Query().Get("name")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.routers))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		rt := s.store.routers[id]
		if nameFilter != "" && rt.Name != nameFilter {
			continue
		}
		out = append(out, neutronRouterResponse(rt))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"routers": out})
}

func (s *Server) handleNeutronCreateRouter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Router struct {
			Name        string               `json:"name"`
			Description string               `json:"description"`
			AdminUp     *bool                `json:"admin_state_up"`
			Distributed *bool                `json:"distributed"`
			GatewayInfo *ExternalGatewayInfo `json:"external_gateway_info"`
			ProjectID   string               `json:"project_id"`
			TenantID    string               `json:"tenant_id"`
		} `json:"router"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron router payload")
		return
	}

	tok := tokenFromContext(r.Context())
	adminUp := true
	if body.Router.AdminUp != nil {
		adminUp = *body.Router.AdminUp
	}
	now := time.Now().UTC()
	rt := &RouterRecord{
		ID:                  newUUID(),
		Name:                body.Router.Name,
		Description:         body.Router.Description,
		ProjectID:           cmp.Or(body.Router.ProjectID, body.Router.TenantID, tok.ProjectID),
		AdminStateUp:        adminUp,
		Status:              "ACTIVE",
		Distributed:         body.Router.Distributed != nil && *body.Router.Distributed,
		ExternalGatewayInfo: body.Router.GatewayInfo,
		SubnetPorts:         make(map[string]string),
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	s.store.mu.Lock()
	s.store.routers[rt.ID] = rt
	s.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{"router": neutronRouterResponse(rt)})
}

func (s *Server) handleNeutronGetRouter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	rt, ok := s.store.routers[id]
	s.store.mu.RUnlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "router not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"router": neutronRouterResponse(rt)})
}

func (s *Server) handleNeutronUpdateRouter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Router struct {
			Name        *string              `json:"name"`
			Description *string              `json:"description"`
			AdminUp     *bool                `json:"admin_state_up"`
			GatewayInfo *ExternalGatewayInfo `json:"external_gateway_info"`
		} `json:"router"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron router update payload")
		return
	}

	s.store.mu.Lock()
	rt, ok := s.store.routers[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "router not found: "+id)
		return
	}
	if body.Router.Name != nil {
		rt.Name = *body.Router.Name
	}
	if body.Router.Description != nil {
		rt.Description = *body.Router.Description
	}
	if body.Router.AdminUp != nil {
		rt.AdminStateUp = *body.Router.AdminUp
	}
	if body.Router.GatewayInfo != nil {
		rt.ExternalGatewayInfo = body.Router.GatewayInfo
	}
	rt.UpdatedAt = time.Now().UTC()
	resp := neutronRouterResponse(rt)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"router": resp})
}

func (s *Server) handleNeutronDeleteRouter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	rt, ok := s.store.routers[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "router not found: "+id)
		return
	}
	if len(rt.SubnetPorts) > 0 {
		writeAPIError(w, http.StatusConflict, "router still has attached subnet interfaces; remove them first")
		return
	}
	delete(s.store.routers, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNeutronAddRouterInterface(w http.ResponseWriter, r *http.Request) {
	routerID := r.PathValue("id")
	var req struct {
		SubnetID string `json:"subnet_id"`
		PortID   string `json:"port_id"`
	}
	if err := decodeJSON(r, &req); err != nil || (req.SubnetID == "" && req.PortID == "") {
		writeAPIError(w, http.StatusBadRequest, "subnet_id or port_id is required")
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	rt, ok := s.store.routers[routerID]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "router not found: "+routerID)
		return
	}

	var portID, subnetID string
	if req.SubnetID != "" {
		sub, exists := s.store.subnets[req.SubnetID]
		if !exists {
			writeAPIError(w, http.StatusNotFound, "subnet not found: "+req.SubnetID)
			return
		}
		if _, dup := rt.SubnetPorts[req.SubnetID]; dup {
			writeAPIError(w, http.StatusConflict, "subnet already attached to router")
			return
		}
		ip := sub.GatewayIP
		if ip == "" {
			var err error
			ip, err = s.store.allocateIPFromSubnetLocked(sub, "")
			if err != nil {
				writeAPIError(w, http.StatusConflict, err.Error())
				return
			}
		}
		now := time.Now().UTC()
		portID = newUUID()
		subnetID = sub.ID
		s.store.ports[portID] = &PortRecord{
			ID:           portID,
			Name:         "router-if-" + sub.ID[:8],
			NetworkID:    sub.NetworkID,
			ProjectID:    rt.ProjectID,
			MACAddress:   newMAC(),
			AdminStateUp: true,
			Status:       "ACTIVE",
			DeviceID:     rt.ID,
			DeviceOwner:  "network:router_interface",
			FixedIPs:     []FixedIP{{SubnetID: sub.ID, IPAddress: ip}},
			HostID:       "ovn-gateway-01",
			VIFType:      "ovs",
			VNICType:     "normal",
			VIFDetails:   map[string]any{"ovn_chassis": "ovn-gateway-01"},
			CreatedAt:    now,
			UpdatedAt:    now,
		}
		rt.SubnetPorts[subnetID] = portID
	} else {
		p, exists := s.store.ports[req.PortID]
		if !exists {
			writeAPIError(w, http.StatusNotFound, "port not found: "+req.PortID)
			return
		}
		if p.DeviceID != "" {
			writeAPIError(w, http.StatusConflict, "port is already attached to device "+p.DeviceID)
			return
		}
		if len(p.FixedIPs) != 1 {
			writeAPIError(w, http.StatusBadRequest, "port must have exactly one fixed_ip")
			return
		}
		p.DeviceID = rt.ID
		p.DeviceOwner = "network:router_interface"
		p.UpdatedAt = time.Now().UTC()
		portID = p.ID
		subnetID = p.FixedIPs[0].SubnetID
		rt.SubnetPorts[subnetID] = portID
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         rt.ID,
		"tenant_id":  rt.ProjectID,
		"subnet_id":  subnetID,
		"subnet_ids": []string{subnetID},
		"port_id":    portID,
	})
}

func (s *Server) handleNeutronRemoveRouterInterface(w http.ResponseWriter, r *http.Request) {
	routerID := r.PathValue("id")
	var req struct {
		SubnetID string `json:"subnet_id"`
		PortID   string `json:"port_id"`
	}
	if err := decodeJSON(r, &req); err != nil || (req.SubnetID == "" && req.PortID == "") {
		writeAPIError(w, http.StatusBadRequest, "subnet_id or port_id is required")
		return
	}

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	rt, ok := s.store.routers[routerID]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "router not found: "+routerID)
		return
	}

	subnetID := req.SubnetID
	portID := req.PortID
	if subnetID != "" {
		mappedPort, exists := rt.SubnetPorts[subnetID]
		if !exists {
			writeAPIError(w, http.StatusNotFound, "router interface not found for subnet "+subnetID)
			return
		}
		if portID != "" && portID != mappedPort {
			writeAPIError(w, http.StatusConflict, "port_id does not match router interface for subnet")
			return
		}
		portID = mappedPort
	} else {
		found := false
		for sid, pid := range rt.SubnetPorts {
			if pid == portID {
				subnetID = sid
				found = true
				break
			}
		}
		if !found {
			writeAPIError(w, http.StatusNotFound, "router interface not found for port "+portID)
			return
		}
	}

	delete(rt.SubnetPorts, subnetID)
	delete(s.store.ports, portID)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         rt.ID,
		"tenant_id":  rt.ProjectID,
		"subnet_id":  subnetID,
		"subnet_ids": []string{subnetID},
		"port_id":    portID,
	})
}

func neutronRouterResponse(rt *RouterRecord) map[string]any {
	resp := map[string]any{
		"id":              rt.ID,
		"name":            rt.Name,
		"description":     rt.Description,
		"status":          rt.Status,
		"admin_state_up":  rt.AdminStateUp,
		"distributed":     rt.Distributed,
		"tenant_id":       rt.ProjectID,
		"project_id":      rt.ProjectID,
		"routes":          []any{},
		"tags":            []string{},
		"created_at":      rt.CreatedAt.Format(time.RFC3339),
		"updated_at":      rt.UpdatedAt.Format(time.RFC3339),
		"revision_number": 1,
	}
	if rt.ExternalGatewayInfo != nil {
		resp["external_gateway_info"] = rt.ExternalGatewayInfo
	}
	return resp
}

// --- Security Groups & Rules ---

func (s *Server) handleNeutronListSecGroups(w http.ResponseWriter, r *http.Request) {
	nameFilter := r.URL.Query().Get("name")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.securityGroups))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		sg := s.store.securityGroups[id]
		if nameFilter != "" && sg.Name != nameFilter {
			continue
		}
		out = append(out, s.neutronSecGroupResponseLocked(sg))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"security_groups": out})
}

func (s *Server) handleNeutronCreateSecGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SecurityGroup struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			ProjectID   string `json:"project_id"`
			TenantID    string `json:"tenant_id"`
		} `json:"security_group"`
	}
	if err := decodeJSON(r, &body); err != nil || body.SecurityGroup.Name == "" {
		writeAPIError(w, http.StatusBadRequest, "security_group.name is required")
		return
	}

	tok := tokenFromContext(r.Context())
	now := time.Now().UTC()
	sg := &SecurityGroupRecord{
		ID:          newUUID(),
		Name:        body.SecurityGroup.Name,
		Description: body.SecurityGroup.Description,
		ProjectID:   cmp.Or(body.SecurityGroup.ProjectID, body.SecurityGroup.TenantID, tok.ProjectID),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Seed default IPv4 egress rule.
	defaultEgress := &SecurityGroupRuleRecord{
		ID:         newUUID(),
		SecGroupID: sg.ID,
		ProjectID:  sg.ProjectID,
		Direction:  "egress",
		EtherType:  "IPv4",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	s.store.mu.Lock()
	s.store.securityGroups[sg.ID] = sg
	s.store.secGroupRules[defaultEgress.ID] = defaultEgress
	resp := s.neutronSecGroupResponseLocked(sg)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{"security_group": resp})
}

func (s *Server) handleNeutronGetSecGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	sg, ok := s.store.securityGroups[id]
	var resp map[string]any
	if ok {
		resp = s.neutronSecGroupResponseLocked(sg)
	}
	s.store.mu.RUnlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "security group not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"security_group": resp})
}

func (s *Server) handleNeutronUpdateSecGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		SecurityGroup struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		} `json:"security_group"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid security_group update payload")
		return
	}

	s.store.mu.Lock()
	sg, ok := s.store.securityGroups[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "security group not found: "+id)
		return
	}
	if body.SecurityGroup.Name != nil {
		sg.Name = *body.SecurityGroup.Name
	}
	if body.SecurityGroup.Description != nil {
		sg.Description = *body.SecurityGroup.Description
	}
	sg.UpdatedAt = time.Now().UTC()
	resp := s.neutronSecGroupResponseLocked(sg)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"security_group": resp})
}

func (s *Server) handleNeutronDeleteSecGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	if _, ok := s.store.securityGroups[id]; !ok {
		writeAPIError(w, http.StatusNotFound, "security group not found: "+id)
		return
	}
	for _, p := range s.store.ports {
		if slices.Contains(p.SecurityGroups, id) {
			writeAPIError(w, http.StatusConflict, fmt.Sprintf("security group %s is in use by port %s", id, p.ID))
			return
		}
	}
	maps.DeleteFunc(s.store.secGroupRules, func(_ string, rule *SecurityGroupRuleRecord) bool {
		return rule.SecGroupID == id
	})
	delete(s.store.securityGroups, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNeutronListSecGroupRules(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sgFilter := q.Get("security_group_id")
	dirFilter := q.Get("direction")
	protoFilter := q.Get("protocol")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.secGroupRules))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		rule := s.store.secGroupRules[id]
		if sgFilter != "" && rule.SecGroupID != sgFilter {
			continue
		}
		if dirFilter != "" && rule.Direction != dirFilter {
			continue
		}
		if protoFilter != "" && rule.Protocol != protoFilter {
			continue
		}
		out = append(out, neutronSecGroupRuleResponse(rule))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"security_group_rules": out})
}

func (s *Server) handleNeutronCreateSecGroupRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rule struct {
			SecGroupID     string `json:"security_group_id"`
			Direction      string `json:"direction"`
			EtherType      string `json:"ethertype"`
			Protocol       string `json:"protocol"`
			PortRangeMin   int    `json:"port_range_min"`
			PortRangeMax   int    `json:"port_range_max"`
			RemoteIPPrefix string `json:"remote_ip_prefix"`
			RemoteGroupID  string `json:"remote_group_id"`
			Description    string `json:"description"`
			ProjectID      string `json:"project_id"`
		} `json:"security_group_rule"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid security_group_rule payload")
		return
	}
	if body.Rule.SecGroupID == "" || (body.Rule.Direction != "ingress" && body.Rule.Direction != "egress") {
		writeAPIError(w, http.StatusBadRequest, "security_group_id and valid direction (ingress|egress) are required")
		return
	}

	tok := tokenFromContext(r.Context())
	now := time.Now().UTC()
	rule := &SecurityGroupRuleRecord{
		ID:             newUUID(),
		SecGroupID:     body.Rule.SecGroupID,
		ProjectID:      cmp.Or(body.Rule.ProjectID, tok.ProjectID),
		Direction:      body.Rule.Direction,
		EtherType:      cmp.Or(body.Rule.EtherType, "IPv4"),
		Protocol:       body.Rule.Protocol,
		PortRangeMin:   body.Rule.PortRangeMin,
		PortRangeMax:   body.Rule.PortRangeMax,
		RemoteIPPrefix: body.Rule.RemoteIPPrefix,
		RemoteGroupID:  body.Rule.RemoteGroupID,
		Description:    body.Rule.Description,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	s.store.mu.Lock()
	if _, ok := s.store.securityGroups[rule.SecGroupID]; !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "security group not found: "+rule.SecGroupID)
		return
	}
	s.store.secGroupRules[rule.ID] = rule
	s.store.mu.Unlock()

	writeJSON(w, http.StatusCreated, map[string]any{"security_group_rule": neutronSecGroupRuleResponse(rule)})
}

func (s *Server) handleNeutronGetSecGroupRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	rule, ok := s.store.secGroupRules[id]
	s.store.mu.RUnlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "security group rule not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"security_group_rule": neutronSecGroupRuleResponse(rule)})
}

func (s *Server) handleNeutronDeleteSecGroupRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.Lock()
	if _, ok := s.store.secGroupRules[id]; !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "security group rule not found: "+id)
		return
	}
	delete(s.store.secGroupRules, id)
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) neutronSecGroupResponseLocked(sg *SecurityGroupRecord) map[string]any {
	ruleIDs := slices.Sorted(maps.Keys(s.store.secGroupRules))
	rules := make([]map[string]any, 0)
	for _, rid := range ruleIDs {
		if r := s.store.secGroupRules[rid]; r.SecGroupID == sg.ID {
			rules = append(rules, neutronSecGroupRuleResponse(r))
		}
	}
	return map[string]any{
		"id":                   sg.ID,
		"name":                 sg.Name,
		"description":          sg.Description,
		"tenant_id":            sg.ProjectID,
		"project_id":           sg.ProjectID,
		"security_group_rules": rules,
		"tags":                 []string{},
		"created_at":           sg.CreatedAt.Format(time.RFC3339),
		"updated_at":           sg.UpdatedAt.Format(time.RFC3339),
		"revision_number":      1,
	}
}

func neutronSecGroupRuleResponse(rule *SecurityGroupRuleRecord) map[string]any {
	resp := map[string]any{
		"id":                rule.ID,
		"security_group_id": rule.SecGroupID,
		"tenant_id":         rule.ProjectID,
		"project_id":        rule.ProjectID,
		"direction":         rule.Direction,
		"ethertype":         rule.EtherType,
		"protocol":          rule.Protocol,
		"remote_ip_prefix":  rule.RemoteIPPrefix,
		"remote_group_id":   rule.RemoteGroupID,
		"description":       rule.Description,
		"created_at":        rule.CreatedAt.Format(time.RFC3339),
		"updated_at":        rule.UpdatedAt.Format(time.RFC3339),
		"revision_number":   1,
	}
	if rule.PortRangeMin > 0 {
		resp["port_range_min"] = rule.PortRangeMin
	}
	if rule.PortRangeMax > 0 {
		resp["port_range_max"] = rule.PortRangeMax
	}
	return resp
}

// --- Ports (with OVN port binding) ---

func (s *Server) handleNeutronListPorts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nameFilter := q.Get("name")
	netFilter := q.Get("network_id")
	devFilter := q.Get("device_id")
	ownerFilter := q.Get("device_owner")

	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.ports))
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		p := s.store.ports[id]
		if nameFilter != "" && p.Name != nameFilter {
			continue
		}
		if netFilter != "" && p.NetworkID != netFilter {
			continue
		}
		if devFilter != "" && p.DeviceID != devFilter {
			continue
		}
		if ownerFilter != "" && p.DeviceOwner != ownerFilter {
			continue
		}
		out = append(out, neutronPortResponse(p))
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"ports": out})
}

func (s *Server) handleNeutronCreatePort(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port struct {
			Name           string    `json:"name"`
			Description    string    `json:"description"`
			NetworkID      string    `json:"network_id"`
			MACAddress     string    `json:"mac_address"`
			AdminUp        *bool     `json:"admin_state_up"`
			DeviceID       string    `json:"device_id"`
			DeviceOwner    string    `json:"device_owner"`
			FixedIPs       []FixedIP `json:"fixed_ips"`
			SecurityGroups *[]string `json:"security_groups"`
			HostID         string    `json:"binding:host_id"`
			VNICType       string    `json:"binding:vnic_type"`
			ProjectID      string    `json:"project_id"`
			TenantID       string    `json:"tenant_id"`
		} `json:"port"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Port.NetworkID == "" {
		writeAPIError(w, http.StatusBadRequest, "port.network_id is required")
		return
	}

	tok := tokenFromContext(r.Context())
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	if _, ok := s.store.networks[body.Port.NetworkID]; !ok {
		writeAPIError(w, http.StatusNotFound, "network not found: "+body.Port.NetworkID)
		return
	}

	var secGroups []string
	if body.Port.SecurityGroups != nil {
		secGroups = slices.Clone(*body.Port.SecurityGroups)
	}

	adminUp := true
	if body.Port.AdminUp != nil {
		adminUp = *body.Port.AdminUp
	}

	port, err := s.createPortLocked(portCreateParams{
		Name:           body.Port.Name,
		Description:    body.Port.Description,
		NetworkID:      body.Port.NetworkID,
		ProjectID:      cmp.Or(body.Port.ProjectID, body.Port.TenantID, tok.ProjectID),
		MACAddress:     body.Port.MACAddress,
		AdminStateUp:   adminUp,
		DeviceID:       body.Port.DeviceID,
		DeviceOwner:    body.Port.DeviceOwner,
		FixedIPs:       body.Port.FixedIPs,
		SecurityGroups: secGroups,
		HostID:         body.Port.HostID,
		VNICType:       body.Port.VNICType,
	})
	if err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"port": neutronPortResponse(port)})
}

type portCreateParams struct {
	Name           string
	Description    string
	NetworkID      string
	ProjectID      string
	MACAddress     string
	AdminStateUp   bool
	DeviceID       string
	DeviceOwner    string
	FixedIPs       []FixedIP
	SecurityGroups []string
	HostID         string
	VNICType       string
	ManagedByNova  bool
}

func (s *Server) createPortLocked(p portCreateParams) (*PortRecord, error) {
	allocated := make([]FixedIP, 0, max(len(p.FixedIPs), 1))
	if len(p.FixedIPs) > 0 {
		for _, req := range p.FixedIPs {
			var sub *SubnetRecord
			if req.SubnetID != "" {
				sub = s.store.subnets[req.SubnetID]
			} else {
				sub = s.firstSubnetForNetworkLocked(p.NetworkID)
			}
			if sub == nil {
				return nil, fmt.Errorf("no matching subnet found on network %s", p.NetworkID)
			}
			ip, err := s.store.allocateIPFromSubnetLocked(sub, req.IPAddress)
			if err != nil {
				return nil, err
			}
			allocated = append(allocated, FixedIP{SubnetID: sub.ID, IPAddress: ip})
		}
	} else if sub := s.firstSubnetForNetworkLocked(p.NetworkID); sub != nil {
		ip, err := s.store.allocateIPFromSubnetLocked(sub, "")
		if err != nil {
			return nil, err
		}
		allocated = append(allocated, FixedIP{SubnetID: sub.ID, IPAddress: ip})
	}

	vifType := "unbound"
	vifDetails := map[string]any{}
	if p.HostID != "" {
		vifType = "ovs"
		vifDetails = map[string]any{
			"port_filter":     true,
			"ovs_hybrid_plug": false,
			"connectivity":    "l2",
			"datapath_type":   "system",
			"ovn_chassis":     p.HostID,
		}
	}

	now := time.Now().UTC()
	rec := &PortRecord{
		ID:             newUUID(),
		Name:           p.Name,
		Description:    p.Description,
		NetworkID:      p.NetworkID,
		ProjectID:      p.ProjectID,
		MACAddress:     cmp.Or(p.MACAddress, newMAC()),
		AdminStateUp:   p.AdminStateUp,
		Status:         "ACTIVE",
		DeviceID:       p.DeviceID,
		DeviceOwner:    p.DeviceOwner,
		FixedIPs:       allocated,
		SecurityGroups: slices.Clone(p.SecurityGroups),
		HostID:         p.HostID,
		VIFType:        vifType,
		VNICType:       cmp.Or(p.VNICType, "normal"),
		VIFDetails:     vifDetails,
		ManagedByNova:  p.ManagedByNova,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	s.store.ports[rec.ID] = rec
	return rec, nil
}

func (s *Server) firstSubnetForNetworkLocked(networkID string) *SubnetRecord {
	ids := slices.Sorted(maps.Keys(s.store.subnets))
	for _, id := range ids {
		if sub := s.store.subnets[id]; sub.NetworkID == networkID {
			return sub
		}
	}
	return nil
}

func (s *Server) handleNeutronGetPort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.RLock()
	p, ok := s.store.ports[id]
	s.store.mu.RUnlock()
	if !ok {
		writeAPIError(w, http.StatusNotFound, "port not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"port": neutronPortResponse(p)})
}

func (s *Server) handleNeutronUpdatePort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Port struct {
			Name           *string   `json:"name"`
			Description    *string   `json:"description"`
			AdminUp        *bool     `json:"admin_state_up"`
			DeviceID       *string   `json:"device_id"`
			DeviceOwner    *string   `json:"device_owner"`
			SecurityGroups *[]string `json:"security_groups"`
			HostID         *string   `json:"binding:host_id"`
			VNICType       *string   `json:"binding:vnic_type"`
		} `json:"port"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Neutron port update payload")
		return
	}

	s.store.mu.Lock()
	p, ok := s.store.ports[id]
	if !ok {
		s.store.mu.Unlock()
		writeAPIError(w, http.StatusNotFound, "port not found: "+id)
		return
	}
	if body.Port.Name != nil {
		p.Name = *body.Port.Name
	}
	if body.Port.Description != nil {
		p.Description = *body.Port.Description
	}
	if body.Port.AdminUp != nil {
		p.AdminStateUp = *body.Port.AdminUp
	}
	if body.Port.DeviceID != nil {
		p.DeviceID = *body.Port.DeviceID
	}
	if body.Port.DeviceOwner != nil {
		p.DeviceOwner = *body.Port.DeviceOwner
	}
	if body.Port.SecurityGroups != nil {
		p.SecurityGroups = slices.Clone(*body.Port.SecurityGroups)
	}
	if body.Port.HostID != nil {
		p.HostID = *body.Port.HostID
		if p.HostID != "" {
			p.VIFType = "ovs"
			p.VIFDetails = map[string]any{
				"port_filter":     true,
				"ovs_hybrid_plug": false,
				"connectivity":    "l2",
				"datapath_type":   "system",
				"ovn_chassis":     p.HostID,
			}
		} else {
			p.VIFType = "unbound"
			p.VIFDetails = map[string]any{}
		}
	}
	if body.Port.VNICType != nil {
		p.VNICType = *body.Port.VNICType
	}
	p.UpdatedAt = time.Now().UTC()
	resp := neutronPortResponse(p)
	s.store.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"port": resp})
}

func (s *Server) handleNeutronDeletePort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	p, ok := s.store.ports[id]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "port not found: "+id)
		return
	}
	if p.DeviceOwner == "network:router_interface" {
		writeAPIError(w, http.StatusConflict, "port is attached to a router; call remove_router_interface first")
		return
	}
	delete(s.store.ports, id)
	w.WriteHeader(http.StatusNoContent)
}

func neutronPortResponse(p *PortRecord) map[string]any {
	fips := p.FixedIPs
	if fips == nil {
		fips = []FixedIP{}
	}
	sgs := p.SecurityGroups
	if sgs == nil {
		sgs = []string{}
	}
	vifDetails := p.VIFDetails
	if vifDetails == nil {
		vifDetails = map[string]any{}
	}
	return map[string]any{
		"id":                    p.ID,
		"name":                  p.Name,
		"description":           p.Description,
		"network_id":            p.NetworkID,
		"tenant_id":             p.ProjectID,
		"project_id":            p.ProjectID,
		"mac_address":           p.MACAddress,
		"admin_state_up":        p.AdminStateUp,
		"status":                p.Status,
		"device_id":             p.DeviceID,
		"device_owner":          p.DeviceOwner,
		"fixed_ips":             fips,
		"security_groups":       sgs,
		"allowed_address_pairs": []any{},
		"binding:host_id":       p.HostID,
		"binding:vif_type":      p.VIFType,
		"binding:vnic_type":     p.VNICType,
		"binding:vif_details":   vifDetails,
		"binding:profile":       map[string]any{},
		"port_security_enabled": true,
		"tags":                  []string{},
		"created_at":            p.CreatedAt.Format(time.RFC3339),
		"updated_at":            p.UpdatedAt.Format(time.RFC3339),
		"revision_number":       1,
	}
}
