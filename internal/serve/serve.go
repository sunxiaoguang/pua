package serve

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	urlpath "path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	agenthubapp "github.com/disksing/pua/agenthub/app"
	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/buildinfo"
	"github.com/disksing/pua/internal/workspacepath"
	"github.com/disksing/pua/web"
)

var staticFiles = web.Assets

type config struct {
	Version            int                 `json:"version"`
	ActiveID           string              `json:"activeId,omitempty"`
	Workspaces         []serveWorkspace    `json:"workspaces"`
	AgentHubEndpoint   string              `json:"agentHubEndpoint,omitempty"`
	AgentHubInstanceID string              `json:"agentHubInstanceId,omitempty"`
	AgentProfiles      []agentProfileRoute `json:"agentProfiles,omitempty"`
}

type agentProfileRoute struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
	AgentName   string `json:"agentName,omitempty"`
}

type serveWorkspace struct {
	ID         string `json:"id"`
	InstanceID string `json:"instanceId,omitempty"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Icon       string `json:"icon,omitempty"`
}

// workspacesResponse is the GET /api/workspaces payload: the persisted serve
// configuration plus a revision marker the frontend polls to detect settings
// changes made by other clients.
type workspacesResponse struct {
	config
	Revision          string `json:"revision"`
	SuggestedUserName string `json:"suggestedUserName,omitempty"`
}

var workspaceIconFiles = map[string]string{
	"home-base":               "01-home-base.png",
	"personal-tasks":          "02-personal-tasks.png",
	"product-roadmap":         "03-product-roadmap.png",
	"software-engineering":    "04-software-engineering.png",
	"design-studio":           "05-design-studio.png",
	"marketing-campaign":      "06-marketing-campaign.png",
	"sales-pipeline":          "07-sales-pipeline.png",
	"operations":              "08-operations.png",
	"finance":                 "09-finance.png",
	"research-lab":            "10-research-lab.png",
	"learning-education":      "11-learning-education.png",
	"customer-support":        "12-customer-support.png",
	"events-calendar":         "13-events-calendar.png",
	"documentation-knowledge": "14-documentation-knowledge.png",
	"analytics":               "15-analytics.png",
	"community-team":          "16-community-team.png",
}

type workspaceTree struct {
	Root                string                    `json:"root"`
	AgentBinding        app.AgentBinding          `json:"agentBinding"`
	ResourceDefaults    app.ResourceAgentDefaults `json:"resourceDefaults"`
	GenerationPolicy    app.GenerationPolicy      `json:"generationPolicy"`
	StallWatchdogPolicy app.StallWatchdogPolicy   `json:"stallWatchdogPolicy"`
	Workspace           resourceSnapshot          `json:"workspace"`
	Scheduler           resourceSnapshot          `json:"scheduler"`
	Projects            []resourceSnapshot        `json:"projects"`
	Activity            resourceActivityLists     `json:"activity"`
	Wiki                workspaceWiki             `json:"wiki"`
}

type workspaceWiki struct {
	Exists  bool            `json:"exists"`
	Entries []fileTreeEntry `json:"entries"`
	Error   string          `json:"error,omitempty"`
}

type fileTreeEntry struct {
	Name     string          `json:"name"`
	Path     string          `json:"path"`
	Type     string          `json:"type"`
	Size     int64           `json:"size,omitempty"`
	Modified string          `json:"modified,omitempty"`
	Children []fileTreeEntry `json:"children,omitempty"`
}

type resourceSnapshot struct {
	ID               string                     `json:"id"`
	Type             string                     `json:"type"`
	Title            string                     `json:"title"`
	Path             string                     `json:"path"`
	Archived         bool                       `json:"archived"`
	AgentBinding     app.AgentBinding           `json:"agentBinding"`
	State            app.TaskState              `json:"state,omitempty"`
	StateNote        string                     `json:"stateNote,omitempty"`
	StateUpdatedAt   string                     `json:"stateUpdatedAt,omitempty"`
	Runtime          *resourceRuntimeSnapshot   `json:"runtime,omitempty"`
	UserState        *resourceUserStateSnapshot `json:"userState,omitempty"`
	LatestTurnNumber int                        `json:"latestTurnNumber,omitempty"`
	LatestTurnAt     string                     `json:"latestTurnAt,omitempty"`
	LatestAgentName  string                     `json:"latestAgentName,omitempty"`
	UnreadCount      int                        `json:"unreadCount,omitempty"`
	Children         []resourceSnapshot         `json:"children,omitempty"`
}

type filePreview struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Truncated   bool   `json:"truncated"`
	Binary      bool   `json:"binary"`
	Image       bool   `json:"image"`
	MimeType    string `json:"mimeType,omitempty"`
	Content     string `json:"content,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
}

type diffResponse struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Branch     string `json:"branch"`
	Base       string `json:"base,omitempty"`
	Diff       string `json:"diff"`
	HasChanges bool   `json:"hasChanges"`
}

// uiStateFolder is a virtual sidebar folder. Folders are a pure UI-layer
// grouping device: they only nest Tasks visually inside their Project and
// never affect the real resource directories on disk.
type uiStateFolder struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Expanded  bool   `json:"expanded"`
}

type uiState struct {
	Version          int                               `json:"version"`
	ExpandedProjects []string                          `json:"expandedProjects"`
	LastResourceID   string                            `json:"lastResourceId,omitempty"`
	ProjectOrder     []string                          `json:"projectOrder,omitempty"`
	TaskOrder        map[string][]string               `json:"taskOrder,omitempty"`
	Folders          []uiStateFolder                   `json:"folders,omitempty"`
	FolderOrder      map[string][]string               `json:"folderOrder,omitempty"`
	ResourceStates   map[string]resourceUserState      `json:"resourceStates,omitempty"`
	Attention        map[string]resourceAttentionState `json:"attention,omitempty"`
}

type server struct {
	addr             string
	config           string
	agentHubMode     string
	agentHubEndpoint string
	agents           *agentManager
	doctor           *doctorMonitor
	locks            *workspaceLockManager
	// configMu serializes the commit boundary of every serve-config mutation.
	// Workspace mutations acquire their handoff barrier before this mutex; no
	// config transaction may enter a Workspace controller or perform remote
	// AgentHub work while holding it.
	configMu  sync.Mutex
	uiStateMu sync.Mutex
}

const (
	previewMaxBytes      = 512 * 1024
	diffMaxBytes         = 4 * 1024 * 1024
	agentHubModeEmbedded = "embedded"
	agentHubModeExternal = "external"
)

const serveUsage = `usage: pua serve [--addr=<address>] [--workspace=<path>]
                 [--no-default-workspace]
                 [--agenthub-mode=embedded|external]
                 [--agenthub-endpoint=<url>] [--version]

Start the PUA web service: Workspace API, AgentHub session orchestration and
recovery, and the static web UI.
The service uses the in-process application API rooted at each explicit
Workspace path; it does not invoke the pua CLI as a child process.

Options:
  --addr <address>       local address to listen on (default 127.0.0.1:4936)
  --workspace <path>     AgentWorkspace path to add before starting
  --no-default-workspace do not add the current directory to an empty config
  --agenthub-mode <mode> AgentHub mode: embedded (default) or external
  --agenthub-endpoint    external AgentHub base URL ending in /agenthub;
                         required when --agenthub-mode=external
  --version              print build-time branch and sha

Workspace ownership:
  Each managed Workspace is exclusively owned by one pua serve process via
  an OS advisory lock in the Workspace control directory (.pua). A second
  instance using a different PUA_SERVE_CONFIG cannot manage
  the same Workspace; it
  fails at startup before session recovery begins. The OS releases the
  lock automatically when the owning process exits.

Embedded AgentHub:
  The AgentHub Web UI and API share this server's listener at /agenthub/ and
  /agenthub/v1/. The same network exposure and trust boundary applies to both.

Environment:
  PUA_SERVE_CONFIG      serve configuration file path (default ~/.pua/serve.json)
`

// PrintHelp writes the pua serve usage text to stdout.
func PrintHelp() {
	fmt.Print(serveUsage)
}

// Main runs the pua serve subcommand.
func Main(args []string) error {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			fmt.Print(serveUsage)
			return nil
		}
	}
	flags := flag.NewFlagSet("pua serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var addr string
	var initialWorkspace string
	var noDefaultWorkspace bool
	var agentHubMode string
	var agentHubEndpoint string
	var showVersion bool
	flags.StringVar(&addr, "addr", "127.0.0.1:4936", "local address to listen on")
	flags.StringVar(&initialWorkspace, "workspace", "", "AgentWorkspace path to add before starting")
	flags.BoolVar(&noDefaultWorkspace, "no-default-workspace", false, "do not add the current directory to an empty config")
	flags.StringVar(&agentHubMode, "agenthub-mode", agentHubModeEmbedded, "AgentHub mode: embedded or external")
	flags.StringVar(&agentHubEndpoint, "agenthub-endpoint", "", "external AgentHub base URL ending in /agenthub")
	flags.BoolVar(&showVersion, "version", false, "print build-time branch and sha")
	if err := flags.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, serveUsage)
		return err
	}
	if flags.NArg() != 0 {
		fmt.Fprint(os.Stderr, serveUsage)
		return fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if showVersion {
		fmt.Print(buildinfo.Text("pua"))
		return nil
	}
	agentHubMode = strings.ToLower(strings.TrimSpace(agentHubMode))
	switch agentHubMode {
	case agentHubModeEmbedded:
		if strings.TrimSpace(agentHubEndpoint) != "" {
			return errors.New("--agenthub-endpoint is only valid with --agenthub-mode=external")
		}
	case agentHubModeExternal:
		if strings.TrimSpace(agentHubEndpoint) == "" {
			return errors.New("--agenthub-endpoint is required with --agenthub-mode=external")
		}
		normalized, err := normalizeAgentHubEndpoint(agentHubEndpoint)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(normalized, agenthubapp.BasePath) {
			return fmt.Errorf("--agenthub-endpoint must end in %s", agenthubapp.BasePath)
		}
		agentHubEndpoint = normalized
	default:
		return fmt.Errorf("invalid --agenthub-mode %q: expected embedded or external", agentHubMode)
	}
	signalContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	configPath, err := defaultConfigPath()
	if err != nil {
		return err
	}
	configLock, err := acquireServeConfigLock(configPath, addr)
	if err != nil {
		return err
	}
	defer configLock.Close()
	var agentHubService *agenthubapp.Service
	if agentHubMode == agentHubModeEmbedded {
		info := buildinfo.Current()
		agentHubService, err = agenthubapp.New(agenthubapp.Options{Address: addr, Version: info.SHA})
		if err != nil {
			return fmt.Errorf("initialize embedded AgentHub: %w", err)
		}
		defer agentHubService.Close()
		agentHubEndpoint = agentHubService.Endpoint()
	}
	s := &server{
		addr: addr, config: configPath, agentHubMode: agentHubMode, agentHubEndpoint: agentHubEndpoint,
		locks: newWorkspaceLockManager(addr, configPath),
	}
	defer s.locks.closeAll()
	s.agents = newAgentManager(s)
	if initialWorkspace != "" {
		if _, err := s.addWorkspace(signalContext, initialWorkspace); err != nil {
			return fmt.Errorf("add initial workspace: %w", err)
		}
	} else if !noDefaultWorkspace {
		s.addCurrentDirectoryIfEmpty(signalContext)
	}
	// Every configured Workspace must be owned before AgentHub recovery or any
	// writable HTTP endpoint may touch it.
	if err := s.acquireConfiguredWorkspaceLocks(); err != nil {
		return err
	}
	if err := s.backfillConfiguredWorkspaceInstanceIDs(); err != nil {
		return fmt.Errorf("backfill configured Workspace instance ids: %w", err)
	}
	if err := s.ensureConfiguredResourceRuntimes(); err != nil {
		return err
	}
	puaHandler, err := s.httpHandler()
	if err != nil {
		return err
	}
	ready := &readinessHandler{next: puaHandler}
	rootMux := http.NewServeMux()
	if agentHubService != nil {
		rootMux.Handle(agenthubapp.BasePath, agentHubService.Handler())
		rootMux.Handle(agenthubapp.BasePath+"/", agentHubService.Handler())
	}
	rootMux.Handle("/", ready)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	httpServer := &http.Server{Handler: rootMux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 75 * time.Second}
	serverErrors := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErrors <- err
	}()
	if agentHubService != nil {
		if err := agentHubService.Activate(); err != nil {
			_ = httpServer.Close()
			return fmt.Errorf("publish embedded AgentHub endpoint: %w", err)
		}
	}
	handshakeContext, cancelHandshake := context.WithTimeout(signalContext, agentHubRequestTimeout)
	agentHubSettings, err := s.readAgentHubSettings(handshakeContext)
	cancelHandshake()
	if err != nil {
		_ = httpServer.Close()
		return fmt.Errorf("validate AgentHub configuration: %w", err)
	}
	if !agentHubSettings.Connected || !agentHubSettings.Compatible {
		_ = httpServer.Close()
		return fmt.Errorf("validate AgentHub configuration: %s", agentHubSettings.Error)
	}
	lifecycleContext, cancelLifecycle := context.WithCancel(signalContext)
	defer func() {
		ready.ready.Store(false)
		cancelLifecycle()
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		shutdownDone := make(chan error, 1)
		go func() { shutdownDone <- httpServer.Shutdown(shutdownContext) }()
		done := make(chan struct{})
		go func() {
			s.agents.waitBackground()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if agentHubService != nil {
			_ = agentHubService.Close()
		}
		if err := <-shutdownDone; err != nil {
			_ = httpServer.Close()
		}
	}()
	s.agents.startAgentRecovery(lifecycleContext)
	s.doctor = newDoctorMonitor(s)
	s.doctor.start(lifecycleContext)
	ready.ready.Store(true)

	log.Printf("pua serve listening on http://%s (AgentHub %s at %s)", addr, agentHubMode, agentHubEndpoint)
	if agentHubService != nil && agentHubService.Exposed() {
		log.Printf("WARNING: PUA and AgentHub are both reachable from the network at %s; neither service provides authentication", addr)
	}
	select {
	case err := <-serverErrors:
		return err
	case <-signalContext.Done():
		return nil
	}
}

type readinessHandler struct {
	ready atomic.Bool
	next  http.Handler
}

func (h *readinessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.ready.Load() {
		http.Error(w, "PUA is starting", http.StatusServiceUnavailable)
		return
	}
	h.next.ServeHTTP(w, r)
}

func (s *server) httpHandler() (http.Handler, error) {
	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveStatic(staticRoot, w, r)
	})
	mux.HandleFunc("/api/workspaces", s.handleWorkspaces)
	mux.HandleFunc("/api/workspaces/", s.handleWorkspace)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/settings/", s.handleSettings)
	mux.HandleFunc("/api/doctor", s.handleDoctor)
	return mux, nil
}

func (s *server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := s.loadConfig()
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
		cfg.Workspaces = resolvedWorkspaceSummaries(cfg.Workspaces)
		writeJSON(w, workspacesResponse{config: cfg, Revision: settingsRevision(cfg, cfg.Workspaces), SuggestedUserName: suggestedSystemUserName()})
	case http.MethodPost:
		var body struct {
			Path            string `json:"path"`
			Create          bool   `json:"create"`
			Language        string `json:"language"`
			InitialUserName string `json:"initialUserName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		workspace, err := s.addWorkspaceWithOptions(r.Context(), body.Path, body.Create, body.Language, body.InitialUserName)
		if err != nil {
			var conflict *workspaceLockConflictError
			if errors.As(err, &conflict) {
				writeError(w, err, http.StatusConflict)
				return
			}
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, workspace)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/workspaces/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, errors.New("workspace id is required"), http.StatusBadRequest)
		return
	}
	id := parts[0]
	if len(parts) == 1 {
		if r.Method == http.MethodPut {
			var body struct {
				Icon *string `json:"icon"`
				Name *string `json:"name"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			if body.Icon == nil && body.Name == nil {
				writeError(w, errors.New("icon or name is required"), http.StatusBadRequest)
				return
			}
			var workspace serveWorkspace
			var err error
			if body.Icon != nil {
				workspace, err = s.updateWorkspaceIcon(id, *body.Icon)
				if err != nil {
					writeError(w, err, http.StatusBadRequest)
					return
				}
			}
			if body.Name != nil {
				workspace, err = s.updateWorkspaceName(id, *body.Name)
				if err != nil {
					writeError(w, err, http.StatusBadRequest)
					return
				}
			}
			writeJSON(w, workspace)
			return
		}
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.removeWorkspace(id); err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch parts[1] {
	case "defaults":
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.updateWorkspaceDefaults(w, r, id)
		return
	case "generation-policy":
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.updateWorkspaceGenerationPolicy(w, r, id)
		return
	case "stall-watchdog-policy":
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.updateWorkspaceStallWatchdogPolicy(w, r, id)
		return
	case "tree":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		workspace, err := s.workspace(id)
		if err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		userName, err := s.workspaceUserName(r, workspace.Path)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		tree, err := s.tree(r.Context(), id, userName)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, tree)
	case "resources":
		if len(parts) == 4 && parts[3] == "documents" {
			s.saveResourceMarkdownFile(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "artifacts" {
			s.deleteResourceArtifact(w, r, id, parts[2])
			return
		}
		if len(parts) >= 5 && parts[3] == "history" {
			s.agents.handleResourceHistory(w, r, id, parts[2], parts[4:])
			return
		}
		if len(parts) == 4 && parts[3] == "events" {
			s.agents.handleResourceEvents(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "stream" {
			s.agents.handleResourceStream(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "approval" {
			s.agents.handleResourceApproval(w, r, id, parts[2])
			return
		}
		if len(parts) == 5 && parts[3] == "turn" && parts[4] == "end" {
			s.agents.handleResourceEndTurn(w, r, id, parts[2])
			return
		}
		if len(parts) == 5 && parts[3] == "generation" && parts[4] == "end" {
			s.agents.handleResourceEndGeneration(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "uploads" {
			s.agents.handleResourceUpload(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "status" {
			s.agents.handleResourceStatus(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "task-state" {
			s.agents.handleTaskState(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "read" {
			s.handleResourceRead(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "messages" {
			s.agents.handleResourceMessages(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "agent-binding" {
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			s.updateResourceAgentBinding(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "title" {
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			s.updateResourceTitle(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "description" {
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			s.updateResourceDescription(w, r, id, parts[2])
			return
		}
		if len(parts) == 4 && parts[3] == "task-default" {
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			s.updateProjectTaskDefault(w, r, id, parts[2])
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if len(parts) != 3 || parts[2] == "" {
			writeError(w, errors.New("resource id is required"), http.StatusBadRequest)
			return
		}
		detail, err := s.resource(r.Context(), id, parts[2])
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, schedulerResourceDetailAPIResponse(detail))
	case "scheduler":
		s.handleScheduler(w, r, id, parts[2:])
	case "users":
		s.handleUsers(w, r, id, parts[2:])
	case "messages":
		if (len(parts) != 3 && len(parts) != 4) || parts[2] == "" {
			writeError(w, &resourceAPIError{Code: "invalid_request", Message: "message id is required"}, http.StatusBadRequest)
			return
		}
		if len(parts) == 4 {
			if parts[3] != "steer" {
				http.NotFound(w, r)
				return
			}
			s.agents.handleResourceMessageSteer(w, r, id, parts[2])
			return
		}
		s.agents.handleResourceMessage(w, r, id, parts[2])
	case "files":
		if len(parts) == 3 && parts[2] == "raw" {
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			s.serveRawFile(w, r, id)
			return
		}
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			s.previewFile(w, r, id)
		case http.MethodPut:
			s.saveWorkspaceAgentsFile(w, r, id)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case "diff":
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.worktreeDiff(w, r, id)
	case "ui-state":
		s.handleUIState(w, r, id)
	case "projects":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.createProject(w, r, id)
	case "tasks":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if len(parts) == 3 && parts[2] == "preview" {
			s.previewTask(w, r, id)
			return
		}
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		s.createTask(w, r, id)
	case "templates":
		s.handleTemplates(w, r, id, parts[2:])
	case "archive":
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.archiveResource(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// validateProfileBinding rejects Profile bindings that have no configured
// route, mirroring the agent-binding endpoint.
func (s *server) validateProfileBinding(binding app.AgentBinding) error {
	if binding.Kind != "profile" {
		return nil
	}
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	if configuredAgentProfileName(cfg.AgentProfiles, binding.Name) == "" {
		return fmt.Errorf("unknown or unconfigured Agent Profile %q", binding.Name)
	}
	return nil
}

// revalidateWorkspaceMutation confirms that the Workspace resolved before a
// mutation was queued is still the same configured Workspace while the
// caller owns the shared handoff lease. Removal cannot change the
// configuration or release the advisory lock until that lease is released.
func (s *server) revalidateWorkspaceMutation(expected serveWorkspace) (serveWorkspace, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return serveWorkspace{}, err
	}
	expectedPath, err := canonicalWorkspacePath(expected.Path)
	if err != nil {
		return serveWorkspace{}, err
	}
	for _, current := range cfg.Workspaces {
		if current.ID != expected.ID {
			continue
		}
		currentPath, pathErr := canonicalWorkspacePath(current.Path)
		if pathErr != nil || currentPath != expectedPath ||
			(strings.TrimSpace(expected.InstanceID) != "" && strings.TrimSpace(current.InstanceID) != strings.TrimSpace(expected.InstanceID)) {
			return serveWorkspace{}, &resourceAPIError{
				Code:    "workspace_not_owned",
				Message: fmt.Sprintf("workspace %s changed while the mutation was waiting for ownership", expected.ID),
			}
		}
		if s.locks == nil && strings.TrimSpace(current.InstanceID) == "" {
			if err := s.requireWorkspaceOwnership(current.Path); err != nil {
				return serveWorkspace{}, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}
			}
			return current, nil
		}
		if _, err := s.requireWorkspaceInstanceOwnership(current); err != nil {
			return serveWorkspace{}, err
		}
		return current, nil
	}
	return serveWorkspace{}, &resourceAPIError{
		Code:    "workspace_not_owned",
		Message: fmt.Sprintf("workspace %s is no longer configured by this pua serve instance", expected.ID),
	}
}

// withWorkspaceMutation enrolls Server-managed portable and runtime writes in
// the same Workspace-wide handoff barrier used by resource controllers. The
// resource controller also preserves existing per-resource serialization.
func (s *server) withWorkspaceMutation(ctx context.Context, workspace serveWorkspace, resourceID string, mutate func(serveWorkspace) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	run := func() error {
		current, err := s.revalidateWorkspaceMutation(workspace)
		if err != nil {
			return err
		}
		if mutate == nil {
			return nil
		}
		return mutate(current)
	}
	if s.agents == nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		return run()
	}
	return s.agents.withResourceController(ctx, workspace, resourceID, run)
}

func (s *server) updateWorkspaceDefaults(w http.ResponseWriter, r *http.Request, workspaceID string) {
	var body struct {
		Project app.AgentBinding `json:"project"`
		Task    app.AgentBinding `json:"task"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	defaults := app.ResourceAgentDefaults{Project: body.Project, Task: body.Task}
	for _, binding := range []app.AgentBinding{defaults.Project, defaults.Task} {
		if err := s.validateProfileBinding(binding); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	updated, err := s.setWorkspaceDefaults(r.Context(), workspace, defaults)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"resourceDefaults": updated})
}

// setWorkspaceDefaults serializes fallback changes with native Scheduler
// reconciliation. An attention-held occurrence has no deadline, so a durable
// material change requests a prompt pass from the controller job which made
// the change. Cancelled-before-start, stale, failed, and normalized no-op
// requests do not disturb the reconcile loop.
func (s *server) setWorkspaceDefaults(ctx context.Context, workspace serveWorkspace, defaults app.ResourceAgentDefaults) (app.ResourceAgentDefaults, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.agents == nil {
		if err := ctx.Err(); err != nil {
			return app.ResourceAgentDefaults{}, err
		}
		if err := s.requireWorkspaceOwnership(workspace.Path); err != nil {
			return app.ResourceAgentDefaults{}, err
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			return app.ResourceAgentDefaults{}, err
		}
		return puaWorkspace.SetResourceAgentDefaults(defaults)
	}

	return runSchedulerControllerJob(ctx, s, workspace, func() schedulerControllerJobOutcome[app.ResourceAgentDefaults] {
		current, err := s.revalidateWorkspaceMutation(workspace)
		if err != nil {
			return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{Err: err}
		}
		puaWorkspace, err := app.OpenWorkspace(current.Path)
		if err != nil {
			return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{Err: err}
		}
		previous, err := puaWorkspace.RuntimeConfig()
		if err != nil {
			return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{Err: err}
		}
		updated, err := puaWorkspace.SetResourceAgentDefaults(defaults)
		if err != nil {
			return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{Err: err}
		}
		return schedulerControllerJobOutcome[app.ResourceAgentDefaults]{
			Value: updated, Material: previous.ResourceDefaults != updated,
		}
	}, func(app.ResourceAgentDefaults) {
		s.agents.requestReconcile(reconcileScheduler)
	})
}

func (s *server) updateWorkspaceGenerationPolicy(w http.ResponseWriter, r *http.Request, workspaceID string) {
	var policy app.GenerationPolicy
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	var updated app.GenerationPolicy
	err = s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		updated, openErr = puaWorkspace.SetGenerationPolicy(policy)
		return openErr
	})
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"generationPolicy": updated})
}

func (s *server) updateWorkspaceStallWatchdogPolicy(w http.ResponseWriter, r *http.Request, workspaceID string) {
	var policy app.StallWatchdogPolicy
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	var updated app.StallWatchdogPolicy
	err = s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		updated, openErr = puaWorkspace.SetStallWatchdogPolicy(policy)
		return openErr
	})
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"stallWatchdogPolicy": updated})
}

func (s *server) updateResourceTitle(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	var body struct {
		Title string `json:"title"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	var updated string
	err = s.withWorkspaceMutation(r.Context(), workspace, resourceID, func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		updated, openErr = puaWorkspace.SetResourceTitle(resourceID, body.Title)
		return openErr
	})
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"title": updated})
}

func (s *server) updateResourceDescription(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	var body struct {
		Description string `json:"description"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	var updated string
	err = s.withWorkspaceMutation(r.Context(), workspace, resourceID, func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		updated, openErr = puaWorkspace.SetResourceDescription(resourceID, body.Description)
		return openErr
	})
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"description": updated})
}

func (s *server) updateProjectTaskDefault(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	var binding app.AgentBinding
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(binding.Kind) != "" || strings.TrimSpace(binding.Name) != "" {
		normalized, err := app.NormalizeAgentBinding(binding)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if err := s.validateProfileBinding(normalized); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	var updated app.AgentBinding
	err = s.withWorkspaceMutation(r.Context(), workspace, resourceID, func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		updated, openErr = puaWorkspace.SetProjectTaskDefault(resourceID, binding)
		return openErr
	})
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"taskDefault": updated})
}

func (s *server) updateResourceAgentBinding(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	var binding app.AgentBinding
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	binding, err := app.NormalizeAgentBinding(binding)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if binding.Kind == "profile" {
		cfg, cfgErr := s.loadConfig()
		if cfgErr != nil {
			writeError(w, cfgErr, http.StatusInternalServerError)
			return
		}
		if configuredAgentProfileName(cfg.AgentProfiles, binding.Name) == "" {
			writeError(w, fmt.Errorf("unknown or unconfigured Agent Profile %q", binding.Name), http.StatusBadRequest)
			return
		}
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	var updated app.AgentBinding
	if s.agents != nil {
		var persisted bool
		updated, persisted, err = s.agents.updateResourceAgentBinding(r.Context(), workspace, resourceID, binding)
		if err != nil {
			status := http.StatusBadRequest
			if persisted {
				status = http.StatusBadGateway
			}
			writeError(w, err, status)
			return
		}
	} else {
		err = s.withWorkspaceMutation(r.Context(), workspace, resourceID, func(current serveWorkspace) error {
			puaWorkspace, openErr := app.OpenWorkspace(current.Path)
			if openErr != nil {
				return openErr
			}
			updated, openErr = puaWorkspace.SetResourceAgentBinding(resourceID, binding)
			return openErr
		})
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	}
	writeJSON(w, map[string]any{"agentBinding": updated})
}

func (s *server) handleUIState(w http.ResponseWriter, r *http.Request, id string) {
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	userName, err := s.workspaceUserName(r, workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		state, err := s.loadUIState(id, userName)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, state)
	case http.MethodPut:
		var state uiState
		if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if err := s.saveUIState(id, state, userName); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		saved, err := s.loadUIState(id, userName)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, saved)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Description string `json:"description"`
		Slug        string `json:"slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	var result app.Project
	err = s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		result, openErr = puaWorkspace.CreateProject(body.Description, body.Slug)
		return openErr
	})
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

type createTaskRequest struct {
	Project                string         `json:"project"`
	Title                  string         `json:"title"`
	Detail                 string         `json:"detail"`
	TaskMarkdown           *string        `json:"taskMarkdown"`
	Description            string         `json:"description"`
	TemplateName           string         `json:"templateName"`
	TemplateFields         map[string]any `json:"templateFields"`
	ExpectedTemplateDigest string         `json:"expectedTemplateDigest"`
	Slug                   string         `json:"slug"`
}

func decodeCreateTaskRequest(r *http.Request) (createTaskRequest, error) {
	var body createTaskRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return body, err
	}
	if strings.TrimSpace(body.Title) == "" {
		body.Title = body.Description
	}
	if body.TaskMarkdown != nil && strings.TrimSpace(body.Detail) != "" {
		return body, errors.New("detail and taskMarkdown are mutually exclusive")
	}
	if strings.TrimSpace(body.TemplateName) != "" && (body.TaskMarkdown != nil || strings.TrimSpace(body.Detail) != "") {
		return body, errors.New("templateName is mutually exclusive with detail and taskMarkdown")
	}
	return body, nil
}

func createTaskInputFromRequest(body createTaskRequest) app.CreateTaskInput {
	input := app.CreateTaskInput{
		ProjectID: body.Project, Title: body.Title, Detail: body.Detail, Slug: body.Slug,
		TemplateName: body.TemplateName, TemplateFields: body.TemplateFields, ExpectedTemplateDigest: body.ExpectedTemplateDigest,
	}
	if body.TaskMarkdown != nil {
		input.CompleteMarkdown, input.CompleteMarkdownSet = *body.TaskMarkdown, true
	}
	return input
}

func (s *server) createTask(w http.ResponseWriter, r *http.Request, id string) {
	body, err := decodeCreateTaskRequest(r)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	var result app.Task
	err = s.withWorkspaceMutation(r.Context(), workspace, body.Project, func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		result, openErr = puaWorkspace.CreateTask(createTaskInputFromRequest(body))
		return openErr
	})
	if err != nil {
		status := http.StatusBadRequest
		if app.IsKind(err, "template_conflict") {
			status = http.StatusConflict
		}
		writeError(w, err, status)
		return
	}
	writeJSON(w, result)
}

func (s *server) previewTask(w http.ResponseWriter, r *http.Request, id string) {
	body, err := decodeCreateTaskRequest(r)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	result, err := puaWorkspace.PreviewTask(createTaskInputFromRequest(body))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

func (s *server) handleTemplates(w http.ResponseWriter, r *http.Request, id string, parts []string) {
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project"))
	if len(parts) == 0 {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		result, err := puaWorkspace.Templates(projectID)
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"templates": result})
		return
	}
	if len(parts) == 1 && parts[0] == "validate" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, puaWorkspace.ValidateTemplateContent(body.Name, body.Content))
		return
	}
	name := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		result, err := puaWorkspace.Template(projectID, name)
		if err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		writeJSON(w, result)
		return
	}
	if len(parts) == 2 && parts[1] == "render" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Fields map[string]any `json:"fields"`
			Title  string         `json:"title"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		result, err := puaWorkspace.RenderTemplate(app.TemplateRenderInput{ProjectID: projectID, Name: name, Fields: body.Fields, Title: body.Title})
		if err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, result)
		return
	}
	http.NotFound(w, r)
}

func (s *server) archiveResource(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		ResourceID string `json:"resourceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	resourceID := strings.TrimSpace(body.ResourceID)
	if resourceID == "" {
		writeError(w, errors.New("resourceId is required"), http.StatusBadRequest)
		return
	}
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	var archiveResult app.ArchiveResult
	var archivedResourceIDs []string
	archive := func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		resourceIDs, resourceIDsErr := archiveResourceIDs(puaWorkspace, resourceID)
		result, archiveErr := puaWorkspace.ArchiveResource(resourceID)
		if archiveErr != nil {
			return archiveErr
		}
		archivedResourceIDs = resourceIDs
		if resourceIDsErr != nil {
			warning := app.ArchiveWarning{
				Severity:   "warning",
				Code:       "runtime_descendants_unverifiable",
				Message:    fmt.Sprintf("resource %s was archived, but runtime descendants could not be enumerated: %v; background reconciliation will retry", resourceID, resourceIDsErr),
				ResourceID: resourceID,
			}
			result.Warnings = append(result.Warnings, warning)
		}
		for _, archivedResourceID := range resourceIDs {
			if markErr := markResourceMailboxArchived(current.Path, archivedResourceID); markErr != nil {
				result.Warnings = append(result.Warnings, app.ArchiveWarning{
					Severity:   "warning",
					Code:       "runtime_mailbox_mark_failed",
					Message:    fmt.Sprintf("resource %s was archived, but its runtime mailbox could not be marked archived: %v; background reconciliation will retry", archivedResourceID, markErr),
					ResourceID: archivedResourceID,
				})
			}
		}
		archiveResult = result
		if pruneErr := s.pruneUIStateForArchivedResources(current.Path, archivedResourceIDs); pruneErr != nil {
			archiveResult.Warnings = append(archiveResult.Warnings, app.ArchiveWarning{
				Severity:   "warning",
				Code:       "ui_state_prune_failed",
				Message:    fmt.Sprintf("resource %s was archived, but its persisted UI state could not be pruned: %v", resourceID, pruneErr),
				ResourceID: resourceID,
			})
		}
		return nil
	}
	archiveErr := s.withWorkspaceMutation(r.Context(), workspace, resourceID, archive)
	if archiveErr != nil {
		status := http.StatusBadRequest
		if s.agents != nil {
			var apiErr *resourceAPIError
			if errors.As(archiveErr, &apiErr) {
				status = resourceErrorStatus(archiveErr)
			}
		}
		writeError(w, archiveErr, status)
		return
	}
	// Keep the existing path field while exposing non-blocking conditions to
	// HTTP/Web callers. Warnings are omitted for the common clean case.
	writeJSON(w, struct {
		Path     string               `json:"path"`
		Warnings []app.ArchiveWarning `json:"warnings,omitempty"`
	}{Path: archiveResult.Path, Warnings: archiveResult.Warnings})
	if s.agents != nil {
		s.agents.requestReconcile(reconcileColdAudit)
	}
}

func (s *server) worktreeDiff(w http.ResponseWriter, r *http.Request, id string) {
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if relPath == "" {
		writeError(w, errors.New("path is required"), http.StatusBadRequest)
		return
	}
	cleanRelPath := filepath.ToSlash(filepath.Clean(relPath))
	if isHiddenAgentsPath(cleanRelPath) {
		writeError(w, errors.New("project and task AGENTS.md files are hidden in the PUA web UI"), http.StatusNotFound)
		return
	}
	abs, err := safeWorkspacePath(workspace.Path, relPath)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if !info.IsDir() {
		writeError(w, errors.New("diff path must be a worktree directory"), http.StatusBadRequest)
		return
	}
	if _, err := s.runGit(r.Context(), abs, "rev-parse", "--show-toplevel"); err != nil {
		writeError(w, fmt.Errorf("not a git worktree: %w", err), http.StatusBadRequest)
		return
	}
	branchOut, _ := s.runGit(r.Context(), abs, "rev-parse", "--abbrev-ref", "HEAD")
	branch := strings.TrimSpace(string(branchOut))
	base := strings.TrimSpace(r.URL.Query().Get("base"))
	if !safeGitRef(base) {
		base = ""
	}
	diff, err := s.buildDiff(r.Context(), abs, base)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, diffResponse{
		Path:       filepath.ToSlash(filepath.Clean(relPath)),
		Name:       info.Name(),
		Branch:     branch,
		Base:       base,
		Diff:       diff,
		HasChanges: strings.TrimSpace(diff) != "",
	})
}

func (s *server) previewFile(w http.ResponseWriter, r *http.Request, id string) {
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if relPath == "" {
		writeError(w, errors.New("path is required"), http.StatusBadRequest)
		return
	}
	abs, resolvedRel, err := resolveWorkspaceFileLink(workspace.Path, relPath)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	previewPath(w, resolvedRel, abs)
}

func previewPath(w http.ResponseWriter, relPath, abs string) {
	info, err := os.Stat(abs)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	if info.IsDir() {
		writeError(w, errors.New("cannot preview a directory"), http.StatusBadRequest)
		return
	}

	file, err := os.Open(abs)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, previewMaxBytes+1))
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	truncated := len(data) > previewMaxBytes
	if truncated {
		data = data[:previewMaxBytes]
	}
	binary := containsNUL(data) || !utf8.Valid(data)
	mimeType := fileMimeType(relPath, data)
	image := isPreviewableImage(relPath)
	preview := filePreview{
		Path:        filepath.ToSlash(filepath.Clean(relPath)),
		Name:        info.Name(),
		Size:        info.Size(),
		Truncated:   truncated,
		Binary:      binary,
		Image:       image,
		MimeType:    mimeType,
		ContentHash: previewContentHash(data),
	}
	if !binary && !image {
		preview.Content = string(data)
	}
	writeJSON(w, preview)
}

func previewContentHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s *server) saveWorkspaceAgentsFile(w http.ResponseWriter, r *http.Request, id string) {
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	relPath := filepath.ToSlash(filepath.Clean(strings.TrimSpace(r.URL.Query().Get("path"))))
	if relPath != "AGENTS.md" {
		writeError(w, errors.New("only workspace AGENTS.md can be edited"), http.StatusBadRequest)
		return
	}
	var body struct {
		Content             string `json:"content"`
		ExpectedContentHash string `json:"expectedContentHash"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, markdownSaveRequestMaxBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.ExpectedContentHash) == "" {
		writeError(w, errors.New("expectedContentHash is required"), http.StatusBadRequest)
		return
	}
	content := []byte(body.Content)
	if len(content) > previewMaxBytes {
		writeError(w, fmt.Errorf("AGENTS.md files larger than %d bytes cannot be edited", previewMaxBytes), http.StatusRequestEntityTooLarge)
		return
	}
	if !utf8.Valid(content) || containsNUL(content) {
		writeError(w, errors.New("AGENTS.md content must be valid UTF-8 text"), http.StatusBadRequest)
		return
	}
	errorStatus := http.StatusInternalServerError
	err = s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		path := filepath.Join(current.Path, "AGENTS.md")
		writeErr := replaceMarkdownFile(path, content, body.ExpectedContentHash)
		if errors.Is(writeErr, errMarkdownContentConflict) {
			errorStatus = http.StatusConflict
			return errors.New("AGENTS.md changed on disk; reconcile the preserved browser draft before saving")
		}
		if writeErr == nil {
			previewPath(w, relPath, path)
		}
		return writeErr
	})
	if err != nil {
		writeError(w, err, errorStatus)
		return
	}
}

func isHiddenAgentsPath(relPath string) bool {
	return relPath != "AGENTS.md" && urlpath.Base(relPath) == "AGENTS.md"
}

func (s *server) serveRawFile(w http.ResponseWriter, r *http.Request, id string) {
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if relPath == "" {
		writeError(w, errors.New("path is required"), http.StatusBadRequest)
		return
	}
	abs, resolvedRel, err := resolveWorkspaceFileLink(workspace.Path, relPath)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	serveRawPath(w, r, resolvedRel, abs)
}

func serveRawPath(w http.ResponseWriter, r *http.Request, relPath, abs string) {
	file, err := os.Open(abs)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		writeError(w, errors.New("cannot preview a directory"), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name()}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
		return
	}
	sample, err := io.ReadAll(io.LimitReader(file, previewMaxBytes+1))
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if !isPreviewableImage(relPath) && (containsNUL(sample) || !utf8.Valid(sample)) {
		writeError(w, errors.New("raw preview is only available for text and common image formats"), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", contentTypeWithCharset(fileMimeType(relPath, sample)))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
}

func (s *server) addCurrentDirectoryIfEmpty(ctx context.Context) {
	cfg, err := s.loadConfig()
	if err != nil || len(cfg.Workspaces) > 0 {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	if _, err := s.addWorkspace(ctx, cwd); err != nil {
		log.Printf("add current directory as workspace: %v", err)
	}
}

func (s *server) addWorkspace(ctx context.Context, path string) (serveWorkspace, error) {
	return s.addWorkspaceWithOptions(ctx, path, false, "", "")
}

func (s *server) addWorkspaceWithOptions(ctx context.Context, path string, create bool, language, initialUserName string) (workspace serveWorkspace, err error) {
	return s.addWorkspaceWithOptionsAndSave(ctx, path, create, language, initialUserName, s.saveConfigLocked)
}

func (s *server) addWorkspaceWithOptionsAndSave(ctx context.Context, path string, create bool, language, initialUserName string, save func(config) error) (workspace serveWorkspace, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return serveWorkspace{}, errors.New("workspace path is required")
	}
	if !create && strings.TrimSpace(language) != "" {
		return serveWorkspace{}, errors.New("language is only valid when creating a Workspace")
	}
	if create {
		initialUserName = strings.TrimSpace(initialUserName)
		if err := app.ValidateUserName(initialUserName); err != nil {
			return serveWorkspace{}, fmt.Errorf("initial user name: %w", err)
		}
		language, err = app.NormalizeLanguage(language)
		if err != nil {
			return serveWorkspace{}, err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return serveWorkspace{}, err
	}
	if create {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return serveWorkspace{}, err
		}
	}
	canonical, err := canonicalWorkspacePath(abs)
	if err != nil {
		return serveWorkspace{}, err
	}
	// Ownership comes first: the lock is acquired before the Workspace is
	// inspected or persisted, and rolled back if any later step fails, so a
	// failed add never leaves a half-written config or a stray lock.
	locked := false
	if s.locks != nil && !s.locks.owns(canonical) {
		if _, err := s.locks.acquire(canonical); err != nil {
			return serveWorkspace{}, err
		}
		locked = true
	}
	defer func() {
		if err != nil && locked {
			s.locks.release(canonical)
		}
	}()
	tree, err := s.treeAt(ctx, canonical)
	if err != nil {
		if !create {
			return serveWorkspace{}, err
		}
		if _, initErr := app.Initialize(canonical, language); initErr != nil {
			return serveWorkspace{}, initErr
		}
		tree, err = s.treeAt(ctx, canonical)
		if err != nil {
			return serveWorkspace{}, err
		}
	}
	workspace = serveWorkspace{
		ID:   workspaceID(tree.Root),
		Name: workspaceName(tree.Root),
		Path: tree.Root,
	}
	puaWorkspace, err := app.OpenWorkspace(tree.Root)
	if err != nil {
		return serveWorkspace{}, err
	}
	if err := s.ensureWorkspaceUsersAndMigrateUIState(tree.Root); err != nil {
		return serveWorkspace{}, err
	}
	if create {
		profile, registerErr := puaWorkspace.RegisterUser(initialUserName)
		if registerErr != nil {
			return serveWorkspace{}, registerErr
		}
		if baselineErr := s.ensureUserUIStateBaseline(tree.Root, profile.Name); baselineErr != nil {
			return serveWorkspace{}, baselineErr
		}
	}
	runtime, err := puaWorkspace.EnsureResourceRuntime()
	if err != nil {
		return serveWorkspace{}, err
	}
	workspace.InstanceID = strings.TrimSpace(runtime.InstanceID)
	if workspace.InstanceID == "" {
		return serveWorkspace{}, errors.New("Workspace resource runtime has no instance id")
	}
	_, _, err = s.mutateConfigWithSave(save, func(cfg *config) (bool, error) {
		replaced := false
		for i := range cfg.Workspaces {
			if cfg.Workspaces[i].ID == workspace.ID {
				// Refresh mutable on-disk metadata at the commit boundary. A
				// concurrent name mutation writes the Workspace before entering
				// this config transaction, so either this read observes it or its
				// later transaction wins without a stale live add reverting it.
				workspace.Name = workspaceName(tree.Root)
				workspace.Icon = cfg.Workspaces[i].Icon
				cfg.Workspaces[i] = workspace
				replaced = true
				break
			}
		}
		if !replaced {
			cfg.Workspaces = append(cfg.Workspaces, workspace)
		}
		cfg.ActiveID = workspace.ID
		return true, nil
	})
	if err != nil {
		return serveWorkspace{}, err
	}
	if s.agents != nil {
		if err := s.agents.reviveWorkspaceBarrier(workspace.Path); err != nil {
			return serveWorkspace{}, err
		}
		// Configuration persistence makes this Workspace visible to the global
		// Scheduler deadline projection. Refresh it only after a retired handoff
		// barrier has been revived, and independently of caller cancellation: at
		// this point the live add is already durable.
		s.agents.requestReconcile(reconcileScheduler)
	}
	if s.doctor != nil {
		s.doctor.requestScan()
	}
	return workspace, nil
}

func (s *server) ensureConfiguredResourceRuntimes() error {
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	for _, workspace := range cfg.Workspaces {
		if !s.ownsWorkspace(workspace.Path) {
			continue
		}
		// Startup backfill leaves the identity empty only when a legacy
		// Workspace is already missing or unreadable. Keep serving so the
		// stale entry can be removed; all Workspace writes still require a
		// readable runtime identity and the held advisory lock.
		configuredInstanceID := strings.TrimSpace(workspace.InstanceID)
		if configuredInstanceID == "" {
			continue
		}
		puaWorkspace, err := app.OpenWorkspace(workspace.Path)
		if err != nil {
			return fmt.Errorf("open Workspace %s for resource runtime: %w", workspace.ID, err)
		}
		runtime, err := puaWorkspace.EnsureResourceRuntime()
		if err != nil {
			return fmt.Errorf("initialize Workspace %s resource runtime: %w", workspace.ID, err)
		}
		if strings.TrimSpace(runtime.InstanceID) != configuredInstanceID {
			return fmt.Errorf("Workspace %s instance changed after serve configuration was persisted", workspace.ID)
		}
		if _, err := puaWorkspace.EnsureScheduler(); err != nil {
			return fmt.Errorf("initialize Workspace %s Scheduler: %w", workspace.ID, err)
		}
	}
	return nil
}

type configuredWorkspaceInstanceBackfillKey struct {
	id   string
	path string
}

func (s *server) backfillConfiguredWorkspaceInstanceIDs() error {
	return s.backfillConfiguredWorkspaceInstanceIDsWithSave(s.saveConfigLocked)
}

// backfillConfiguredWorkspaceInstanceIDsWithSave upgrades fixed-base serve
// configurations before recovery can enqueue any Workspace work. Resolvable
// IDs are committed together; entries that are already stale remain blank and
// use the collision-free path removal controller.
func (s *server) backfillConfiguredWorkspaceInstanceIDsWithSave(save func(config) error) error {
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	resolved := make(map[configuredWorkspaceInstanceBackfillKey]string)
	for _, workspace := range cfg.Workspaces {
		if strings.TrimSpace(workspace.InstanceID) != "" || !s.ownsWorkspace(workspace.Path) {
			continue
		}
		puaWorkspace, openErr := app.OpenWorkspace(workspace.Path)
		if openErr != nil {
			continue
		}
		runtime, runtimeErr := puaWorkspace.EnsureResourceRuntime()
		if runtimeErr != nil || strings.TrimSpace(runtime.InstanceID) == "" {
			continue
		}
		canonical, canonicalErr := canonicalWorkspacePath(workspace.Path)
		if canonicalErr != nil {
			continue
		}
		resolved[configuredWorkspaceInstanceBackfillKey{id: workspace.ID, path: canonical}] = strings.TrimSpace(runtime.InstanceID)
	}
	if len(resolved) == 0 {
		return nil
	}

	// Reload inside the serialized commit boundary so settings and Workspace
	// list mutations cannot be overwritten. Each candidate's path and live
	// identity are revalidated before it is persisted; a changed entry is left
	// untouched for the next pass.
	_, _, err = s.mutateConfigWithSave(save, func(current *config) (bool, error) {
		changed := false
		for index := range current.Workspaces {
			workspace := &current.Workspaces[index]
			if strings.TrimSpace(workspace.InstanceID) != "" {
				continue
			}
			canonical, canonicalErr := canonicalWorkspacePath(workspace.Path)
			if canonicalErr != nil {
				continue
			}
			candidate, ok := resolved[configuredWorkspaceInstanceBackfillKey{id: workspace.ID, path: canonical}]
			if !ok {
				continue
			}
			liveInstanceID, liveErr := workspaceInstanceID(workspace.Path)
			if liveErr != nil || strings.TrimSpace(liveInstanceID) != candidate {
				continue
			}
			workspace.InstanceID = candidate
			changed = true
		}
		return changed, nil
	})
	return err
}

func (s *server) updateWorkspaceIcon(id, icon string) (serveWorkspace, error) {
	icon = strings.TrimSpace(icon)
	if icon != "" {
		if _, ok := workspaceIconFiles[icon]; !ok {
			return serveWorkspace{}, fmt.Errorf("unknown workspace icon: %s", icon)
		}
	}
	workspace, err := s.workspace(id)
	if err != nil {
		return serveWorkspace{}, err
	}
	var updated serveWorkspace
	err = s.withWorkspaceMutation(context.Background(), workspace, "workspace", func(current serveWorkspace) error {
		_, _, mutateErr := s.mutateConfig(func(cfg *config) (bool, error) {
			for i := range cfg.Workspaces {
				if cfg.Workspaces[i].ID != current.ID {
					continue
				}
				cfg.Workspaces[i].Icon = icon
				updated = cfg.Workspaces[i]
				return true, nil
			}
			return false, fmt.Errorf("workspace not found: %s", id)
		})
		return mutateErr
	})
	return updated, err
}

func (s *server) updateWorkspaceName(id, name string) (serveWorkspace, error) {
	workspace, err := s.workspace(id)
	if err != nil {
		return serveWorkspace{}, err
	}
	var updated serveWorkspace
	err = s.withWorkspaceMutation(context.Background(), workspace, "workspace", func(current serveWorkspace) error {
		puaWorkspace, openErr := app.OpenWorkspace(current.Path)
		if openErr != nil {
			return openErr
		}
		resolved, setErr := puaWorkspace.SetName(name)
		if setErr != nil {
			return setErr
		}
		_, _, mutateErr := s.mutateConfig(func(cfg *config) (bool, error) {
			for i := range cfg.Workspaces {
				if cfg.Workspaces[i].ID != current.ID {
					continue
				}
				cfg.Workspaces[i].Name = resolved
				updated = cfg.Workspaces[i]
				return true, nil
			}
			return false, fmt.Errorf("workspace not found: %s", id)
		})
		return mutateErr
	})
	return updated, err
}

func (s *server) removeWorkspace(id string) error {
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	var removing serveWorkspace
	for _, workspace := range cfg.Workspaces {
		if workspace.ID == id {
			removing = workspace
			break
		}
	}
	if removing.ID == "" {
		return fmt.Errorf("workspace not found: %s", id)
	}
	expectedPath, err := canonicalWorkspacePath(removing.Path)
	if err != nil {
		return err
	}
	controllerInstanceID := strings.TrimSpace(removing.InstanceID)
	legacyInstanceLookup := controllerInstanceID == ""
	staleLegacyRemoval := false
	if legacyInstanceLookup {
		controllerInstanceID, err = workspaceInstanceID(removing.Path)
		if err != nil {
			controllerInstanceID = ""
			staleLegacyRemoval = true
		}
		controllerInstanceID = strings.TrimSpace(controllerInstanceID)
		if controllerInstanceID == "" && !staleLegacyRemoval {
			staleLegacyRemoval = true
		}
	}
	remove := func() error {
		return s.agents.withWorkspaceHandoff(context.Background(), expectedPath, func() error {
			return s.removeWorkspaceLocked(id, expectedPath, controllerInstanceID, legacyInstanceLookup, staleLegacyRemoval)
		})
	}
	if s.agents == nil {
		return s.removeWorkspaceLocked(id, expectedPath, controllerInstanceID, legacyInstanceLookup, staleLegacyRemoval)
	}
	// The Scheduler controller establishes one outer lock order for Scheduler
	// delivery and removal. The exclusive Workspace barrier then drains jobs on
	// every resource controller before config deletion and advisory-lock release;
	// jobs starting behind the handoff revalidate ownership and fail closed.
	if staleLegacyRemoval {
		return s.agents.withStaleWorkspacePathController(context.Background(), expectedPath, app.SchedulerResourceID, remove)
	}
	return s.agents.withResourceControllerInstanceID(context.Background(), controllerInstanceID, app.SchedulerResourceID, remove)
}

func (s *server) removeWorkspaceLocked(id, expectedPath, controllerInstanceID string, legacyInstanceLookup, staleLegacyRemoval bool) error {
	if err := s.requireWorkspaceRemovalClaim(expectedPath); err != nil {
		return err
	}
	var removedPath string
	_, _, err := s.mutateConfig(func(cfg *config) (bool, error) {
		next := cfg.Workspaces[:0]
		removed := false
		for _, workspace := range cfg.Workspaces {
			if workspace.ID == id {
				if canonical, canonicalErr := canonicalWorkspacePath(workspace.Path); canonicalErr != nil || canonical != expectedPath {
					return false, fmt.Errorf("workspace %s changed while removal was waiting", id)
				}
				configuredInstanceID := strings.TrimSpace(workspace.InstanceID)
				if staleLegacyRemoval {
					if configuredInstanceID != "" {
						return false, fmt.Errorf("workspace %s instance changed while stale removal was waiting", id)
					}
					if liveInstanceID, liveErr := workspaceInstanceID(workspace.Path); liveErr == nil && strings.TrimSpace(liveInstanceID) != "" {
						return false, fmt.Errorf("workspace %s became available while stale removal was waiting", id)
					}
				} else if configuredInstanceID == "" && legacyInstanceLookup {
					liveInstanceID, liveErr := workspaceInstanceID(workspace.Path)
					if liveErr != nil {
						return false, fmt.Errorf("verify legacy Workspace %s instance id: %w", id, liveErr)
					}
					configuredInstanceID = strings.TrimSpace(liveInstanceID)
				} else if configuredInstanceID != "" {
					// An unavailable persisted Workspace is removable, but a newly
					// readable Workspace at the same path must still be the same
					// instance. Otherwise this callback belongs to the old controller
					// and must not release ownership underneath the replacement.
					liveInstanceID, liveErr := workspaceInstanceID(workspace.Path)
					if liveErr == nil && strings.TrimSpace(liveInstanceID) != configuredInstanceID {
						return false, fmt.Errorf("workspace %s live instance changed while removal was waiting", id)
					}
				}
				if !staleLegacyRemoval && configuredInstanceID != controllerInstanceID {
					return false, fmt.Errorf("workspace %s instance changed while removal was waiting", id)
				}
				removed = true
				removedPath = workspace.Path
				continue
			}
			next = append(next, workspace)
		}
		if !removed {
			return false, fmt.Errorf("workspace not found: %s", id)
		}
		cfg.Workspaces = next
		if cfg.ActiveID == id {
			cfg.ActiveID = ""
			if len(cfg.Workspaces) > 0 {
				cfg.ActiveID = cfg.Workspaces[0].ID
			}
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	// Keep the persisted removal and advisory-lock handoff in the same
	// Scheduler-controller callback. No stale callback can cross this release.
	if s.locks != nil {
		s.locks.release(removedPath)
	}
	if s.doctor != nil {
		s.doctor.requestScan()
	}
	return nil
}

func (s *server) tree(ctx context.Context, id string, userNames ...string) (workspaceTree, error) {
	workspace, err := s.workspace(id)
	if err != nil {
		return workspaceTree{}, err
	}
	return s.treeAt(ctx, workspace.Path, userNames...)
}

func (s *server) treeAt(ctx context.Context, path string, userNames ...string) (workspaceTree, error) {
	if err := s.requireWorkspaceOwnership(path); err != nil {
		return workspaceTree{}, err
	}
	puaWorkspace, err := app.OpenWorkspace(path)
	if err != nil {
		return workspaceTree{}, err
	}
	_ = ctx
	typedTree, err := puaWorkspace.Tree()
	if err != nil {
		return workspaceTree{}, err
	}
	tree := workspaceTreeFromApp(typedTree)
	runtimeConfig, runtimeErr := puaWorkspace.RuntimeConfig()
	if runtimeErr == nil {
		tree.AgentBinding = runtimeConfig.AgentBinding
		tree.ResourceDefaults = runtimeConfig.ResourceDefaults
		tree.GenerationPolicy = runtimeConfig.GenerationPolicy
		tree.StallWatchdogPolicy = runtimeConfig.StallWatchdogPolicy
	} else {
		tree.AgentBinding = app.AgentBinding{Kind: "profile", Name: "default"}
		tree.ResourceDefaults = app.ResourceAgentDefaults{
			Project: app.AgentBinding{Kind: "profile", Name: "default"},
			Task:    app.AgentBinding{Kind: "profile", Name: "default"},
		}
		tree.GenerationPolicy = app.GenerationPolicy{
			Enabled: true, MaxTurns: app.DefaultGenerationMaxTurns,
			MaxAccumulatedTurnMinutes: app.DefaultGenerationMaxAccumulatedTurnMinutes,
		}
		tree.StallWatchdogPolicy = app.StallWatchdogPolicy{Enabled: true, TimeoutMinutes: app.DefaultStallWatchdogTimeoutMinutes}
	}
	tree.Workspace = resourceSnapshot{
		ID: "workspace", Type: "workspace", Title: workspaceName(path), Path: ".", AgentBinding: tree.AgentBinding,
	}
	if err := s.enrichTreeResourceRuntime(path, &tree); err != nil {
		return workspaceTree{}, err
	}
	if selectedUserName(userNames) != "" {
		if err := s.enrichTreeResourceActivity(path, &tree, userNames...); err != nil {
			return workspaceTree{}, err
		}
	}
	return tree, nil
}

type resourceRuntimeSnapshot struct {
	Generation              int    `json:"generation,omitempty"`
	GenerationID            string `json:"generationId"`
	Status                  string `json:"status"`
	SessionState            string `json:"sessionState"`
	AgentName               string `json:"agentName,omitempty"`
	UpdatedAt               string `json:"updatedAt,omitempty"`
	LastOutputAt            string `json:"lastOutputAt,omitempty"`
	CompletionMarker        string `json:"completionMarker,omitempty"`
	CompletionState         string `json:"completionState,omitempty"`
	CompletionHasFinalReply bool   `json:"completionHasFinalReply"`
	CompletionAt            string `json:"completionAt,omitempty"`
	ReplacementPending      bool   `json:"replacementPending,omitempty"`
	Resumable               bool   `json:"resumable,omitempty"`
	IdleSuspended           bool   `json:"idleSuspended,omitempty"`
	ResumeUnavailable       bool   `json:"resumeUnavailable,omitempty"`
	TurnNumber              int    `json:"turnNumber,omitempty"`
	ActiveTurn              bool   `json:"activeTurn,omitempty"`
	TurnStartedAt           string `json:"turnStartedAt,omitempty"`
}

func (s *server) enrichTreeResourceRuntime(workspacePath string, tree *workspaceTree) error {
	records, err := loadCurrentGenerationRecords(workspacePath)
	if err != nil {
		return fmt.Errorf("load resource generations for tree: %w", err)
	}
	byResourceID := make(map[string]generationRecord)
	for _, record := range records {
		if strings.TrimSpace(record.GenerationID) == "" || !isAgentHubGeneration(record) {
			continue
		}
		resourceID := normalizedResourceID(record.ResourceID)
		if resourceID == "" {
			resourceID = "workspace"
		}
		if current, ok := byResourceID[resourceID]; !ok || resourceRuntimeGenerationNewer(record, current) {
			byResourceID[resourceID] = record
		}
	}
	var attach func(*resourceSnapshot)
	attach = func(item *resourceSnapshot) {
		resourceID := normalizedResourceID(item.ID)
		if record, ok := byResourceID[resourceID]; ok {
			item.Runtime = resourceRuntimeSnapshotForGeneration(record)
		}
		for i := range item.Children {
			attach(&item.Children[i])
		}
	}
	attach(&tree.Workspace)
	attach(&tree.Scheduler)
	for i := range tree.Projects {
		attach(&tree.Projects[i])
	}
	return nil
}

func resourceRuntimeGenerationNewer(left, right generationRecord) bool {
	if left.Generation != right.Generation {
		return left.Generation > right.Generation
	}
	leftTime, leftErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(left.UpdatedAt))
	rightTime, rightErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(right.UpdatedAt))
	leftOK, rightOK := leftErr == nil, rightErr == nil
	if leftOK && rightOK && !leftTime.Equal(rightTime) {
		return leftTime.After(rightTime)
	}
	if leftOK != rightOK {
		return leftOK
	}
	return left.ID > right.ID
}

func (s *server) resource(ctx context.Context, id string, resourceID string) (app.ResourceDetailView, error) {
	workspace, err := s.workspace(id)
	if err != nil {
		return app.ResourceDetailView{}, err
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		return app.ResourceDetailView{}, err
	}
	detail, err := puaWorkspace.Resource(resourceID)
	if err != nil {
		return app.ResourceDetailView{}, err
	}
	if normalizedResourceID(resourceID) == app.SchedulerResourceID && s.agents != nil {
		snapshot, snapshotErr := runSchedulerControllerJob(ctx, s, workspace, func() schedulerControllerJobOutcome[app.SchedulerSnapshot] {
			value, readErr := newNativeScheduler(s.agents, workspace).Snapshot(s.agents.now())
			return schedulerControllerJobOutcome[app.SchedulerSnapshot]{Value: value, Err: readErr}
		}, nil)
		err = snapshotErr
		if err != nil {
			return app.ResourceDetailView{}, err
		}
		detail.Scheduler = &snapshot
	}
	return detail, nil
}

func (s *server) loadUIState(id string, userNames ...string) (uiState, error) {
	userName := selectedUserName(userNames)
	if userName == "" {
		return uiState{}, &resourceAPIError{Code: "user_required", Message: "select a Workspace user before accessing personal data"}
	}
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	workspace, err := s.workspace(id)
	if err != nil {
		return uiState{}, err
	}
	return loadUIStateFile(userUIStatePath(workspace.Path, userName))
}

func (s *server) saveUIState(id string, state uiState, userNames ...string) error {
	userName := selectedUserName(userNames)
	if userName == "" {
		return &resourceAPIError{Code: "user_required", Message: "select a Workspace user before accessing personal data"}
	}
	workspace, err := s.workspace(id)
	if err != nil {
		return err
	}
	return s.withWorkspaceMutation(context.Background(), workspace, "workspace", func(current serveWorkspace) error {
		s.uiStateMu.Lock()
		defer s.uiStateMu.Unlock()
		// UI navigation updates predate user resource state. Preserve the
		// server-owned map so an older browser cannot overwrite read cursors.
		statePath := userUIStatePath(current.Path, userName)
		existing, loadErr := loadUIStateFile(statePath)
		if loadErr != nil {
			return loadErr
		}
		state.ResourceStates = existing.ResourceStates
		state.Attention = existing.Attention
		return saveUIStateFile(statePath, state)
	})
}

func (s *server) buildDiff(ctx context.Context, worktreePath string, base string) (string, error) {
	var parts []string
	if base != "" && s.gitRefExists(ctx, worktreePath, base) {
		if out, err := s.runGit(ctx, worktreePath, "diff", "--no-ext-diff", "--find-renames", "--src-prefix=a/", "--dst-prefix=b/", base+"...HEAD", "--"); err == nil {
			parts = append(parts, string(out))
		}
	}
	if out, err := s.runGit(ctx, worktreePath, "diff", "--no-ext-diff", "--find-renames", "--src-prefix=a/", "--dst-prefix=b/", "HEAD", "--"); err == nil {
		parts = append(parts, string(out))
	}
	if out, err := s.untrackedDiff(ctx, worktreePath); err == nil {
		parts = append(parts, out)
	}
	diff := strings.TrimLeft(strings.Join(parts, "\n"), "\n")
	if len(diff) > diffMaxBytes {
		diff = diff[:diffMaxBytes] + "\n\n--- Diff truncated by PUA ---\n"
	}
	return diff, nil
}

func (s *server) untrackedDiff(ctx context.Context, worktreePath string) (string, error) {
	out, err := s.runGit(ctx, worktreePath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, name := range strings.Split(string(out), "\x00") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if builder.Len() > diffMaxBytes {
			break
		}
		diff, err := s.runGit(ctx, worktreePath, "diff", "--no-ext-diff", "--no-index", "--src-prefix=a/", "--dst-prefix=b/", "--", "/dev/null", name)
		if err != nil && len(diff) == 0 {
			continue
		}
		builder.Write(diff)
		if len(diff) > 0 && diff[len(diff)-1] != '\n' {
			builder.WriteByte('\n')
		}
	}
	return builder.String(), nil
}

func (s *server) gitRefExists(ctx context.Context, worktreePath string, ref string) bool {
	_, err := s.runGit(ctx, worktreePath, "rev-parse", "--verify", ref+"^{commit}")
	return err == nil
}

func (s *server) runGit(ctx context.Context, worktreePath string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = worktreePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return out, fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return out, nil
}

func (s *server) workspace(id string) (serveWorkspace, error) {
	cfg, err := s.loadConfig()
	if err != nil {
		return serveWorkspace{}, err
	}
	for _, workspace := range cfg.Workspaces {
		if workspace.ID == id {
			if err := s.requireWorkspaceOwnership(workspace.Path); err != nil {
				return serveWorkspace{}, err
			}
			return workspace, nil
		}
	}
	return serveWorkspace{}, fmt.Errorf("workspace not found: %s", id)
}

func (s *server) loadConfig() (config, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return s.loadConfigLocked()
}

// loadConfigLocked may upgrade the file and therefore always runs under
// configMu, even when the caller only needs a snapshot.
func (s *server) loadConfigLocked() (config, error) {
	var cfg config
	data, err := os.ReadFile(s.config)
	if err != nil {
		if os.IsNotExist(err) {
			cfg = config{
				Version:          agentHubConfigVersion,
				Workspaces:       []serveWorkspace{},
				AgentHubEndpoint: defaultAgentHubEndpoint,
				AgentProfiles:    []agentProfileRoute{},
			}
			normalized, normalizeErr := normalizeConfigAgentProfileRoutes(cfg.AgentProfiles)
			if normalizeErr != nil {
				return config{}, normalizeErr
			}
			cfg.AgentProfiles = normalized
			return cfg, nil
		}
		return config{}, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return config{}, err
	}
	if cfg.Workspaces == nil {
		cfg.Workspaces = []serveWorkspace{}
	}
	if cfg.Version < 3 {
		return config{}, fmt.Errorf("unsupported PUA serve configuration version %d; migrate the configuration before starting pua serve", cfg.Version)
	}
	needsUpgrade := cfg.Version != agentHubConfigVersion
	cfg.Version = agentHubConfigVersion
	cfg.AgentHubEndpoint, err = normalizeAgentHubEndpoint(cfg.AgentHubEndpoint)
	if err != nil {
		return config{}, err
	}
	normalizedProfiles, err := normalizeConfigAgentProfileRoutes(cfg.AgentProfiles)
	if err != nil {
		return config{}, err
	}
	if !agentProfileRoutesEqual(cfg.AgentProfiles, normalizedProfiles) {
		cfg.AgentProfiles = normalizedProfiles
		needsUpgrade = true
	}
	if needsUpgrade {
		if err := s.saveConfigLocked(cfg); err != nil {
			return config{}, err
		}
	}
	return cfg, nil
}

func (s *server) saveConfig(cfg config) error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	return s.saveConfigLocked(cfg)
}

func (s *server) saveConfigLocked(cfg config) error {
	if cfg.Version < agentHubConfigVersion {
		return fmt.Errorf("unsupported PUA serve configuration version %d", cfg.Version)
	}
	normalizedProfiles, err := normalizeConfigAgentProfileRoutes(cfg.AgentProfiles)
	if err != nil {
		return err
	}
	routes := make([]agentHubProfileRoute, 0, len(normalizedProfiles))
	for _, route := range normalizedProfiles {
		routes = append(routes, agentHubProfileRoute{
			Key: route.Key, Description: route.Description, AgentName: route.AgentName,
		})
	}
	data, err := json.MarshalIndent(agentHubServeConfig{
		Version:            agentHubConfigVersion,
		ActiveID:           cfg.ActiveID,
		Workspaces:         cfg.Workspaces,
		AgentHubEndpoint:   cfg.AgentHubEndpoint,
		AgentHubInstanceID: cfg.AgentHubInstanceID,
		AgentProfiles:      routes,
	}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteConfig(s.config, append(data, '\n'))
}

// mutateConfig serializes an in-process read-modify-write transaction and
// reloads the latest file at its commit boundary. Callers must complete slow
// filesystem discovery, Workspace operations, and AgentHub requests first.
// The callback must not enter a Workspace controller or call loadConfig or
// saveConfig: Workspace mutations use the lock order handoff barrier ->
// configMu.
func (s *server) mutateConfig(mutate func(*config) (bool, error)) (config, bool, error) {
	return s.mutateConfigWithSave(s.saveConfigLocked, mutate)
}

// mutateConfigWithSave is the injectable form used by failure-boundary tests.
// The writer executes while configMu is held and must not call a locking
// server config method.
func (s *server) mutateConfigWithSave(save func(config) error, mutate func(*config) (bool, error)) (config, bool, error) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	cfg, err := s.loadConfigLocked()
	if err != nil {
		return config{}, false, err
	}
	changed, err := mutate(&cfg)
	if err != nil {
		return config{}, false, err
	}
	if !changed {
		return cfg, false, nil
	}
	if save == nil {
		return config{}, false, errors.New("serve configuration writer is nil")
	}
	if err := save(cfg); err != nil {
		return config{}, false, err
	}
	return cfg, true, nil
}

func defaultConfigPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("PUA_SERVE_CONFIG")); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pua", "serve.json"), nil
}

func workspaceID(path string) string {
	sum := sha1.Sum([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:8])
}

func workspaceName(path string) string {
	return app.WorkspaceName(path)
}

// resolvedWorkspaceSummaries refreshes the display names cached in the serve
// config with the name configured in each workspace.json, falling back to the
// directory base name. The persisted cache is only updated on writes; reads
// resolve live so externally edited configs stay accurate.
func resolvedWorkspaceSummaries(workspaces []serveWorkspace) []serveWorkspace {
	result := make([]serveWorkspace, len(workspaces))
	for i, workspace := range workspaces {
		workspace.Name = workspaceName(workspace.Path)
		if opened, err := app.OpenWorkspace(workspace.Path); err == nil {
			if runtime, runtimeErr := opened.RuntimeConfig(); runtimeErr == nil {
				workspace.InstanceID = runtime.InstanceID
			}
		}
		result[i] = workspace
	}
	return result
}

func uiStatePath(workspacePath string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "ui-state.json")
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func safeWorkspacePath(root string, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", errors.New("path must be relative to the workspace")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.Clean(relPath)))
	if err != nil {
		return "", err
	}
	if err := ensurePathInside(rootAbs, targetAbs); err != nil {
		return "", err
	}
	if rootEval, err := filepath.EvalSymlinks(rootAbs); err == nil {
		if targetEval, err := filepath.EvalSymlinks(targetAbs); err == nil {
			if err := ensurePathInside(rootEval, targetEval); err != nil {
				return "", err
			}
		}
	}
	return targetAbs, nil
}

var (
	linkProjectSegment   = regexp.MustCompile(`^project([0-9]+)$`)
	linkTaskSegment      = regexp.MustCompile(`^task([0-9]+)$`)
	linkLineColumnSuffix = regexp.MustCompile(`^(.+):[1-9][0-9]*:[1-9][0-9]*$`)
	linkLineSuffix       = regexp.MustCompile(`^(.+):[1-9][0-9]*$`)
)

// resolveWorkspaceFileLink resolves a Workspace-root link or a machine-local
// absolute link inside the Workspace to its absolute path and canonical
// Workspace-relative path. Leading projectN/taskN segments may name resources
// without a slug even when the on-disk directory carries a slug suffix. Codex
// :line and :line:column suffixes are ignored when the unsuffixed file exists.
func resolveWorkspaceFileLink(root, target string) (string, string, error) {
	relPath, err := workspaceRelativeFileLinkTarget(root, target)
	if err != nil {
		return "", "", err
	}
	clean := filepath.ToSlash(filepath.Clean(relPath))
	if clean == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", "", errors.New("path must be relative to the workspace")
	}
	parts := strings.Split(clean, "/")
	if len(parts) > 0 && linkProjectSegment.MatchString(parts[0]) {
		parts[0] = resolveSluggedDir(root, parts[0])
	}
	if len(parts) > 1 && linkTaskSegment.MatchString(parts[1]) {
		projectDir := filepath.Join(root, filepath.FromSlash(parts[0]))
		parts[1] = resolveSluggedDir(projectDir, parts[1])
	}
	resolvedRel := filepath.FromSlash(strings.Join(parts, "/"))
	abs, err := safeWorkspacePath(root, resolvedRel)
	if err != nil {
		return "", "", err
	}
	if info, statErr := os.Stat(abs); statErr == nil && !info.IsDir() {
		return abs, filepath.ToSlash(resolvedRel), nil
	}
	if base, found := fileLinkBaseWithoutPosition(filepath.ToSlash(resolvedRel)); found {
		baseRel := filepath.FromSlash(base)
		baseAbs, baseErr := safeWorkspacePath(root, baseRel)
		if baseErr == nil {
			if info, statErr := os.Stat(baseAbs); statErr == nil && !info.IsDir() {
				return baseAbs, filepath.ToSlash(baseRel), nil
			}
		}
	}
	return abs, filepath.ToSlash(resolvedRel), nil
}

func workspaceRelativeFileLinkTarget(root, target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", errors.New("path must be relative to the workspace")
	}
	native := filepath.FromSlash(trimmed)
	if !filepath.IsAbs(native) {
		return native, nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rootAbs = filepath.Clean(rootAbs)
	targetAbs := filepath.Clean(native)
	rel, relErr := filepath.Rel(rootAbs, targetAbs)
	if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel, nil
	}
	// If an absolute target textually starts at this Workspace but cleans
	// outside it, do not reinterpret the escaped suffix as a Workspace-root
	// link. This preserves the existing traversal rejection.
	rootPrefix := rootAbs + string(filepath.Separator)
	if native == rootAbs || strings.HasPrefix(native, rootPrefix) {
		return "", errors.New("path escapes the workspace")
	}
	// Existing Markdown uses a leading slash to mean Workspace root. Keep that
	// syntax when the first path segment identifies a PUA Project or an actual
	// top-level Workspace entry; otherwise reject a machine-absolute path that
	// belongs to another location.
	workspaceTarget := strings.TrimPrefix(native, string(filepath.Separator))
	cleanWorkspaceTarget := filepath.Clean(workspaceTarget)
	parts := strings.Split(filepath.ToSlash(cleanWorkspaceTarget), "/")
	if len(parts) > 0 && linkProjectSegment.MatchString(parts[0]) {
		return workspaceTarget, nil
	}
	if len(parts) > 0 {
		if _, statErr := os.Lstat(filepath.Join(rootAbs, filepath.FromSlash(parts[0]))); statErr == nil {
			return workspaceTarget, nil
		}
	}
	return "", errors.New("absolute path must be inside the workspace")
}

func fileLinkBaseWithoutPosition(path string) (string, bool) {
	if match := linkLineColumnSuffix.FindStringSubmatch(path); match != nil {
		return match[1], true
	}
	if match := linkLineSuffix.FindStringSubmatch(path); match != nil {
		return match[1], true
	}
	return "", false
}

func resolveSluggedDir(parent, segment string) string {
	if info, err := os.Stat(filepath.Join(parent, segment)); err == nil && info.IsDir() {
		return segment
	}
	prefix := segment + "-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		return segment
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return segment
}

func ensurePathInside(root string, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes the workspace")
	}
	return nil
}

func containsNUL(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}

func fileMimeType(path string, data []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".mdown", ".mkdn":
		return "text/markdown"
	}
	if mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); mimeType != "" {
		return strings.Split(mimeType, ";")[0]
	}
	if len(data) > 0 {
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}

func contentTypeWithCharset(mimeType string) string {
	if strings.HasPrefix(mimeType, "text/") && !strings.Contains(mimeType, "charset") {
		return mimeType + "; charset=utf-8"
	}
	return mimeType
}

func isPreviewableImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".apng", ".avif", ".bmp", ".gif", ".ico", ".jpg", ".jpeg", ".png", ".svg", ".webp":
		return true
	default:
		return false
	}
}

func safeGitRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return false
	}
	for _, r := range ref {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		switch r {
		case '/', '.', '_', '-':
			continue
		default:
			return false
		}
	}
	return !strings.Contains(ref, "..") && !strings.HasSuffix(ref, ".lock")
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{"error": err.Error()}
	var resourceErr *resourceAPIError
	if errors.As(err, &resourceErr) {
		payload["code"] = resourceErr.Code
	}
	var validation *app.TemplateValidationError
	if errors.As(err, &validation) {
		payload["code"] = "template_validation"
		payload["template"] = validation.Template
		payload["issues"] = validation.Issues
	}
	if app.IsKind(err, "template_conflict") {
		payload["code"] = "template_digest_conflict"
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func serveStatic(root fs.FS, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	} else {
		name = strings.TrimPrefix(urlpath.Clean("/"+name), "/")
	}
	data, err := fs.ReadFile(root, name)
	if err != nil {
		if filepath.Ext(name) != "" && !strings.Contains(name, "/") {
			http.NotFound(w, r)
			return
		}
		name = "index.html"
		data, err = fs.ReadFile(root, name)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
	} else if info, statErr := fs.Stat(root, name); statErr == nil && info.IsDir() {
		name = "index.html"
		data, err = fs.ReadFile(root, name)
		if err != nil {
			writeError(w, err, http.StatusInternalServerError)
			return
		}
	}
	if contentType := mime.TypeByExtension(filepath.Ext(name)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", http.DetectContentType(data))
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
