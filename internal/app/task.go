package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/localize"
)

var topProjectName = regexp.MustCompile(`^project([0-9]+)$`)
var topProjectDirName = regexp.MustCompile(`^project([0-9]+)(?:-[A-Za-z0-9][A-Za-z0-9._-]*)?$`)
var taskDirName = regexp.MustCompile(`^task([0-9]+)(?:-[A-Za-z0-9][A-Za-z0-9._-]*)?$`)
var resourceSlugName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

const (
	projectJSONFile = "project.json"
	projectMDFile   = "project.md"
	taskJSONFile    = "task.json"
	taskMDFile      = "task.md"
)

type taskListEntry struct {
	Task Task
	Path string
}

type projectListEntry struct {
	Project Project
	Path    string
}

func newProject(id, title, description string) Project {
	now := time.Now().Format(time.RFC3339)
	return Project{
		ResourceMeta: ResourceMeta{SchemaVersion: resourceSchemaVersion, ID: id, Type: resourceTypeProject, Title: strings.TrimSpace(title), CreatedAt: now, UpdatedAt: now, AgentBinding: defaultAgentBinding()},
		Description:  description,
	}
}

func newTask(id, parent, title, description string) Task {
	now := time.Now().Format(time.RFC3339)
	task := Task{
		ResourceMeta: ResourceMeta{
			SchemaVersion: resourceSchemaVersion,
			ID:            id,
			Type:          resourceTypeTask,
			Title:         strings.TrimSpace(title),
			CreatedAt:     now,
			UpdatedAt:     now,
			AgentBinding:  defaultAgentBinding(),
		},
		Parent:         parent,
		Description:    description,
		State:          TaskStateNotStarted,
		StateUpdatedAt: now,
	}
	return task
}

func createResourceFiles(dir string, resource Resource, languages ...string) error {
	language := defaultLanguage
	if len(languages) > 0 {
		language = languages[0]
	}
	return createResourceFilesWithMarkdown(dir, resource, defaultTaskMD(resource, language), language)
}

func createResourceFilesWithMarkdown(dir string, resource Resource, markdown, language string) error {
	if pathExists(dir) {
		return fmt.Errorf("task directory already exists: %s", dir)
	}
	subdirs := []string{"artifacts"}
	if isProject(resource) {
		subdirs = append(subdirs, "templates")
	} else {
		subdirs = append(subdirs, "worktree")
	}
	for _, subdir := range subdirs {
		if err := os.MkdirAll(filepath.Join(dir, subdir), 0o755); err != nil {
			return err
		}
	}
	if err := writeResourceMetadata(dir, resource); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, markdownFileName(resource)), []byte(markdown), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(taskAgentsBlock(resource, language)+"\n"), 0o644)
}

func metadataFileName(resource Resource) string {
	if isProject(resource) {
		return projectJSONFile
	}
	return taskJSONFile
}

func markdownFileName(resource Resource) string {
	if isProject(resource) {
		return projectMDFile
	}
	return taskMDFile
}

func writeResourceMetadata(dir string, resource Resource) error {
	if err := validateResource(resource); err != nil {
		return fmt.Errorf("invalid resource metadata for %s: %w", dir, err)
	}
	path := filepath.Join(dir, metadataFileName(resource))
	if err := writeJSON(path, resource); err != nil {
		return err
	}
	stale := taskJSONFile
	if !isProject(resource) {
		stale = projectJSONFile
	}
	if err := os.Remove(filepath.Join(dir, stale)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readResourceAtDir(dir string) (Resource, error) {
	projectPath := filepath.Join(dir, projectJSONFile)
	taskPath := filepath.Join(dir, taskJSONFile)
	if pathExists(projectPath) && pathExists(taskPath) {
		return nil, fmt.Errorf("resource directory contains both %s and %s: %s", projectJSONFile, taskJSONFile, dir)
	}
	path := taskPath
	expectedType := resourceTypeTask
	if pathExists(projectPath) {
		path = projectPath
		expectedType = resourceTypeProject
	}
	var resource Resource
	if expectedType == resourceTypeProject {
		resource = &Project{}
	} else {
		resource = &Task{}
	}
	if err := readJSON(path, resource); err != nil {
		return nil, err
	}
	meta := resource.resourceMeta()
	if meta.Type != expectedType {
		return nil, fmt.Errorf("invalid resource metadata %s: file requires type %q, got %q", path, expectedType, meta.Type)
	}
	if err := validateResource(resource); err != nil {
		return nil, fmt.Errorf("invalid resource metadata %s: %w", path, err)
	}
	return resource, nil
}

func readProjectAtDir(dir string, project *Project) error {
	resource, err := readResourceAtDir(dir)
	if err != nil {
		return err
	}
	typed, ok := resource.(*Project)
	if !ok {
		return fmt.Errorf("resource is not a project: %s", dir)
	}
	*project = *typed
	return nil
}

func readTaskAtDir(dir string, task *Task) error {
	resource, err := readResourceAtDir(dir)
	if err != nil {
		return err
	}
	typed, ok := resource.(*Task)
	if !ok {
		return fmt.Errorf("resource is not a task: %s", dir)
	}
	*task = *typed
	return nil
}

func nextProjectID(root string) (string, error) {
	maxID := 0
	for _, dir := range []string{root, filepath.Join(root, archiveDir)} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			match := topProjectDirName.FindStringSubmatch(entry.Name())
			if match == nil {
				continue
			}
			n, _ := strconv.Atoi(match[1])
			if n > maxID {
				maxID = n
			}
		}
	}
	return fmt.Sprintf("project%d", maxID+1), nil
}

func nextProjectTaskID(parentPath, parentID string) (string, error) {
	pattern := projectTaskName(parentID)
	maxID := 0
	entries, err := readTaskEntriesInDirs([]string{parentPath, filepath.Join(parentPath, archiveDir)}, pattern)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		suffix := strings.TrimPrefix(entry.Task.ID, parentID+".task")
		parts := strings.Split(suffix, ".")
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		if n > maxID {
			maxID = n
		}
	}
	return fmt.Sprintf("%s.task%d", parentID, maxID+1), nil
}

func readTaskEntriesInDir(dir string, pattern *regexp.Regexp) ([]taskListEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tasks []taskListEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var task Task
		taskPath := filepath.Join(dir, entry.Name())
		if err := readTaskAtDir(taskPath, &task); err != nil {
			continue
		}
		if !pattern.MatchString(task.ID) || !resourceDirNameMatches(entry.Name(), &task) {
			continue
		}
		tasks = append(tasks, taskListEntry{Task: task, Path: taskPath})
	}
	sort.Slice(tasks, func(i, j int) bool {
		return taskSortKey(tasks[i].Task.ID) < taskSortKey(tasks[j].Task.ID)
	})
	return tasks, nil
}

func readProjectEntriesInDir(dir string) ([]projectListEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var projects []projectListEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectPath := filepath.Join(dir, entry.Name())
		var project Project
		if err := readProjectAtDir(projectPath, &project); err != nil {
			continue
		}
		if !resourceDirNameMatches(entry.Name(), &project) {
			continue
		}
		projects = append(projects, projectListEntry{Project: project, Path: projectPath})
	}
	sort.Slice(projects, func(i, j int) bool { return taskSortKey(projects[i].Project.ID) < taskSortKey(projects[j].Project.ID) })
	return projects, nil
}

func readProjectEntriesInDirs(dirs []string) ([]projectListEntry, error) {
	var projects []projectListEntry
	for _, dir := range dirs {
		entries, err := readProjectEntriesInDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		projects = append(projects, entries...)
	}
	sort.Slice(projects, func(i, j int) bool { return taskSortKey(projects[i].Project.ID) < taskSortKey(projects[j].Project.ID) })
	return projects, nil
}

func readTaskEntriesInDirs(dirs []string, pattern *regexp.Regexp) ([]taskListEntry, error) {
	var tasks []taskListEntry
	for _, dir := range dirs {
		dirTasks, err := readTaskEntriesInDir(dir, pattern)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		tasks = append(tasks, dirTasks...)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return taskSortKey(tasks[i].Task.ID) < taskSortKey(tasks[j].Task.ID)
	})
	return tasks, nil
}

func findResourceDir(root, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("resource id cannot be empty")
	}
	parents := []string{root, filepath.Join(root, archiveDir)}
	if projectID, _, ok := strings.Cut(id, ".task"); ok {
		projectPath, err := findResourceDir(root, projectID)
		if err != nil {
			return "", err
		}
		parents = []string{projectPath, filepath.Join(projectPath, archiveDir)}
	}
	var matches []string
	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(parent, entry.Name())
			resource, err := readResourceAtDir(path)
			if err == nil && resource.resourceMeta().ID == id && resourceDirNameMatches(entry.Name(), resource) {
				matches = append(matches, path)
			}
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: %s", ErrResourceNotFound, id)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple resource directories found for %s: %s", id, strings.Join(matches, ", "))
	}
	return matches[0], nil
}

func isArchivedPath(root, path string) bool {
	rel := relPath(root, path)
	if rel == archiveDir || strings.HasPrefix(rel, archiveDir+"/") {
		return true
	}
	for _, part := range strings.Split(rel, "/") {
		if part == archiveDir {
			return true
		}
	}
	return false
}

func updateOpenTaskAgentsMD(root, language string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		if path != root {
			switch entry.Name() {
			case ".git", reposDir, archiveDir, "worktree", "artifacts":
				return filepath.SkipDir
			}
		}

		if !pathExists(filepath.Join(path, projectJSONFile)) && !pathExists(filepath.Join(path, taskJSONFile)) {
			return nil
		}
		resource, err := readResourceAtDir(path)
		if err != nil {
			return nil
		}
		return updateTaskAgentsMD(root, path, resource, language)
	})
}

func updateTaskAgentsMD(root, dir string, resource Resource, language string) error {
	path := filepath.Join(dir, "AGENTS.md")
	block := taskAgentsBlock(resource, language)

	content := ""
	if data, err := os.ReadFile(path); err == nil {
		content = string(data)
	} else if !os.IsNotExist(err) {
		return err
	}
	if strings.TrimSpace(content) == strings.TrimSpace(taskAgentsPrompt(resource, defaultLanguage)) ||
		strings.TrimSpace(content) == strings.TrimSpace(taskAgentsPrompt(resource, languageSimplifiedChinese)) {
		content = ""
	}

	updated, err := upsertManagedBlock(content, block)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

func taskAgentsBlock(resource Resource, language string) string {
	return puaPromptStart + "\n" + taskAgentsPrompt(resource, language) + "\n" + puaPromptEnd
}

func projectTaskName(projectID string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(projectID) + `\.task([0-9]+)$`)
}

func projectDirectoryName(id, slug string) string {
	return withResourceSlug(id, slug)
}

func taskDirectoryName(id string, slug ...string) string {
	projectID, suffix, ok := strings.Cut(id, ".task")
	name := id
	if ok && topProjectName.MatchString(projectID) && suffix != "" {
		name = "task" + suffix
	}
	if len(slug) > 0 {
		return withResourceSlug(name, slug[0])
	}
	return name
}

func withResourceSlug(name, slug string) string {
	if slug == "" {
		return name
	}
	return name + "-" + slug
}

func normalizeResourceSlug(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", nil
	}
	if !resourceSlugName.MatchString(slug) {
		return "", fmt.Errorf("invalid slug %q: use only letters, numbers, dot, underscore, or hyphen, and start with a letter or number", slug)
	}
	return slug, nil
}

func resourceDirNameMatches(name string, resource Resource) bool {
	meta := resource.resourceMeta()
	if isProject(resource) {
		return resourceDirNameID(name, topProjectDirName, "project") == meta.ID
	}
	if _, ok := resource.(*Task); ok {
		if name == meta.ID {
			return true
		}
		return resourceDirNameID(name, taskDirName, "task") == taskDirectoryName(meta.ID)
	}
	return false
}

func resourceDirNameID(name string, pattern *regexp.Regexp, prefix string) string {
	match := pattern.FindStringSubmatch(name)
	if match == nil {
		return ""
	}
	return prefix + match[1]
}

func isProject(resource Resource) bool {
	_, ok := resource.(*Project)
	return ok
}

func taskSortKey(id string) string {
	parts := regexp.MustCompile(`[0-9]+`).FindAllString(id, -1)
	var b strings.Builder
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			b.WriteString(part)
			continue
		}
		b.WriteString(fmt.Sprintf("%08d.", n))
	}
	return b.String()
}

func titleFromDescription(description string) string {
	description = strings.TrimSpace(strings.Split(description, "\n")[0])
	runes := []rune(description)
	if len(runes) <= 80 {
		return description
	}
	return string(runes[:77]) + "..."
}

func defaultTaskMD(resource Resource, language string) string {
	meta := resource.resourceMeta()
	description := ""
	switch typed := resource.(type) {
	case *Project:
		description = typed.Description
	case *Task:
		description = typed.Description
	}
	name := "task.md"
	if isProject(resource) {
		name = "project.md"
	}
	return localize.MustRender(language, name, map[string]string{
		"Title": meta.Title, "Detail": strings.TrimSpace(description),
	})
}

func taskMarkdown(title string, detail string, language string) string {
	return localize.MustRender(language, "task.md", map[string]string{
		"Title": title, "Detail": strings.TrimSpace(detail),
	})
}

func taskAgentsPrompt(resource Resource, language string) string {
	meta := resource.resourceMeta()
	name := "task-agents.md"
	if isProject(resource) {
		name = "project-agents.md"
	}
	return localize.MustRender(language, name, map[string]string{"ResourceID": meta.ID})
}
