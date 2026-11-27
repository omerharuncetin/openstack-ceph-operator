// Package cephapi implements a Go client for the Ceph Manager
// (ceph-mgr) Dashboard REST API v1.0 (https://<mgr>:8443/api).
//
// Using the MGR Dashboard REST API avoids CGO/librados build dependencies
// while talking directly to Ceph clusters (Reef, Squid v19, Tentacle).
package cephapi

import "fmt"

// AcceptHeader is the versioned media type required by Ceph MGR Dashboard API v1.0.
const AcceptHeader = "application/vnd.ceph.api.v1.0+json"

// Config configures the Ceph Dashboard REST API client.
type Config struct {
	BaseURL            string // e.g. "https://localhost:8443"
	Username           string // e.g. "admin"
	Password           string
	InsecureSkipVerify bool // Allow self-signed cert from `ceph dashboard create-self-signed-cert`
}

// APIError captures structured HTTP errors returned by the Ceph Dashboard API.
type APIError struct {
	StatusCode int
	Method     string
	Path       string
	Detail     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ceph api %s %s failed (HTTP %d): %s", e.Method, e.Path, e.StatusCode, e.Detail)
}

// AuthResponse is returned by POST /api/auth.
type AuthResponse struct {
	Token    string `json:"token"`
	Username string `json:"username"`
}

// HealthMinimal mirrors the exact GET /api/health/minimal schema in Ceph Squid (v19).
type HealthMinimal struct {
	Health struct {
		Status string `json:"status"` // HEALTH_OK, HEALTH_WARN, HEALTH_ERR
	} `json:"health"`
	OSDMap struct {
		OSDs []OSD `json:"osds"`
	} `json:"osd_map"`
	PGInfo struct {
		Statuses    map[string]int `json:"statuses,omitempty"`
		ObjectStats struct {
			NumObjects         int64 `json:"num_objects"`
			NumObjectCopies    int64 `json:"num_object_copies"`
			NumObjectsDegraded int64 `json:"num_objects_degraded"`
		} `json:"object_stats"`
	} `json:"pg_info"`
	MgrMap struct {
		ActiveName string `json:"active_name,omitempty"`
	} `json:"mgr_map"`
	DF struct {
		Stats struct {
			TotalBytes        int64 `json:"total_bytes"`
			TotalAvailBytes   int64 `json:"total_avail_bytes"`
			TotalUsedRawBytes int64 `json:"total_used_raw_bytes"`
		} `json:"stats"`
	} `json:"df"`
	Hosts int `json:"hosts,omitzero"`
}

// NumOSDs returns total OSDs reported in osd_map.osds.
func (h *HealthMinimal) NumOSDs() int { return len(h.OSDMap.OSDs) }

// NumUpOSDs returns how many OSDs are currently up.
func (h *HealthMinimal) NumUpOSDs() int {
	n := 0
	for _, o := range h.OSDMap.OSDs {
		if o.Up > 0 {
			n++
		}
	}
	return n
}

// NumInOSDs returns how many OSDs are currently in.
func (h *HealthMinimal) NumInOSDs() int {
	n := 0
	for _, o := range h.OSDMap.OSDs {
		if o.In > 0 {
			n++
		}
	}
	return n
}

// OSD summarizes an Object Storage Daemon from GET /api/osd.
type OSD struct {
	OSDID  int32    `json:"osd"`
	UUID   string   `json:"uuid,omitempty"`
	Up     int      `json:"up"`
	In     int      `json:"in"`
	Weight float64  `json:"weight,omitzero"`
	Host   string   `json:"host,omitempty"`
	State  []string `json:"state,omitempty"`
}

// CrushRule represents a CRUSH placement rule from GET /api/crush_rule.
type CrushRule struct {
	RuleID   int    `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Type     int    `json:"type"` // 1 = replicated, 3 = erasure
}

// Pool mirrors a RADOS storage pool returned by GET /api/pool.
type Pool struct {
	PoolID              int            `json:"pool"`
	PoolName            string         `json:"pool_name"`
	Type                string         `json:"type"` // "replicated" or "erasure"
	Size                int            `json:"size"`
	MinSize             int            `json:"min_size"`
	PGNum               int            `json:"pg_num"`
	CrushRule           string         `json:"crush_rule,omitempty"`
	ApplicationMetadata []string       `json:"application_metadata,omitempty"`
	PGAutoscaleMode     string         `json:"pg_autoscale_mode,omitempty"`
	QuotaMaxBytes       int64          `json:"quota_max_bytes,omitzero"`
	Options             map[string]any `json:"options,omitempty"`
}

// CreatePoolRequest maps to POST /api/pool.
type CreatePoolRequest struct {
	Pool                string   `json:"pool"`
	PoolType            string   `json:"pool_type"`                      // "replicated" or "erasure"
	PGNum               int      `json:"pg_num"`                         // e.g. 16, 32, 64
	Size                int      `json:"size,omitzero"`                  // Replicated size (e.g. 1 or 3)
	MinSize             int      `json:"min_size,omitzero"`              // Minimum active OSDs
	RuleName            string   `json:"rule_name,omitempty"`            // CRUSH rule name
	ApplicationMetadata []string `json:"application_metadata,omitempty"` // e.g. ["rbd"]
	PGAutoscaleMode     string   `json:"pg_autoscale_mode,omitempty"`    // "on", "warn", "off"
	QuotaMaxBytes       int64    `json:"quota_max_bytes,omitzero"`
}

// RBDParent identifies the parent snapshot of a Copy-on-Write RBD clone.
type RBDParent struct {
	PoolName  string `json:"pool_name"`
	ImageName string `json:"image_name"`
	SnapName  string `json:"snap_name"`
}

// RBDSnapshot represents a snapshot on an RBD image.
type RBDSnapshot struct {
	ID          int64  `json:"id,omitzero"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	IsProtected bool   `json:"is_protected"`
}

// RBDImage represents an RBD block device volume from /api/block/image.
type RBDImage struct {
	ID           string        `json:"id,omitempty"`
	Name         string        `json:"name"`
	PoolName     string        `json:"pool_name"`
	Size         int64         `json:"size"`
	DiskUsage    int64         `json:"disk_usage,omitzero"`
	FeaturesName []string      `json:"features_name,omitempty"`
	Parent       *RBDParent    `json:"parent,omitempty"`
	Snapshots    []RBDSnapshot `json:"snapshots,omitempty"`
}

// CreateRBDImageRequest maps to POST /api/block/image.
type CreateRBDImageRequest struct {
	Name     string   `json:"name"`
	PoolName string   `json:"pool_name"`
	Size     int64    `json:"size"` // Size in bytes
	Features []string `json:"features,omitempty"`
}

// ResizeRBDImageRequest maps to PUT /api/block/image/{image_spec}.
type ResizeRBDImageRequest struct {
	Size int64 `json:"size"`
}

// CreateSnapshotRequest maps to POST /api/block/image/{image_spec}/snap.
type CreateSnapshotRequest struct {
	SnapshotName        string `json:"snapshot_name"`
	MirrorImageSnapshot bool   `json:"mirrorImageSnapshot"`
}

// ProtectSnapshotRequest maps to PUT /api/block/image/{image_spec}/snap/{snap_name}.
type ProtectSnapshotRequest struct {
	IsProtected bool `json:"is_protected"`
}

// CloneSnapshotRequest maps to POST /api/block/image/{image_spec}/snap/{snap_name}/clone.
type CloneSnapshotRequest struct {
	ChildPoolName  string   `json:"child_pool_name"`
	ChildImageName string   `json:"child_image_name"`
	Features       []string `json:"features,omitempty"`
}

// PoolRBDListResponse matches the pool-grouped structure returned by GET /api/block/image.
type PoolRBDListResponse struct {
	PoolName string     `json:"pool_name"`
	Value    []RBDImage `json:"value"`
}
