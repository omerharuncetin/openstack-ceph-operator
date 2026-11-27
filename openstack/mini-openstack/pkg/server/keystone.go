package server

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"net/http"
	"slices"
	"time"
)

func (s *Server) registerKeystoneRoutes() {
	s.mux.HandleFunc("GET /", s.handleKeystoneVersions)
	s.mux.HandleFunc("GET /v3", s.handleKeystoneV3Version)
	s.mux.HandleFunc("POST /v3/auth/tokens", s.handleKeystoneCreateToken)
	s.mux.HandleFunc("GET /v3/auth/tokens", s.requireAuth(s.handleKeystoneGetToken))
	s.mux.HandleFunc("GET /v3/projects", s.requireAuth(s.handleKeystoneListProjects))
}

func (s *Server) handleKeystoneVersions(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusMultipleChoices, map[string]any{
		"versions": map[string]any{
			"values": []map[string]any{
				{
					"id":     "v3.14",
					"status": "stable",
					"links": []map[string]string{
						{"rel": "self", "href": base + "/v3/"},
					},
				},
			},
		},
	})
}

func (s *Server) handleKeystoneV3Version(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": map[string]any{
			"id":     "v3.14",
			"status": "stable",
			"links": []map[string]string{
				{"rel": "self", "href": base + "/v3/"},
			},
		},
	})
}

type keystoneAuthRequest struct {
	Auth struct {
		Identity struct {
			Methods  []string `json:"methods"`
			Password struct {
				User struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Password string `json:"password"`
					Domain   struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"domain"`
				} `json:"user"`
			} `json:"password"`
			Token struct {
				ID string `json:"id"`
			} `json:"token"`
		} `json:"identity"`
		Scope struct {
			Project struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"project"`
		} `json:"scope"`
	} `json:"auth"`
}

func (s *Server) handleKeystoneCreateToken(w http.ResponseWriter, r *http.Request) {
	var req keystoneAuthRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid Keystone auth payload")
		return
	}

	methods := req.Auth.Identity.Methods
	if len(methods) == 0 {
		writeAPIError(w, http.StatusBadRequest, "auth.identity.methods is required")
		return
	}

	var username string
	switch {
	case slices.Contains(methods, "password"):
		u := req.Auth.Identity.Password.User
		if u.Name != s.username || u.Password != s.password {
			writeAPIError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		username = u.Name
	case slices.Contains(methods, "token"):
		s.store.mu.RLock()
		existing, ok := s.store.tokens[req.Auth.Identity.Token.ID]
		s.store.mu.RUnlock()
		if !ok || time.Now().UTC().After(existing.ExpiresAt) {
			writeAPIError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		username = existing.Username
	default:
		writeAPIError(w, http.StatusBadRequest, "unsupported auth method")
		return
	}

	project := s.resolveOrCreateProject(req.Auth.Scope.Project.ID, req.Auth.Scope.Project.Name)

	var raw [24]byte
	_, _ = rand.Read(raw[:])
	tokenID := "gAAAAAB" + hex.EncodeToString(raw[:])

	now := time.Now().UTC()
	rec := &TokenRecord{
		Token:       tokenID,
		UserID:      DefaultUserID,
		Username:    username,
		ProjectID:   project.ID,
		ProjectName: project.Name,
		IssuedAt:    now,
		ExpiresAt:   now.Add(24 * time.Hour),
	}

	s.store.mu.Lock()
	s.store.tokens[tokenID] = rec
	s.store.mu.Unlock()

	w.Header().Set("X-Subject-Token", tokenID)
	writeJSON(w, http.StatusCreated, s.tokenEnvelope(r, rec, project))
}

func (s *Server) handleKeystoneGetToken(w http.ResponseWriter, r *http.Request) {
	subject := cmp.Or(r.Header.Get("X-Subject-Token"), r.Header.Get("X-Auth-Token"))
	s.store.mu.RLock()
	rec, ok := s.store.tokens[subject]
	var project *ProjectRecord
	if ok {
		project = s.store.projects[rec.ProjectID]
	}
	s.store.mu.RUnlock()

	if !ok || time.Now().UTC().After(rec.ExpiresAt) {
		writeAPIError(w, http.StatusNotFound, "token not found")
		return
	}
	if project == nil {
		project = &ProjectRecord{ID: rec.ProjectID, Name: rec.ProjectName, DomainID: DefaultDomainID, Enabled: true}
	}

	w.Header().Set("X-Subject-Token", rec.Token)
	writeJSON(w, http.StatusOK, s.tokenEnvelope(r, rec, project))
}

func (s *Server) handleKeystoneListProjects(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.RLock()
	ids := slices.Sorted(maps.Keys(s.store.projects))
	projects := make([]*ProjectRecord, 0, len(ids))
	for _, id := range ids {
		projects = append(projects, s.store.projects[id])
	}
	s.store.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) resolveOrCreateProject(id, name string) *ProjectRecord {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	if id != "" {
		if p, ok := s.store.projects[id]; ok {
			return p
		}
	}
	if name != "" {
		for _, p := range s.store.projects {
			if p.Name == name {
				return p
			}
		}
		// Create missing tenant project on demand.
		p := &ProjectRecord{
			ID:          cmp.Or(id, newUUID()),
			Name:        name,
			DomainID:    DefaultDomainID,
			Description: "Tenant project " + name,
			Enabled:     true,
		}
		s.store.projects[p.ID] = p
		return p
	}
	return s.store.projects[DefaultProjectID]
}

func (s *Server) tokenEnvelope(r *http.Request, tok *TokenRecord, project *ProjectRecord) map[string]any {
	base := s.baseURL(r)
	endpoint := func(id, url string) []map[string]any {
		return []map[string]any{
			{
				"id":        id,
				"interface": "public",
				"region":    s.region,
				"region_id": s.region,
				"url":       url,
			},
		}
	}

	catalog := []map[string]any{
		{
			"id":        "svc-identity",
			"name":      "keystone",
			"type":      "identity",
			"endpoints": endpoint("ep-identity", base+"/v3"),
		},
		{
			"id":        "svc-image",
			"name":      "glance",
			"type":      "image",
			"endpoints": endpoint("ep-image", base+"/v2"),
		},
		{
			"id":        "svc-block-storage",
			"name":      "cinderv3",
			"type":      "block-storage",
			"endpoints": endpoint("ep-block-storage", base+"/v3/"+project.ID),
		},
		{
			"id":        "svc-network",
			"name":      "neutron",
			"type":      "network",
			"endpoints": endpoint("ep-network", base+"/v2.0"),
		},
		{
			"id":        "svc-compute",
			"name":      "nova",
			"type":      "compute",
			"endpoints": endpoint("ep-compute", base+"/v2.1"),
		},
	}

	return map[string]any{
		"token": map[string]any{
			"methods":    []string{"password"},
			"issued_at":  tok.IssuedAt.Format(time.RFC3339Nano),
			"expires_at": tok.ExpiresAt.Format(time.RFC3339Nano),
			"user": map[string]any{
				"id":   tok.UserID,
				"name": tok.Username,
				"domain": map[string]string{
					"id":   DefaultDomainID,
					"name": DefaultDomainName,
				},
			},
			"project": map[string]any{
				"id":   project.ID,
				"name": project.Name,
				"domain": map[string]string{
					"id":   DefaultDomainID,
					"name": DefaultDomainName,
				},
			},
			"roles": []map[string]string{
				{"id": "role-admin", "name": "admin"},
			},
			"catalog": catalog,
		},
	}
}
