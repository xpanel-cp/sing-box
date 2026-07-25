package option

import "github.com/sagernet/sing/common/json/badoption"

type ExperimentalOptions struct {
	CacheFile        *CacheFileOptions        `json:"cache_file,omitempty"`
	ClashAPI         *ClashAPIOptions         `json:"clash_api,omitempty"`
	V2RayAPI         *V2RayAPIOptions         `json:"v2ray_api,omitempty"`
	Debug            *DebugOptions            `json:"debug,omitempty"`
	SessionAdmission *SessionAdmissionOptions `json:"session_admission,omitempty"`
}

// SessionAdmissionOptions is the config schema for the pre-connection session
// admission gate (experimental.session_admission).
//
// It is declared here, in the option package, rather than importing the
// experimental/sessionadmission package: that package imports adapter, and
// adapter imports option, so importing sessionadmission from option would form
// an import cycle. This struct mirrors the sessionadmission package's own
// Options JSON; startup wiring translates parsed values into the runtime types.
type SessionAdmissionOptions struct {
	Enabled         bool                   `json:"enabled,omitempty"`
	DefaultLimit    int                    `json:"default_limit,omitempty"`
	FailureMode     string                 `json:"failure_mode,omitempty"`      // "allow" (default) | "reject"
	SlotGracePeriod badoption.Duration     `json:"slot_grace_period,omitempty"` // 0 = disabled
	Users           []SessionAdmissionUser `json:"users,omitempty"`

	// Central Session Manager (Phase 3 RemoteStore) options. When ManagerEndpoint
	// is set AND Enabled, startup wiring backs the admission Gate with a
	// RemoteStore (central, multi-node enforcement) instead of the per-node
	// in-memory MemoryStore. All four are optional; absent ManagerEndpoint keeps
	// the local MemoryStore behavior unchanged.
	ManagerEndpoint string `json:"manager_endpoint,omitempty"` // manager gRPC target "host:port"
	NodeID          string `json:"node_id,omitempty"`          // this node's identity to the manager
	NodeSecret      string `json:"node_secret,omitempty"`      // shared HMAC secret for per-RPC signing
	MaxSessions     int    `json:"max_sessions,omitempty"`     // node-side concurrent-session cap (0 = disabled)
}

// SessionAdmissionUser maps a user UUID to its maximum device count.
type SessionAdmissionUser struct {
	UUID       string `json:"uuid"`
	MaxDevices int    `json:"max_devices"`
}

type CacheFileOptions struct {
	Enabled     bool               `json:"enabled,omitempty"`
	Path        string             `json:"path,omitempty"`
	CacheID     string             `json:"cache_id,omitempty"`
	StoreFakeIP bool               `json:"store_fakeip,omitempty"`
	StoreRDRC   bool               `json:"store_rdrc,omitempty"`
	RDRCTimeout badoption.Duration `json:"rdrc_timeout,omitempty"`
	StoreDNS    bool               `json:"store_dns,omitempty"`
}

type ClashAPIOptions struct {
	ExternalController               string                     `json:"external_controller,omitempty"`
	ExternalUI                       string                     `json:"external_ui,omitempty"`
	ExternalUIDownloadURL            string                     `json:"external_ui_download_url,omitempty"`
	ExternalUIDownloadDetour         string                     `json:"external_ui_download_detour,omitempty"`
	Secret                           string                     `json:"secret,omitempty"`
	DefaultMode                      string                     `json:"default_mode,omitempty"`
	ModeList                         []string                   `json:"-"`
	AccessControlAllowOrigin         badoption.Listable[string] `json:"access_control_allow_origin,omitempty"`
	AccessControlAllowPrivateNetwork bool                       `json:"access_control_allow_private_network,omitempty"`

	// Deprecated: migrated to global cache file
	CacheFile string `json:"cache_file,omitempty"`
	// Deprecated: migrated to global cache file
	CacheID string `json:"cache_id,omitempty"`
	// Deprecated: migrated to global cache file
	StoreMode bool `json:"store_mode,omitempty"`
	// Deprecated: migrated to global cache file
	StoreSelected bool `json:"store_selected,omitempty"`
	// Deprecated: migrated to global cache file
	StoreFakeIP bool `json:"store_fakeip,omitempty"`
}

type V2RayAPIOptions struct {
	Listen string                    `json:"listen,omitempty"`
	Stats  *V2RayStatsServiceOptions `json:"stats,omitempty"`
}

type V2RayStatsServiceOptions struct {
	Enabled   bool     `json:"enabled,omitempty"`
	Inbounds  []string `json:"inbounds,omitempty"`
	Outbounds []string `json:"outbounds,omitempty"`
	Users     []string `json:"users,omitempty"`
}
