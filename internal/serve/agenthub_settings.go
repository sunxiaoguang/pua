package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
)

type agentHubSettingsResponse struct {
	Mode               string                 `json:"mode"`
	Config             agentHubServeConfig    `json:"config"`
	AgentConfig        agentHubSettingsConfig `json:"agentConfig"`
	ConfiguredEndpoint string                 `json:"configuredEndpoint"`
	EffectiveEndpoint  string                 `json:"effectiveEndpoint"`
	Connected          bool                   `json:"connected"`
	Compatible         bool                   `json:"compatible"`
	Status             *agentHubStatus        `json:"status,omitempty"`
	Catalog            agentHubCatalog        `json:"catalog"`
	Revision           string                 `json:"revision,omitempty"`
	Error              string                 `json:"error,omitempty"`
}

type agentHubSettingsConfig struct {
	AgentProviders []agentHubConfiguredProvider `json:"providers"`
	Agents         []agentHubConfiguredAgent    `json:"agents"`
}

type updateAgentHubSettingsRequest struct {
	Endpoint       string                       `json:"endpoint"`
	AgentProfiles  []agentHubProfileRoute       `json:"agentProfiles"`
	AgentProviders []agentHubConfiguredProvider `json:"agentProviders"`
	Agents         []agentHubConfiguredAgent    `json:"agents"`
}

func (s *server) handleAgentHubSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		response, err := s.readAgentHubSettings(r.Context())
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, response)
	case http.MethodPut:
		var request updateAgentHubSettingsRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		response, err := s.saveAgentHubSettings(r.Context(), request)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, response)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) readAgentHubSettings(ctx context.Context) (agentHubSettingsResponse, error) {
	cfg, configChanged, err := s.mutateAgentHubConfig(func(current *agentHubServeConfig) (bool, error) {
		structuralProfiles, normalizeErr := normalizeAgentHubProfileRoutes(current.AgentProfiles, agentHubCatalog{})
		if normalizeErr != nil {
			return false, normalizeErr
		}
		if reflect.DeepEqual(current.AgentProfiles, structuralProfiles) {
			return false, nil
		}
		current.AgentProfiles = structuralProfiles
		return true, nil
	})
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	configured := cfg.AgentHubEndpoint
	if configured == "" {
		configured = defaultAgentHubEndpoint
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if s.agentHubMode != "" {
		configured = effective
		cfg.AgentHubEndpoint = effective
	}
	response := agentHubSettingsResponse{
		Mode:               s.agentHubMode,
		Config:             cfg,
		ConfiguredEndpoint: configured,
		EffectiveEndpoint:  effective,
		Catalog:            agentHubCatalog{Providers: []agentHubProvider{}, Agents: []agentHubAgent{}, Probes: []agentHubProbe{}},
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	status, err := client.Status(ctx)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.Connected = true
	response.Status = &status
	if err := validateAgentHubStatus(status); err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.Compatible = true
	catalog, err := client.Agents(ctx)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.Catalog = catalog
	configuredAgentHub, err := client.Config(ctx)
	if err != nil {
		response.Error = err.Error()
		return response, nil
	}
	response.AgentConfig = projectAgentHubSettingsConfig(configuredAgentHub)
	cfg, normalizedChanged, err := s.mutateAgentHubConfig(func(current *agentHubServeConfig) (bool, error) {
		if s.agentHubMode != "" {
			current.AgentHubEndpoint = effective
		}
		normalized, normalizeErr := normalizeAgentHubConfig(*current, catalog)
		if normalizeErr != nil {
			return false, normalizeErr
		}
		if reflect.DeepEqual(*current, normalized) {
			return false, nil
		}
		*current = normalized
		return true, nil
	})
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	configChanged = configChanged || normalizedChanged
	if configChanged {
		s.requestSchedulerReconcileForOwnedWorkspaces(cfg.Workspaces)
	}
	response.Config = cfg
	response.Revision = s.settingsRevisionOrEmpty()
	return response, nil
}

func (s *server) saveAgentHubSettings(ctx context.Context, request updateAgentHubSettingsRequest) (agentHubSettingsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	configured, err := normalizeAgentHubEndpoint(request.Endpoint)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if s.agentHubMode != "" {
		configured = effective
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return agentHubSettingsResponse{}, fmt.Errorf("validate AgentHub status: %w", err)
	}
	if err := validateAgentHubStatus(status); err != nil {
		return agentHubSettingsResponse{}, err
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return agentHubSettingsResponse{}, fmt.Errorf("validate AgentHub catalog: %w", err)
	}
	var configuredAgentHub agentHubConfiguredConfig
	agentHubConfigChanged := false
	remoteMutationStarted := false
	if request.AgentProviders != nil || request.Agents != nil {
		configuredAgentHub, err = client.Config(ctx)
		if err != nil {
			return agentHubSettingsResponse{}, fmt.Errorf("read AgentHub config: %w", err)
		}
		persistedAgentHub := configuredAgentHub
		if request.AgentProviders != nil {
			configuredAgentHub.AgentProviders = request.AgentProviders
		}
		if request.Agents != nil {
			configuredAgentHub.Agents = request.Agents
		}
		if !reflect.DeepEqual(persistedAgentHub, configuredAgentHub) {
			mutationContext, cancelMutation, contextErr := agentHubSettingsMutationContext(ctx)
			if contextErr != nil {
				return agentHubSettingsResponse{}, contextErr
			}
			remoteMutationStarted = true
			configuredAgentHub, err = client.SaveConfig(mutationContext, configuredAgentHub)
			cancelMutation()
			if err != nil {
				return agentHubSettingsResponse{}, fmt.Errorf("save AgentHub config: %w", err)
			}
			agentHubConfigChanged = !reflect.DeepEqual(persistedAgentHub, configuredAgentHub)
		}
	}
	if !remoteMutationStarted && ctx.Err() != nil {
		return agentHubSettingsResponse{}, ctx.Err()
	}
	cfg, configChanged, err := s.mutateAgentHubConfig(func(current *agentHubServeConfig) (bool, error) {
		before := *current
		current.AgentHubEndpoint = configured
		current.AgentProfiles = request.AgentProfiles
		normalized, normalizeErr := normalizeAgentHubConfig(*current, catalog)
		if normalizeErr != nil {
			return false, normalizeErr
		}
		*current = normalized
		return !reflect.DeepEqual(before, normalized), nil
	})
	if err != nil {
		return agentHubSettingsResponse{}, err
	}
	if agentHubConfigChanged || configChanged {
		s.requestSchedulerReconcileForOwnedWorkspaces(cfg.Workspaces)
	}
	return agentHubSettingsResponse{
		Mode:               s.agentHubMode,
		Config:             cfg,
		AgentConfig:        projectAgentHubSettingsConfig(configuredAgentHub),
		ConfiguredEndpoint: configured,
		EffectiveEndpoint:  effective,
		Connected:          true,
		Compatible:         true,
		Status:             &status,
		Catalog:            catalog,
		Revision:           s.settingsRevisionOrEmpty(),
	}, nil
}

func projectAgentHubSettingsConfig(config agentHubConfiguredConfig) agentHubSettingsConfig {
	return agentHubSettingsConfig{
		AgentProviders: config.AgentProviders,
		Agents:         config.Agents,
	}
}

func (s *server) handleAgentHubProviderSettings(w http.ResponseWriter, r *http.Request, providerID string) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Enabled == nil {
		if err == nil {
			err = fmt.Errorf("enabled is required")
		}
		writeError(w, err, http.StatusBadRequest)
		return
	}
	cfg, err := s.readAgentHubConfig()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	configured := cfg.AgentHubEndpoint
	if configured == "" {
		configured = defaultAgentHubEndpoint
	}
	effective, err := s.effectiveAgentHubEndpoint(configured)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	status, err := client.Status(r.Context())
	if err != nil {
		writeError(w, fmt.Errorf("validate AgentHub status: %w", err), http.StatusBadRequest)
		return
	}
	if err := validateAgentHubStatus(status); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	configuredAgentHub, err := client.Config(r.Context())
	if err != nil {
		writeError(w, fmt.Errorf("read AgentHub config: %w", err), http.StatusBadRequest)
		return
	}
	provider := agentHubConfiguredProvider{}
	changed := true
	for _, configuredProvider := range configuredAgentHub.AgentProviders {
		if strings.TrimSpace(configuredProvider.ID) == strings.TrimSpace(providerID) {
			provider = configuredProvider
			changed = configuredProvider.Enabled != *request.Enabled
			break
		}
	}
	if changed {
		persistedProvider := provider
		mutationContext, cancelMutation, contextErr := agentHubSettingsMutationContext(r.Context())
		if contextErr != nil {
			writeError(w, contextErr, http.StatusBadRequest)
			return
		}
		provider, err = client.SetProviderEnabled(mutationContext, providerID, *request.Enabled)
		cancelMutation()
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if !reflect.DeepEqual(persistedProvider, provider) {
			s.requestSchedulerReconcileForOwnedWorkspaces(cfg.Workspaces)
		}
	}
	writeJSON(w, struct {
		Provider agentHubConfiguredProvider `json:"provider"`
	}{Provider: provider})
}

// agentHubSettingsMutationContext makes the remote commit boundary explicit.
// Cancellation before this helper runs prevents the mutation; after it starts,
// the bounded AgentHub request can confirm its durable result independently of
// an HTTP client disconnect.
func agentHubSettingsMutationContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	mutationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentHubRequestTimeout)
	return mutationContext, cancel, nil
}

// requestSchedulerReconcileForOwnedWorkspaces keeps a global settings change
// quiet when this Server no longer owns any Workspace that could consume it.
func (s *server) requestSchedulerReconcileForOwnedWorkspaces(workspaces []serveWorkspace) {
	if s == nil || s.agents == nil {
		return
	}
	for _, workspace := range workspaces {
		if s.ownsWorkspace(workspace.Path) {
			s.agents.requestReconcile(reconcileScheduler)
			return
		}
	}
}

// settingsRevisionOrEmpty best-effort computes the settings revision after a
// settings read or save; a revision failure must not fail the settings flow
// itself, so the frontend simply falls back to polling.
func (s *server) settingsRevisionOrEmpty() string {
	revision, err := s.currentSettingsRevision()
	if err != nil {
		return ""
	}
	return revision
}

func agentHubConfigFromServeConfig(cfg config) agentHubServeConfig {
	profiles := make([]agentHubProfileRoute, 0, len(cfg.AgentProfiles))
	for _, profile := range cfg.AgentProfiles {
		profiles = append(profiles, agentHubProfileRoute{
			Key: profile.Key, Description: profile.Description, AgentName: profile.AgentName,
		})
	}
	return agentHubServeConfig{
		Version: cfg.Version, ActiveID: cfg.ActiveID, Workspaces: cfg.Workspaces,
		AgentHubEndpoint: cfg.AgentHubEndpoint, AgentHubInstanceID: cfg.AgentHubInstanceID,
		AgentProfiles: profiles,
	}
}

func applyAgentHubFieldsToServeConfig(dst *config, source agentHubServeConfig) {
	profiles := make([]agentProfileRoute, 0, len(source.AgentProfiles))
	for _, profile := range source.AgentProfiles {
		profiles = append(profiles, agentProfileRoute{
			Key: profile.Key, Description: profile.Description, AgentName: profile.AgentName,
		})
	}
	dst.Version = source.Version
	dst.AgentHubEndpoint = source.AgentHubEndpoint
	dst.AgentHubInstanceID = source.AgentHubInstanceID
	dst.AgentProfiles = profiles
}

func (s *server) readAgentHubConfig() (agentHubServeConfig, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return agentHubServeConfig{}, err
	}
	return agentHubConfigFromServeConfig(cfg), nil
}

// mutateAgentHubConfig uses the same serialized serve-config transaction as
// Workspace list mutations. The callback receives the latest complete config,
// so AgentHub field updates cannot overwrite a concurrent Workspace add or
// removal.
func (s *server) mutateAgentHubConfig(mutate func(*agentHubServeConfig) (bool, error)) (agentHubServeConfig, bool, error) {
	updated, changed, err := s.mutateConfig(func(current *config) (bool, error) {
		agentHub := agentHubConfigFromServeConfig(*current)
		changed, mutateErr := mutate(&agentHub)
		if mutateErr != nil || !changed {
			return changed, mutateErr
		}
		applyAgentHubFieldsToServeConfig(current, agentHub)
		return true, nil
	})
	if err != nil {
		return agentHubServeConfig{}, false, err
	}
	return agentHubConfigFromServeConfig(updated), changed, nil
}

func writeAgentHubConfigFile(path string, cfg agentHubServeConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteConfig(path, append(data, '\n'))
}

func readAgentHubConfigFile(path string) (agentHubServeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return agentHubServeConfig{
				Version: agentHubConfigVersion, Workspaces: []serveWorkspace{},
				AgentHubEndpoint: defaultAgentHubEndpoint,
			}, nil
		}
		return agentHubServeConfig{}, err
	}
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return agentHubServeConfig{}, err
	}
	if version.Version < 3 {
		return agentHubServeConfig{}, fmt.Errorf("unsupported PUA serve configuration version %d; migrate the configuration before starting pua serve", version.Version)
	}
	needsUpgrade := version.Version != agentHubConfigVersion
	var cfg agentHubServeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return agentHubServeConfig{}, err
	}
	cfg.Version = agentHubConfigVersion
	if needsUpgrade {
		if err := writeAgentHubConfigFile(path, cfg); err != nil {
			return agentHubServeConfig{}, err
		}
	}
	return cfg, nil
}

func (s *server) validatePersistedAgentHubConfig(ctx context.Context) (bool, error) {
	cfg, err := s.readAgentHubConfig()
	if err != nil {
		return false, err
	}
	if cfg.AgentHubInstanceID == "" {
		return false, nil
	}
	effective, err := s.effectiveAgentHubEndpoint(cfg.AgentHubEndpoint)
	if err != nil {
		return true, err
	}
	client, err := newAgentHubClient(effective, nil)
	if err != nil {
		return true, err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return true, fmt.Errorf("connect to AgentHub: %w", err)
	}
	if err := validateAgentHubStatus(status); err != nil {
		return true, err
	}
	catalog, err := client.Agents(ctx)
	if err != nil {
		return true, err
	}
	_, _, err = s.mutateAgentHubConfig(func(current *agentHubServeConfig) (bool, error) {
		normalized, normalizeErr := normalizeAgentHubConfig(*current, catalog)
		if normalizeErr != nil {
			return false, normalizeErr
		}
		if reflect.DeepEqual(*current, normalized) {
			return false, nil
		}
		*current = normalized
		return true, nil
	})
	if err != nil {
		return true, err
	}
	return true, nil
}
