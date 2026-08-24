// Package app contains the in-process PUA application API.
//
// The API is deliberately rooted in a Workspace handle. Callers must open a
// Workspace explicitly and may then reuse the handle from concurrent
// goroutines. It never selects a Workspace from the process working directory
// and it never writes user-facing output.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/disksing/pua/internal/workspacepath"
)

// ErrResourceNotFound identifies a stable missing-resource condition while
// allowing callers to distinguish it from temporary filesystem failures.
var ErrResourceNotFound = errors.New("resource not found")

// APIError describes a failed application operation without requiring callers
// to parse CLI output. The underlying error remains available through Unwrap.
type APIError struct {
	Operation  string
	Kind       string
	Workspace  string
	ResourceID string
	Path       string
	Err        error
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Operation
	if message == "" {
		message = "PUA application operation"
	}
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// IsKind reports whether err, or one of its wrapped errors, is an API error of
// the requested kind.
func IsKind(err error, kind string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Kind == kind
}

// Workspace is a reusable, explicitly rooted PUA application handle.
// Workspace values are immutable and safe to share between goroutines. The
// underlying persistent locks are acquired for each operation, so a handle
// does not keep a process-global mutable store open.
type Workspace struct {
	root string
}

// InitializeOptions contains the immutable metadata written when a Workspace
// is first created.
type InitializeOptions struct {
	Language string
}

// OpenWorkspace validates and canonicalizes root without consulting cwd.
func OpenWorkspace(root string) (*Workspace, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace", Err: errors.New("workspace root is required")}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace", Path: root, Err: err}
	}
	abs = filepath.Clean(abs)
	if _, err := workspacepath.ResolveControlDir(abs); err != nil {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace_control", Workspace: abs, Path: abs, Err: err}
	}
	if pathExists(workspaceInitializationMarker(abs)) {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace_initializing", Workspace: abs, Path: abs, Err: errors.New("Workspace initialization is incomplete; rerun pua init to recover it")}
	}
	canonical, err := canonicalWorkspaceRoot(abs)
	if err != nil {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace", Path: abs, Err: err}
	}
	if !isDir(canonical) {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace", Workspace: canonical, Path: canonical, Err: errors.New("workspace root is not a directory")}
	}
	if !hasWorkspaceConfig(canonical) && !isDir(filepath.Join(canonical, reposDir)) {
		return nil, &APIError{Operation: "open Workspace", Kind: "workspace", Workspace: canonical, Path: canonical, Err: errors.New("could not find AgentWorkspace root; run pua init first")}
	}
	return &Workspace{root: canonical}, nil
}

func canonicalWorkspaceRoot(root string) (string, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err == nil {
		return filepath.Clean(canonical), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	return filepath.Clean(root), nil
}

// Initialize creates a Workspace at an explicit path. It does not use or
// change the process working directory and returns an opened handle after the
// durable files have been written.
func Initialize(root, language string) (*Workspace, error) {
	return InitializeWithOptions(root, InitializeOptions{Language: language})
}

// InitializeWithOptions creates a Workspace. Initialization is serialized by
// the Workspace application lock; an on-disk marker makes an interrupted
// initialization recognizable and safely retryable.
func InitializeWithOptions(root string, options InitializeOptions) (*Workspace, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, &APIError{Operation: "initialize Workspace", Kind: "workspace", Err: errors.New("workspace root is required")}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, &APIError{Operation: "initialize Workspace", Kind: "workspace", Path: root, Err: err}
	}
	abs = filepath.Clean(abs)
	if existing, err := findEnclosingWorkspaceRoot(abs); err != nil {
		return nil, &APIError{Operation: "initialize Workspace", Kind: "workspace", Path: abs, Err: err}
	} else if existing != "" && !(filepath.Clean(existing) == abs && pathExists(workspaceInitializationMarker(abs))) {
		return nil, &APIError{Operation: "initialize Workspace", Kind: "workspace", Workspace: existing, Path: abs, Err: fmt.Errorf("cannot initialize workspace inside existing workspace: %s", existing)}
	}
	language, err := normalizeLanguage(strings.TrimSpace(options.Language))
	if err != nil {
		return nil, &APIError{Operation: "initialize Workspace", Kind: "workspace", Path: abs, Err: err}
	}
	if err := withWorkspaceMutationLock(abs, func() error {
		var existingMarker map[string]any
		if err := readJSON(workspaceInitializationMarker(abs), &existingMarker); err != nil && !os.IsNotExist(err) {
			return err
		}
		marker := map[string]any{"version": 1, "updatedAt": time.Now().Format(time.RFC3339Nano)}
		if err := writeJSON(workspaceInitializationMarker(abs), marker); err != nil {
			return err
		}
		if err := initializeWorkspaceLocked(abs, language); err != nil {
			return err
		}
		if err := os.Remove(workspaceInitializationMarker(abs)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return syncDirectory(workspacepath.ControlDir(abs))
	}); err != nil {
		return nil, &APIError{Operation: "initialize Workspace", Kind: "workspace", Path: abs, Err: err}
	}
	return OpenWorkspace(abs)
}

func initializeWorkspaceLocked(root, language string) error {
	if err := os.MkdirAll(filepath.Join(root, reposDir), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(root, archiveDir), 0o755); err != nil {
		return err
	}
	instanceID, err := newWorkspaceInstanceID()
	if err != nil {
		return err
	}
	config := Config{
		Version: 1, Language: language, InstanceID: instanceID, AgentBinding: defaultAgentBinding(),
		GenerationPolicy:    generationPolicyConfig(defaultGenerationPolicy()),
		StallWatchdogPolicy: stallWatchdogPolicyConfig(defaultStallWatchdogPolicy()),
	}
	if err := readJSON(workspaceConfigPath(root), &config); err != nil && !os.IsNotExist(err) {
		return err
	}
	config.Version, config.Language = 1, language
	if strings.TrimSpace(config.InstanceID) == "" {
		config.InstanceID = instanceID
	}
	if strings.TrimSpace(config.AgentBinding.Name) == "" {
		config.AgentBinding = defaultAgentBinding()
	}
	generationPolicy, err := resolveGenerationPolicy(config.GenerationPolicy)
	if err != nil {
		return err
	}
	config.GenerationPolicy = generationPolicyConfig(generationPolicy)
	stallWatchdogPolicy, err := resolveStallWatchdogPolicy(config.StallWatchdogPolicy)
	if err != nil {
		return err
	}
	config.StallWatchdogPolicy = stallWatchdogPolicyConfig(stallWatchdogPolicy)
	if err := writeWorkspaceConfig(root, config); err != nil {
		return err
	}
	if err := ensureWorkspaceWiki(root, language); err != nil {
		return err
	}
	if err := updateAgentsMD(filepath.Join(root, "AGENTS.md"), language); err != nil {
		return err
	}
	if _, err := ensureSchedulerLocked(root, language); err != nil {
		return err
	}
	return updateOpenTaskAgentsMD(root, language)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Migrate refreshes managed Workspace guidance using an explicit root.
func (w *Workspace) Migrate(language string) error {
	if err := w.require(); err != nil {
		return err
	}
	err := withWorkspaceMutationLock(w.root, func() error { return w.migrate(language) })
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return err
		}
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	return nil
}

func (w *Workspace) migrate(language string) error {
	if err := w.require(); err != nil {
		return err
	}
	config, err := readWorkspaceConfig(w.root)
	if err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	if strings.TrimSpace(language) == "" {
		language = config.Language
	}
	language, err = normalizeLanguage(language)
	if err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	if err := ensureWorkspaceWiki(w.root, language); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	if err := updateAgentsMD(filepath.Join(w.root, "AGENTS.md"), language); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	if err := updateOpenTaskAgentsMD(w.root, language); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	if err := migrateSchedulerJSONLocked(w.root); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	if _, err := ensureSchedulerLocked(w.root, language); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "scheduler", Workspace: w.root, Err: err}
	}
	config.Version, config.Language = 1, language
	if strings.TrimSpace(config.InstanceID) == "" {
		config.InstanceID, err = newWorkspaceInstanceID()
		if err != nil {
			return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
		}
	}
	if strings.TrimSpace(config.AgentBinding.Name) == "" {
		config.AgentBinding = defaultAgentBinding()
	}
	generationPolicy, err := resolveGenerationPolicy(config.GenerationPolicy)
	if err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	config.GenerationPolicy = generationPolicyConfig(generationPolicy)
	stallWatchdogPolicy, err := resolveStallWatchdogPolicy(config.StallWatchdogPolicy)
	if err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	config.StallWatchdogPolicy = stallWatchdogPolicyConfig(stallWatchdogPolicy)
	if err := writeWorkspaceConfig(w.root, config); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	if err := ensureOpenResourceBindings(w.root, normalizeResourceDefaults(config.ResourceDefaults)); err != nil {
		return &APIError{Operation: "migrate Workspace", Kind: "workspace", Workspace: w.root, Err: err}
	}
	return nil
}

// Root returns the canonical absolute root selected when the handle was
// opened.
func (w *Workspace) Root() string {
	if w == nil {
		return ""
	}
	return w.root
}

func (w *Workspace) require() error {
	if w == nil || strings.TrimSpace(w.root) == "" {
		return &APIError{Operation: "use Workspace", Kind: "workspace", Err: errors.New("Workspace handle is nil")}
	}
	return nil
}

// ResourceResult contains the typed resource selected by an id and its
// Workspace-relative path. Exactly one of Project or Task is non-nil.
type ResourceResult struct {
	Project  *Project
	Task     *Task
	Path     string
	Archived bool
}

func (r ResourceResult) Resource() Resource {
	if r.Project != nil {
		return r.Project
	}
	if r.Task != nil {
		return r.Task
	}
	return nil
}

// ProjectListEntry is a typed project listing result.
type ProjectListEntry struct {
	Project  Project
	Path     string
	Archived bool
}

// TaskListEntry is a typed task listing result.
type TaskListEntry struct {
	Task     Task
	Path     string
	Archived bool
}

// TaskListOptions controls project task listing.
type TaskListOptions struct {
	ProjectID       string
	IncludeArchived bool
}

// TaskListResult contains the ordinary typed task list.
type TaskListResult struct {
	Tasks []TaskListEntry
}

// CreateProjectInput contains all typed inputs needed to create a project.
type CreateProjectInput struct {
	Description string
	Slug        string
}

// CreateTaskInput contains all typed inputs needed to create a task.
type CreateTaskInput struct {
	ProjectID              string
	Title                  string
	Detail                 string
	CompleteMarkdown       string
	CompleteMarkdownSet    bool
	TemplateName           string
	TemplateFields         map[string]any
	ExpectedTemplateDigest string
	Slug                   string
	AgentBinding           AgentBinding
}

// TaskPreview is the side-effect-free result of resolving task content and
// execution settings. It is also the contract shown by web and CLI previews.
type TaskPreview struct {
	ProjectID    string              `json:"project"`
	Title        string              `json:"title"`
	Slug         string              `json:"slug,omitempty"`
	Markdown     string              `json:"markdown"`
	AgentBinding AgentBinding        `json:"agentBinding"`
	Template     *TaskTemplateSource `json:"template,omitempty"`
	Warnings     []TemplateIssue     `json:"warnings,omitempty"`
}

// ArchiveResult describes an archive operation without relying on printed
// paths. Warnings are non-blocking observations made before or after the
// directory move; a successful result always means the move itself completed.
type ArchiveResult struct {
	ResourceID string           `json:"resourceId"`
	Path       string           `json:"path"`
	Warnings   []ArchiveWarning `json:"warnings,omitempty"`
}

// Tree returns the complete Workspace resource tree.
func (w *Workspace) Tree() (WorkspaceTree, error) {
	if err := w.require(); err != nil {
		return WorkspaceTree{}, err
	}
	tree, err := buildWorkspaceTreeAt(w.root)
	if err != nil {
		return WorkspaceTree{}, &APIError{Operation: "read Workspace tree", Kind: "tree", Workspace: w.root, Err: err}
	}
	return tree, nil
}

// Resource returns detailed resource data for web and service consumers.
func (w *Workspace) Resource(id string) (ResourceDetailView, error) {
	if err := w.require(); err != nil {
		return ResourceDetailView{}, err
	}
	if cleanID(id) == SchedulerResourceID {
		return w.schedulerResourceDetail()
	}
	path, resource, err := loadResource(w.root, cleanID(id))
	if err != nil {
		return ResourceDetailView{}, &APIError{Operation: "read resource", Kind: "resource", Workspace: w.root, ResourceID: id, Err: err}
	}
	detail, err := buildResourceDetailAt(w.root, resourceEntry{Resource: resource, Path: path})
	if err != nil {
		return ResourceDetailView{}, &APIError{Operation: "read resource detail", Kind: "resource", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	return detail, nil
}

// ResourceValue returns the stored Project or Task value and its path.
func (w *Workspace) ResourceValue(id string) (ResourceResult, error) {
	if err := w.require(); err != nil {
		return ResourceResult{}, err
	}
	path, resource, err := loadResource(w.root, cleanID(id))
	if err != nil {
		return ResourceResult{}, &APIError{Operation: "read resource", Kind: "resource", Workspace: w.root, ResourceID: id, Err: err}
	}
	result := ResourceResult{Path: relPath(w.root, path), Archived: isArchivedPath(w.root, path)}
	switch typed := resource.(type) {
	case *Project:
		result.Project = typed
	case *Task:
		result.Task = typed
	default:
		return ResourceResult{}, &APIError{Operation: "read resource", Kind: "resource", Workspace: w.root, ResourceID: id, Err: fmt.Errorf("unsupported resource type %T", resource)}
	}
	return result, nil
}

// Projects lists open projects, optionally including archived projects.
func (w *Workspace) Projects(includeArchived bool) ([]ProjectListEntry, error) {
	if err := w.require(); err != nil {
		return nil, err
	}
	dirs := []string{w.root}
	if includeArchived {
		dirs = append(dirs, filepath.Join(w.root, archiveDir))
	}
	entries, err := readProjectEntriesInDirs(dirs)
	if err != nil {
		return nil, &APIError{Operation: "list projects", Kind: "project", Workspace: w.root, Err: err}
	}
	result := make([]ProjectListEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, ProjectListEntry{Project: entry.Project, Path: relPath(w.root, entry.Path), Archived: isArchivedPath(w.root, entry.Path)})
	}
	return result, nil
}

// Tasks lists tasks for one explicit project. A project id is required so the
// service never needs to infer a resource from cwd.
func (w *Workspace) Tasks(options TaskListOptions) (TaskListResult, error) {
	if err := w.require(); err != nil {
		return TaskListResult{}, err
	}
	parentID := strings.TrimSpace(options.ProjectID)
	if parentID == "" {
		return TaskListResult{}, &APIError{Operation: "list tasks", Kind: "task", Workspace: w.root, Err: errors.New("project id is required")}
	}
	parentPath, err := findResourceDir(w.root, parentID)
	if err != nil {
		return TaskListResult{}, &APIError{Operation: "list tasks", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	dirs := []string{parentPath}
	if options.IncludeArchived {
		dirs = append(dirs, filepath.Join(parentPath, archiveDir))
	}
	entries, err := readTaskEntriesInDirs(dirs, projectTaskName(parentID))
	if err != nil {
		return TaskListResult{}, &APIError{Operation: "list tasks", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	result := TaskListResult{Tasks: make([]TaskListEntry, 0, len(entries))}
	for _, entry := range entries {
		result.Tasks = append(result.Tasks, TaskListEntry{Task: entry.Task, Path: relPath(w.root, entry.Path), Archived: isArchivedPath(w.root, entry.Path)})
	}
	return result, nil
}

// CreateProject creates and returns a typed Project.
func (w *Workspace) CreateProject(description, slug string) (Project, error) {
	return w.CreateProjectWithInput(CreateProjectInput{Description: description, Slug: slug})
}

// CreateProjectWithInput creates a project.
func (w *Workspace) CreateProjectWithInput(input CreateProjectInput) (Project, error) {
	if err := w.require(); err != nil {
		return Project{}, err
	}
	var project Project
	err := withWorkspaceMutationLock(w.root, func() error {
		var err error
		project, err = w.createProject(input)
		return err
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return Project{}, err
		}
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, Err: err}
	}
	return project, nil
}

func (w *Workspace) createProject(input CreateProjectInput) (Project, error) {
	if err := w.require(); err != nil {
		return Project{}, err
	}
	description, slug := input.Description, input.Slug
	description = strings.TrimSpace(description)
	if description == "" {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, Err: errors.New("description cannot be empty")}
	}
	slug, err := normalizeResourceSlug(slug)
	if err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, Err: err}
	}
	id, err := nextProjectID(w.root)
	if err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, Err: err}
	}
	project := newProject(id, titleFromDescription(description), description)
	cfg, err := readWorkspaceConfig(w.root)
	if err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, Err: err}
	}
	project.AgentBinding = normalizeDefaultBinding(cfg.ResourceDefaults.Project)
	language, err := workspaceLanguage(w.root)
	if err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, Err: err}
	}
	path := filepath.Join(w.root, projectDirectoryName(id, slug))
	staging := filepath.Join(w.root, fmt.Sprintf(".pua-create-%s-%d", strings.ReplaceAll(id, ".", "-"), time.Now().UnixNano()))
	defer os.RemoveAll(staging)
	if err := createResourceFiles(staging, &project, language); err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	if err := os.Rename(staging, path); err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	if err := syncDirectory(w.root); err != nil {
		return Project{}, &APIError{Operation: "create project", Kind: "project", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	return project, nil
}

// CreateTask creates and returns a typed Task.
func (w *Workspace) CreateTask(input CreateTaskInput) (Task, error) {
	if err := w.require(); err != nil {
		return Task{}, err
	}
	var task Task
	err := withWorkspaceMutationLock(w.root, func() error {
		var err error
		task, err = w.createTask(input)
		return err
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return Task{}, err
		}
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, Err: err}
	}
	return task, nil
}

// PreviewTask validates and resolves a task creation request without
// allocating a task id or writing files.
func (w *Workspace) PreviewTask(input CreateTaskInput) (TaskPreview, error) {
	if err := w.require(); err != nil {
		return TaskPreview{}, err
	}
	parentID := strings.TrimSpace(input.ProjectID)
	if parentID == "" {
		return TaskPreview{}, &APIError{Operation: "preview task", Kind: "task", Workspace: w.root, Err: errors.New("project id is required")}
	}
	parentPath, err := findResourceDir(w.root, parentID)
	if err != nil {
		return TaskPreview{}, &APIError{Operation: "preview task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	if isArchivedPath(w.root, parentPath) {
		return TaskPreview{}, &APIError{Operation: "preview task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: fmt.Errorf("cannot create task under archived project: %s", parentID)}
	}
	var parent Project
	if err := readProjectAtDir(parentPath, &parent); err != nil {
		return TaskPreview{}, &APIError{Operation: "preview task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	binding, err := w.resolveTaskAgentBinding(parent, input.AgentBinding)
	if err != nil {
		return TaskPreview{}, &APIError{Operation: "preview task", Kind: "binding", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	slug, err := normalizeResourceSlug(input.Slug)
	if err != nil {
		return TaskPreview{}, &APIError{Operation: "preview task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	title, markdown, source, warnings, err := w.resolveTaskContent(input)
	if err != nil {
		return TaskPreview{}, err
	}
	return TaskPreview{ProjectID: parentID, Title: title, Slug: slug, Markdown: markdown, AgentBinding: binding, Template: source, Warnings: warnings}, nil
}

func (w *Workspace) resolveTaskContent(input CreateTaskInput) (string, string, *TaskTemplateSource, []TemplateIssue, error) {
	templateName := strings.TrimSpace(input.TemplateName)
	if templateName != "" {
		if strings.TrimSpace(input.Detail) != "" || input.CompleteMarkdownSet {
			return "", "", nil, nil, &APIError{Operation: "render task template", Kind: "template", Workspace: w.root, ResourceID: input.ProjectID, Err: errors.New("template is mutually exclusive with detail and taskMarkdown")}
		}
		result, err := w.RenderTemplate(TemplateRenderInput{ProjectID: input.ProjectID, Name: templateName, Fields: input.TemplateFields, Title: input.Title})
		if err != nil {
			return "", "", nil, nil, err
		}
		if expected := strings.TrimSpace(input.ExpectedTemplateDigest); expected != "" && expected != result.Digest {
			issue := templateProblem("template_digest_conflict", fmt.Sprintf("template digest changed: expected %s, got %s", expected, result.Digest), "expectedTemplateDigest", nil)
			return "", "", nil, nil, &APIError{Operation: "render task template", Kind: "template_conflict", Workspace: w.root, ResourceID: input.ProjectID, Err: &TemplateValidationError{Template: templateName, Issues: []TemplateIssue{issue}}}
		}
		source := &TaskTemplateSource{Name: result.TemplateName, SchemaVersion: result.SchemaVersion, Digest: result.Digest}
		return result.Title, result.Markdown, source, result.Warnings, nil
	}
	if len(input.TemplateFields) > 0 || strings.TrimSpace(input.ExpectedTemplateDigest) != "" {
		return "", "", nil, nil, &APIError{Operation: "create task", Kind: "template", Workspace: w.root, ResourceID: input.ProjectID, Err: errors.New("templateFields and expectedTemplateDigest require templateName")}
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		return "", "", nil, nil, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: input.ProjectID, Err: errors.New("title cannot be empty")}
	}
	language, err := workspaceLanguage(w.root)
	if err != nil {
		return "", "", nil, nil, err
	}
	markdown := taskMarkdown(title, strings.TrimSpace(input.Detail), language)
	if input.CompleteMarkdownSet {
		markdown = input.CompleteMarkdown
	}
	return title, markdown, nil, nil, nil
}

func (w *Workspace) resolveTaskAgentBinding(parent Project, explicit AgentBinding) (AgentBinding, error) {
	if strings.TrimSpace(explicit.Name) != "" || strings.TrimSpace(explicit.Kind) != "" {
		return NormalizeAgentBinding(explicit)
	}
	if strings.TrimSpace(parent.TaskDefault.Name) != "" {
		// A Project-level Task default overrides the Workspace default.
		return NormalizeAgentBinding(parent.TaskDefault)
	}
	cfg, err := readWorkspaceConfig(w.root)
	if err != nil {
		return AgentBinding{}, err
	}
	return normalizeDefaultBinding(cfg.ResourceDefaults.Task), nil
}

func (w *Workspace) createTask(input CreateTaskInput) (Task, error) {
	if err := w.require(); err != nil {
		return Task{}, err
	}
	parentID := strings.TrimSpace(input.ProjectID)
	if parentID == "" {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, Err: errors.New("project id is required")}
	}
	slug, err := normalizeResourceSlug(input.Slug)
	if err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, Err: err}
	}
	parentPath, err := findResourceDir(w.root, parentID)
	if err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	if isArchivedPath(w.root, parentPath) {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: fmt.Errorf("cannot create task under archived project: %s", parentID)}
	}
	var parent Project
	if err := readProjectAtDir(parentPath, &parent); err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	title, markdown, templateSource, _, err := w.resolveTaskContent(input)
	if err != nil {
		return Task{}, err
	}
	id, err := nextProjectTaskID(parentPath, parentID)
	if err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: parentID, Err: err}
	}
	path := filepath.Join(parentPath, taskDirectoryName(id, slug))
	staging := filepath.Join(parentPath, fmt.Sprintf(".pua-create-%s-%d", strings.ReplaceAll(id, ".", "-"), time.Now().UnixNano()))
	defer os.RemoveAll(staging)
	task := newTask(id, parentID, title, "")
	task.AgentBinding, err = w.resolveTaskAgentBinding(parent, input.AgentBinding)
	if err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "binding", Workspace: w.root, ResourceID: id, Err: err}
	}
	task.Template = templateSource
	language, err := workspaceLanguage(w.root)
	if err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: id, Err: err}
	}
	if err := createResourceFilesWithMarkdown(staging, &task, markdown, language); err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	if err := os.Rename(staging, path); err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	if err := syncDirectory(parentPath); err != nil {
		return Task{}, &APIError{Operation: "create task", Kind: "task", Workspace: w.root, ResourceID: id, Path: relPath(w.root, path), Err: err}
	}
	task.Path = relPath(w.root, path)
	return task, nil
}

// ArchiveResource moves an open project or task into its archive and returns
// the resulting Workspace-relative path plus non-blocking warnings. A Project
// move includes its complete child subtree.
func (w *Workspace) ArchiveResource(id string) (ArchiveResult, error) {
	if err := w.require(); err != nil {
		return ArchiveResult{}, err
	}
	var result ArchiveResult
	err := withWorkspaceMutationLock(w.root, func() error {
		var err error
		result, err = w.archiveResource(id)
		return err
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return ArchiveResult{}, err
		}
		return ArchiveResult{}, &APIError{Operation: "archive resource", Kind: "resource", Workspace: w.root, ResourceID: id, Err: err}
	}
	return result, nil
}

func (w *Workspace) archiveResource(id string) (ArchiveResult, error) {
	if err := w.require(); err != nil {
		return ArchiveResult{}, err
	}
	cleanID := cleanID(id)
	src, resource, err := loadOpenResource(w.root, cleanID)
	if err != nil {
		return ArchiveResult{}, &APIError{Operation: "archive resource", Kind: "resource", Workspace: w.root, ResourceID: cleanID, Err: err}
	}
	dst, err := resourceArchiveDestination(w.root, src, resource)
	if err != nil {
		return ArchiveResult{}, &APIError{Operation: "archive resource", Kind: "resource", Workspace: w.root, ResourceID: cleanID, Err: err}
	}
	if pathExists(dst) {
		return ArchiveResult{}, &APIError{Operation: "archive resource", Kind: "resource", Workspace: w.root, ResourceID: cleanID, Path: relPath(w.root, dst), Err: fmt.Errorf("archive destination already exists: %s", relPath(w.root, dst))}
	}
	var (
		warnings []ArchiveWarning
		children []archiveTaskReference
	)
	if isProject(resource) {
		children, warnings = collectProjectArchiveTasks(w.root, src, *resource.(*Project))
	}
	if task, ok := resource.(*Task); ok {
		warnings = append(warnings, inspectTaskRepoWorktrees(w.root, src, *task)...)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return ArchiveResult{}, &APIError{Operation: "archive resource", Kind: "resource", Workspace: w.root, ResourceID: cleanID, Err: err}
	}
	if err := os.Rename(src, dst); err != nil {
		return ArchiveResult{}, &APIError{Operation: "archive resource", Kind: "resource", Workspace: w.root, ResourceID: cleanID, Err: err}
	}
	for _, directory := range []string{filepath.Dir(src), filepath.Dir(dst)} {
		if err := syncDirectory(directory); err != nil {
			warning := archiveWarning("archive_sync_failed", fmt.Sprintf("archive directory move for %s completed, but syncing %s failed: %v", cleanID, relPath(w.root, directory), err))
			warning.ResourceID = cleanID
			warning.Path = relPath(w.root, directory)
			warnings = append(warnings, warning)
		}
	}
	if task, ok := resource.(*Task); ok {
		warnings = append(warnings, repairArchivedTaskWorktrees(w.root, dst, *task)...)
	} else {
		warnings = append(warnings, archiveTaskReferencesAfterMove(w.root, src, dst, children)...)
	}
	return ArchiveResult{ResourceID: cleanID, Path: relPath(w.root, dst), Warnings: sortArchiveWarnings(warnings)}, nil
}
