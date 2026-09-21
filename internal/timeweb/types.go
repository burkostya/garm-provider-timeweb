// Package timeweb implements the small part of Timeweb Cloud's public API used
// by this provider. It deliberately does not retain passwords or cloud-init
// contents returned in server responses.
package timeweb

// Server is a Timeweb virtual server. IDs use int64 to preserve the full integer
// value supplied by the API; they must not be decoded through floating point.
type Server struct {
	ID               int64           `json:"id"`
	Name             string          `json:"name"`
	Comment          string          `json:"comment"`
	Status           string          `json:"status"`
	Location         string          `json:"location"`
	AvailabilityZone string          `json:"availability_zone"`
	OS               ServerOS        `json:"os"`
	Networks         []ServerNetwork `json:"networks"`
}

type ServerOS struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ServerNetwork struct {
	ID      string      `json:"id"`
	Type    string      `json:"type"`
	NATMode string      `json:"nat_mode"`
	IPs     []IPAddress `json:"ips"`
}

type IPAddress struct {
	Type   string `json:"type"`
	IP     string `json:"ip"`
	IsMain bool   `json:"is_main"`
}

// CreateServerRequest selects an existing preset and either an OS or a custom
// image. Network and SSH keys refer to existing resources. Backup schedules,
// network creation, and SSH key creation are outside this API surface.
type CreateServerRequest struct {
	Name             string               `json:"name"`
	Comment          string               `json:"comment,omitempty"`
	PresetID         int64                `json:"preset_id"`
	OSID             int64                `json:"os_id,omitempty"`
	ImageID          string               `json:"image_id,omitempty"`
	CloudInit        string               `json:"cloud_init,omitempty"`
	Network          *CreateServerNetwork `json:"network,omitempty"`
	SSHKeysIDs       []int64              `json:"ssh_keys_ids,omitempty"`
	AvailabilityZone string               `json:"availability_zone,omitempty"`
	ProjectID        int64                `json:"project_id,omitempty"`
	Hostname         string               `json:"hostname,omitempty"`
	IsDdosGuard      bool                 `json:"is_ddos_guard"`
}

type CreateServerNetwork struct {
	ID      string `json:"id"`
	LocalIP string `json:"local_ip,omitempty"`
	// Leave FloatingIP empty to avoid allocating or attaching a public IP.
	FloatingIP string `json:"floating_ip,omitempty"`
}
