// Package provider implements GARM's external provider interface for Timeweb.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/burkostya/garm-provider-timeweb/internal/config"
	"github.com/burkostya/garm-provider-timeweb/internal/timeweb"
	"github.com/cloudbase/garm-provider-common/cloudconfig"
	garmerrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/cloudbase/garm-provider-common/util"
)

// A provider process lives for one GARM command. Ownership lives in the server's
// comment, so discovery and failed-create cleanup survive process restarts.
type Provider struct {
	config       config.Config
	controllerID string
	version      string
	client       serverClient
}

type serverClient interface {
	ListServers(context.Context) ([]timeweb.Server, error)
	GetServer(context.Context, int64) (timeweb.Server, error)
	CreateServer(context.Context, timeweb.CreateServerRequest) (timeweb.Server, error)
	DeleteServer(context.Context, int64) error
	PerformAction(context.Context, int64, string) error
	SetNATMode(context.Context, int64, string) error
}

var _ common.ExternalProvider = (*Provider)(nil)

var identityPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// Compact keys keep ownership below Timeweb's 255-character comment limit even
// for 64-character namespace, controller and pool identifiers.
type ownership struct {
	Provider   string `json:"p"`
	Version    int    `json:"v"`
	Namespace  string `json:"n"`
	Controller string `json:"c"`
	Pool       string `json:"l"`
}

func New(cfg config.Config, controllerID, version string) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !identityPattern.MatchString(controllerID) {
		return nil, fmt.Errorf("controller ID must be 1-64 letters, digits, underscores, dots or hyphens")
	}
	client, err := timeweb.NewClient(cfg.APIURL, cfg.Token, &http.Client{Timeout: cfg.RequestTimeout()})
	if err != nil {
		return nil, err
	}
	return &Provider{config: cfg, controllerID: controllerID, version: version, client: client}, nil
}

func (p *Provider) GetVersion(context.Context) string { return p.version }

func (p *Provider) marker(poolID string) (string, error) {
	data, err := json.Marshal(ownership{Provider: "garm-timeweb", Version: 1, Namespace: p.config.Namespace, Controller: p.controllerID, Pool: poolID})
	if err != nil || len(data) > 255 {
		return "", fmt.Errorf("ownership metadata exceeds Timeweb's comment limit")
	}
	return string(data), nil
}

func (p *Provider) owned(server timeweb.Server) (ownership, bool) {
	var owner ownership
	if len(server.Comment) > 255 || json.Unmarshal([]byte(server.Comment), &owner) != nil {
		return owner, false
	}
	ok := owner.Provider == "garm-timeweb" && owner.Version == 1 &&
		owner.Namespace == p.config.Namespace && owner.Controller == p.controllerID &&
		identityPattern.MatchString(owner.Pool)
	return owner, ok
}

func (p *Provider) CreateInstance(ctx context.Context, bootstrap params.BootstrapInstance) (params.ProviderInstance, error) {
	cfg, request, err := p.createRequest(bootstrap)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	// Reconcile a previous successful request before attempting another POST.
	// An interrupted POST is never blindly retried by the HTTP client.
	existing, err := p.findByName(ctx, bootstrap.Name)
	if err == nil {
		owner, _ := p.owned(existing)
		if owner.Pool != bootstrap.PoolID {
			return params.ProviderInstance{}, garmerrors.NewDuplicateUserError("runner name already belongs to another pool")
		}
		if err := p.configureNetwork(ctx, cfg, existing); err != nil {
			return params.ProviderInstance{}, err
		}
		return instance(existing), nil
	}
	if !errors.Is(err, garmerrors.ErrNotFound) {
		return params.ProviderInstance{}, err
	}
	server, err := p.client.CreateServer(ctx, request)
	if err != nil {
		// Timeweb may have accepted a timed-out POST. The atomic comment and
		// GARM name let a later DeleteInstance find it without its numeric ID.
		return params.ProviderInstance{}, fmt.Errorf("create Timeweb server (cleanup can resolve the runner name): %w", err)
	}
	if server.ID <= 0 {
		return params.ProviderInstance{}, fmt.Errorf("create Timeweb server returned no valid ID; reconcile by runner name")
	}
	if err := p.configureNetwork(ctx, cfg, server); err != nil {
		// Preserve a cleanup opportunity after cancellation of the create call.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.RequestTimeout())
		defer cancel()
		cleanupErr := p.client.DeleteServer(cleanupCtx, server.ID)
		if cleanupErr != nil && !errors.Is(cleanupErr, timeweb.ErrNotFound) {
			return params.ProviderInstance{}, errors.Join(err, fmt.Errorf("rollback server %d failed; GARM must retry cleanup by name: %w", server.ID, cleanupErr))
		}
		return params.ProviderInstance{}, err
	}
	// The request is authoritative for the name; some asynchronous create
	// responses do not yet echo all fields.
	server.Name = bootstrap.Name
	return instance(server), nil
}

func (p *Provider) configureNetwork(ctx context.Context, cfg config.Config, server timeweb.Server) error {
	if cfg.NetworkMode == "" {
		return nil
	}
	for _, network := range server.Networks {
		if network.ID == cfg.NetworkID && network.NATMode == cfg.NetworkMode {
			return nil
		}
	}
	// A freshly accepted VM can be locked during installation. Bound retries
	// of this idempotent PATCH; creation itself is never retried.
	ctx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout())
	defer cancel()
	for {
		err := p.client.SetNATMode(ctx, server.ID, cfg.NetworkMode)
		if err == nil {
			return nil
		}
		var apiErr *timeweb.APIError
		if !errors.As(err, &apiErr) || (apiErr.StatusCode != http.StatusLocked && apiErr.StatusCode != http.StatusConflict) {
			return fmt.Errorf("configure server network: %w", err)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("configure server network: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (p *Provider) GetInstance(ctx context.Context, id string) (params.ProviderInstance, error) {
	server, err := p.resolve(ctx, id)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	return instance(server), nil
}

func (p *Provider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	if !identityPattern.MatchString(poolID) {
		return nil, garmerrors.NewBadRequestError("invalid pool ID")
	}
	servers, err := p.client.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(servers, func(i, j int) bool { return servers[i].ID < servers[j].ID })
	instances := make([]params.ProviderInstance, 0)
	for _, server := range servers {
		if owner, ok := p.owned(server); ok && owner.Pool == poolID {
			instances = append(instances, instance(server))
		}
	}
	return instances, nil
}

func (p *Provider) DeleteInstance(ctx context.Context, id string) error {
	server, err := p.resolve(ctx, id)
	if errors.Is(err, garmerrors.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := p.client.DeleteServer(ctx, server.ID); err != nil && !errors.Is(err, timeweb.ErrNotFound) {
		return fmt.Errorf("delete server %d: %w", server.ID, err)
	}
	return nil
}

func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	servers, err := p.client.ListServers(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, server := range servers {
		if _, ok := p.owned(server); !ok {
			continue
		}
		if err := p.DeleteInstance(ctx, strconv.FormatInt(server.ID, 10)); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (p *Provider) Start(ctx context.Context, id string) error {
	server, err := p.resolve(ctx, id)
	if err != nil {
		return err
	}
	if server.Status == "on" {
		return nil
	}
	return p.client.PerformAction(ctx, server.ID, "start")
}

func (p *Provider) Stop(ctx context.Context, id string, force bool) error {
	server, err := p.resolve(ctx, id)
	if err != nil {
		return err
	}
	if server.Status == "off" {
		return nil
	}
	action := "shutdown"
	if force {
		action = "hard_shutdown"
	}
	return p.client.PerformAction(ctx, server.ID, action)
}

func (p *Provider) resolve(ctx context.Context, id string) (timeweb.Server, error) {
	if number, err := strconv.ParseInt(id, 10, 64); err == nil && number > 0 {
		server, err := p.client.GetServer(ctx, number)
		if errors.Is(err, timeweb.ErrNotFound) {
			return timeweb.Server{}, garmerrors.NewNotFoundError("instance not found")
		}
		if err != nil {
			return timeweb.Server{}, err
		}
		if _, ok := p.owned(server); !ok {
			return timeweb.Server{}, garmerrors.NewNotFoundError("instance not owned by this controller and namespace")
		}
		return server, nil
	}
	if !namePattern.MatchString(id) {
		return timeweb.Server{}, garmerrors.NewBadRequestError("instance must be a positive Timeweb ID or a runner name")
	}
	return p.findByName(ctx, id)
}

func (p *Provider) findByName(ctx context.Context, name string) (timeweb.Server, error) {
	servers, err := p.client.ListServers(ctx)
	if err != nil {
		return timeweb.Server{}, err
	}
	var match *timeweb.Server
	for _, server := range servers {
		if _, ok := p.owned(server); !ok || server.Name != name {
			continue
		}
		if match != nil {
			return timeweb.Server{}, garmerrors.NewDuplicateUserError("multiple owned servers have this runner name; resolve their IDs before retrying")
		}
		copy := server
		match = &copy
	}
	if match == nil {
		return timeweb.Server{}, garmerrors.NewNotFoundError("instance not found")
	}
	return *match, nil
}

func instance(server timeweb.Server) params.ProviderInstance {
	result := params.ProviderInstance{
		ProviderID: strconv.FormatInt(server.ID, 10), Name: server.Name,
		OSType: params.Linux, OSArch: params.Amd64,
		OSName: server.OS.Name, OSVersion: server.OS.Version,
		Status: params.InstanceStatusUnknown,
	}
	// GARM v0.2.1 accepts only running/stopped/error/unknown from providers.
	switch server.Status {
	case "on":
		result.Status = params.InstanceRunning
	case "off":
		result.Status = params.InstanceStopped
	case "fail":
		result.Status = params.InstanceError
		result.ProviderFault = []byte("Timeweb reports a failed server")
	}
	seen := make(map[string]bool)
	for _, network := range server.Networks {
		addressType := params.PrivateAddress
		if network.Type == "public" {
			addressType = params.PublicAddress
		}
		for _, address := range network.IPs {
			ip := net.ParseIP(address.IP)
			if ip == nil {
				continue
			}
			key := string(addressType) + "/" + ip.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			result.Addresses = append(result.Addresses, params.Address{Address: ip.String(), Type: addressType})
		}
	}
	return result
}

// Allow upstream template customization alongside per-pool Timeweb overrides.
type extraSpecs struct {
	cloudconfig.CloudConfigSpec
	NetworkID        *string  `json:"network_id,omitempty"`
	NetworkMode      *string  `json:"network_mode,omitempty"`
	AvailabilityZone *string  `json:"availability_zone,omitempty"`
	ProjectID        *int64   `json:"project_id,omitempty"`
	SSHKeyIDs        *[]int64 `json:"ssh_key_ids,omitempty"`
}

func (p *Provider) createRequest(b params.BootstrapInstance) (config.Config, timeweb.CreateServerRequest, error) {
	cfg := p.config
	var request timeweb.CreateServerRequest
	bad := func(message string) (config.Config, timeweb.CreateServerRequest, error) {
		return cfg, request, garmerrors.NewBadRequestError("%s", message)
	}
	if !namePattern.MatchString(b.Name) {
		return bad("runner name must be a DNS hostname of 1-63 characters")
	}
	if strings.IndexFunc(b.Name, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return bad("numeric-only runner names are reserved for Timeweb server IDs")
	}
	if !identityPattern.MatchString(b.PoolID) {
		return bad("invalid pool ID")
	}
	if b.OSType != params.Linux || b.OSArch != params.Amd64 {
		return bad("this release supports Linux amd64 runners")
	}
	preset, err := positiveID(b.Flavor)
	if err != nil {
		return bad("flavor must be a positive Timeweb preset ID")
	}
	request.PresetID = preset
	kind, imageID, ok := strings.Cut(b.Image, ":")
	if !ok {
		return bad("image must be os:<positive OS ID> or image:<UUID>")
	}
	switch kind {
	case "os":
		request.OSID, err = positiveID(imageID)
		if err != nil {
			return bad("image OS ID must be a positive integer")
		}
	case "image":
		if !uuidPattern.MatchString(imageID) {
			return bad("custom image ID must be a UUID")
		}
		request.ImageID = imageID
	default:
		return bad("image must be os:<positive OS ID> or image:<UUID>")
	}
	if len(b.ExtraSpecs) != 0 {
		var extras extraSpecs
		decoder := json.NewDecoder(strings.NewReader(string(b.ExtraSpecs)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&extras); err != nil || strings.TrimSpace(string(b.ExtraSpecs)) == "null" {
			return bad("extra_specs must be an object with supported Timeweb or GARM cloud-init fields")
		}
		if extras.NetworkID != nil {
			cfg.NetworkID = *extras.NetworkID
		}
		if extras.NetworkMode != nil {
			cfg.NetworkMode = *extras.NetworkMode
		}
		if extras.AvailabilityZone != nil {
			cfg.AvailabilityZone = *extras.AvailabilityZone
		}
		if extras.ProjectID != nil {
			cfg.ProjectID = *extras.ProjectID
		}
		if extras.SSHKeyIDs != nil {
			cfg.SSHKeyIDs = *extras.SSHKeyIDs
		}
		if err := cfg.Validate(); err != nil {
			return bad("invalid Timeweb configuration in extra_specs: " + err.Error())
		}
	}
	tools, err := util.GetTools(b.OSType, b.OSArch, b.Tools)
	if err != nil {
		return bad("no matching Linux amd64 runner download in bootstrap tools")
	}
	userData, err := cloudconfig.GetCloudConfig(b, tools, b.Name)
	if err != nil {
		return bad("cannot generate cloud-init; check bootstrap fields and runner template")
	}
	comment, err := p.marker(b.PoolID)
	if err != nil {
		return cfg, request, err
	}
	request.Name, request.Hostname, request.Comment = b.Name, b.Name, comment
	request.CloudInit = userData
	request.Network = &timeweb.CreateServerNetwork{ID: cfg.NetworkID}
	request.AvailabilityZone, request.ProjectID, request.SSHKeysIDs = cfg.AvailabilityZone, cfg.ProjectID, cfg.SSHKeyIDs
	return cfg, request, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func positiveID(value string) (int64, error) {
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("not a decimal ID")
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("not a positive ID")
	}
	return id, nil
}
