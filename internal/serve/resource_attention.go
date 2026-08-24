package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

// resourceAttentionState is the version 1 persisted shape. It is read only
// while migrating read cursors to the version 2 resource state model.
type resourceAttentionState struct {
	ReadTurnNumber *int `json:"readTurnNumber,omitempty"`
	DismissedTurn  *int `json:"dismissedTurn,omitempty"`
	TurnNumber     int  `json:"turnNumber,omitempty"`
}

type resourceUserState struct {
	ReadTurnNumber *int `json:"readTurnNumber,omitempty"`
}

type resourceUserStateSnapshot struct {
	ReadTurnNumber *int `json:"readTurnNumber,omitempty"`
}

type resourceActivityLists struct {
	Running  []resourceSnapshot `json:"running"`
	Unread   []resourceSnapshot `json:"unread"`
	Problems []resourceSnapshot `json:"problems"`
}

func resourceUserStateSnapshotForState(state resourceUserState) *resourceUserStateSnapshot {
	return &resourceUserStateSnapshot{ReadTurnNumber: cloneIntPointer(state.ReadTurnNumber)}
}

type resourceState struct {
	Version     int            `json:"version"`
	TurnNumbers map[string]int `json:"turnNumbers,omitempty"`
}

func cloneIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func loadUIStateFile(path string) (uiState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return uiState{Version: 2, ExpandedProjects: []string{}, ResourceStates: map[string]resourceUserState{}}, nil
		}
		return uiState{}, err
	}
	var state uiState
	if err := json.Unmarshal(data, &state); err != nil {
		return uiState{}, err
	}
	if state.Version == 0 {
		state.Version = 1
	}
	if state.ExpandedProjects == nil {
		state.ExpandedProjects = []string{}
	}
	if state.ResourceStates == nil {
		state.ResourceStates = map[string]resourceUserState{}
	}
	for resourceID, attention := range state.Attention {
		if attention.ReadTurnNumber == nil && attention.DismissedTurn != nil {
			attention.ReadTurnNumber = cloneIntPointer(attention.DismissedTurn)
		}
		state.Attention[resourceID] = attention
		if _, exists := state.ResourceStates[resourceID]; !exists && attention.ReadTurnNumber != nil {
			state.ResourceStates[resourceID] = resourceUserState{
				ReadTurnNumber: cloneIntPointer(attention.ReadTurnNumber),
			}
		}
	}
	return state, nil
}

func saveUIStateFile(path string, state uiState) error {
	state.Version = 2
	state.ExpandedProjects = uniqueNonEmpty(state.ExpandedProjects)
	state.Folders, state.FolderOrder = normalizeUIStateFolders(state.Folders, state.FolderOrder)
	if state.ResourceStates == nil {
		state.ResourceStates = map[string]resourceUserState{}
	}
	for resourceID, resourceState := range state.ResourceStates {
		if resourceState.ReadTurnNumber == nil {
			delete(state.ResourceStates, resourceID)
		}
	}
	state.Attention = nil
	return saveJSONStateFile(path, ".ui-state-*.tmp", state)
}

func saveJSONStateFile(path, pattern string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return nil
}

func loadResourceStateFile(path string) (resourceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return resourceState{Version: 1, TurnNumbers: map[string]int{}}, nil
		}
		return resourceState{}, err
	}
	var state resourceState
	if err := json.Unmarshal(data, &state); err != nil {
		return resourceState{}, err
	}
	state.Version = 1
	if state.TurnNumbers == nil {
		state.TurnNumbers = map[string]int{}
	}
	return state, nil
}

func saveResourceStateFile(path string, state resourceState) error {
	state.Version = 1
	if state.TurnNumbers == nil {
		state.TurnNumbers = map[string]int{}
	}
	return saveJSONStateFile(path, ".resource-state-*.tmp", state)
}

func selectedUserName(userNames []string) string {
	if len(userNames) > 0 && strings.TrimSpace(userNames[0]) != "" {
		return userNames[0]
	}
	return ""
}

func (s *server) loadResourceStatesAtPath(path string, userNames ...string) (map[string]resourceUserState, error) {
	userName := selectedUserName(userNames)
	if userName == "" {
		return map[string]resourceUserState{}, nil
	}
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	state, err := loadUIStateFile(userUIStatePath(path, userName))
	if err != nil {
		return nil, err
	}
	return state.ResourceStates, nil
}

func (s *server) mutateResourceUserStateAtPath(path, resourceID string, mutate func(*resourceUserState), userNames ...string) (resourceUserState, error) {
	userName := selectedUserName(userNames)
	if userName == "" {
		return resourceUserState{}, &resourceAPIError{Code: "user_required", Message: "select a Workspace user before accessing personal data"}
	}
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	statePath := userUIStatePath(path, userName)
	state, err := loadUIStateFile(statePath)
	if err != nil {
		return resourceUserState{}, err
	}
	if state.ResourceStates == nil {
		state.ResourceStates = map[string]resourceUserState{}
	}
	resourceID = normalizedResourceID(resourceID)
	resourceState := state.ResourceStates[resourceID]
	mutate(&resourceState)
	if resourceState.ReadTurnNumber == nil {
		delete(state.ResourceStates, resourceID)
	} else {
		state.ResourceStates[resourceID] = resourceState
	}
	if err := saveUIStateFile(statePath, state); err != nil {
		return resourceUserState{}, err
	}
	return resourceState, nil
}

// pruneUIStateForArchivedResources removes persisted UI state entries that
// reference resources removed by an archive, so read cursors, expansion state
// and custom ordering cannot leak into a resource that later reuses the ID.
func (s *server) pruneUIStateForArchivedResources(workspacePath string, resourceIDs []string) error {
	if len(resourceIDs) == 0 {
		return nil
	}
	archived := make(map[string]bool, len(resourceIDs))
	for _, id := range resourceIDs {
		archived[normalizedResourceID(id)] = true
	}
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return err
	}
	users, err := workspace.Users()
	if err != nil {
		return err
	}
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	for _, user := range users {
		statePath := userUIStatePath(workspacePath, user.Name)
		state, err := loadUIStateFile(statePath)
		if err != nil {
			return err
		}
		state, changed := prunedUIState(state, archived)
		if changed {
			if err := saveUIStateFile(statePath, state); err != nil {
				return err
			}
		}
	}
	sharedPath := resourceStatePath(workspacePath)
	shared, err := loadResourceStateFile(sharedPath)
	if err != nil {
		return err
	}
	sharedChanged := false
	for id := range shared.TurnNumbers {
		if archived[id] {
			delete(shared.TurnNumbers, id)
			sharedChanged = true
		}
	}
	if sharedChanged {
		return saveResourceStateFile(sharedPath, shared)
	}
	return nil
}

func prunedUIState(state uiState, archived map[string]bool) (uiState, bool) {
	changed := false
	for id := range state.ResourceStates {
		if archived[id] {
			delete(state.ResourceStates, id)
			changed = true
		}
	}
	for id := range state.Attention {
		if archived[id] {
			delete(state.Attention, id)
			changed = true
		}
	}
	if kept, dropped := dropArchivedResourceIDs(state.ExpandedProjects, archived); dropped {
		state.ExpandedProjects = kept
		changed = true
	}
	if kept, dropped := dropArchivedResourceIDs(state.ProjectOrder, archived); dropped {
		state.ProjectOrder = kept
		changed = true
	}
	for projectID, order := range state.TaskOrder {
		if archived[projectID] {
			delete(state.TaskOrder, projectID)
			changed = true
			continue
		}
		if kept, dropped := dropArchivedResourceIDs(order, archived); dropped {
			state.TaskOrder[projectID] = kept
			changed = true
		}
	}
	keptFolders := state.Folders[:0]
	for _, folder := range state.Folders {
		if archived[folder.ProjectID] {
			delete(state.FolderOrder, folder.ID)
			changed = true
			continue
		}
		keptFolders = append(keptFolders, folder)
	}
	if len(keptFolders) != len(state.Folders) {
		state.Folders = keptFolders
		changed = true
	}
	for folderID, order := range state.FolderOrder {
		if kept, dropped := dropArchivedResourceIDs(order, archived); dropped {
			state.FolderOrder[folderID] = kept
			changed = true
		}
	}
	if archived[state.LastResourceID] {
		state.LastResourceID = ""
		changed = true
	}
	return state, changed
}

// uiStateFolderNameMaxLength caps persisted virtual folder names.
const uiStateFolderNameMaxLength = 80

// normalizeUIStateFolders keeps the persisted folder list well-formed: folder
// IDs are unique and non-empty, names are trimmed and capped, and folderOrder
// only references folders that still exist.
func normalizeUIStateFolders(folders []uiStateFolder, order map[string][]string) ([]uiStateFolder, map[string][]string) {
	seen := make(map[string]bool, len(folders))
	kept := make([]uiStateFolder, 0, len(folders))
	for _, folder := range folders {
		folder.ID = strings.TrimSpace(folder.ID)
		folder.ProjectID = strings.TrimSpace(folder.ProjectID)
		folder.Name = strings.TrimSpace(folder.Name)
		if folder.ID == "" || folder.ProjectID == "" || seen[folder.ID] {
			continue
		}
		seen[folder.ID] = true
		if len(folder.Name) > uiStateFolderNameMaxLength {
			folder.Name = folder.Name[:uiStateFolderNameMaxLength]
		}
		kept = append(kept, folder)
	}
	if len(order) > 0 {
		cleaned := make(map[string][]string, len(order))
		for folderID, ids := range order {
			if seen[folderID] {
				cleaned[folderID] = ids
			}
		}
		order = cleaned
	}
	return kept, order
}

func dropArchivedResourceIDs(ids []string, archived map[string]bool) ([]string, bool) {
	dropped := false
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if archived[id] {
			dropped = true
			continue
		}
		kept = append(kept, id)
	}
	if !dropped {
		return ids, false
	}
	return kept, true
}

// allocateResourceTurnNumber advances the resource-wide turn ordinal. It is
// separate from the generation record because a replacement generation must
// not reset the read boundary of the resource.
func (s *server) allocateResourceTurnNumber(path, resourceID string) (int, error) {
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	statePath := resourceStatePath(path)
	state, err := loadResourceStateFile(statePath)
	if err != nil {
		return 0, err
	}
	resourceID = normalizedResourceID(resourceID)
	maximum := state.TurnNumbers[resourceID]
	records, err := loadGenerationRecords(path)
	if err != nil {
		return 0, err
	}
	for _, record := range records {
		candidateID := normalizedResourceID(record.ResourceID)
		if candidateID == resourceID && record.TurnNumber > maximum {
			maximum = record.TurnNumber
		}
	}
	state.TurnNumbers[resourceID] = maximum + 1
	if err := saveResourceStateFile(statePath, state); err != nil {
		return 0, err
	}
	return state.TurnNumbers[resourceID], nil
}

func (s *server) handleResourceRead(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	if r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	resourceID = normalizedResourceID(resourceID)
	userName, err := s.workspaceUserName(r, workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	var body struct {
		ThroughTurnNumber *int `json:"throughTurnNumber"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.ThroughTurnNumber == nil {
		if err == nil {
			err = errors.New("throughTurnNumber is required")
		}
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if *body.ThroughTurnNumber < 0 {
		writeError(w, errors.New("throughTurnNumber must not be negative"), http.StatusBadRequest)
		return
	}
	var resourceState resourceUserState
	errorStatus := http.StatusInternalServerError
	err = s.withWorkspaceMutation(r.Context(), workspace, resourceID, func(current serveWorkspace) error {
		if validateErr := validateAttentionResource(current.Path, resourceID); validateErr != nil {
			errorStatus = http.StatusBadRequest
			return validateErr
		}
		currentTurnNumber, currentErr := s.currentCompletedResourceTurnNumber(current.Path, resourceID)
		if currentErr != nil {
			return currentErr
		}
		if *body.ThroughTurnNumber > currentTurnNumber {
			errorStatus = http.StatusBadRequest
			return fmt.Errorf("throughTurnNumber %d exceeds current completed Turn %d", *body.ThroughTurnNumber, currentTurnNumber)
		}
		resourceState, currentErr = s.mutateResourceUserStateAtPath(current.Path, resourceID, func(state *resourceUserState) {
			if state.ReadTurnNumber == nil || *state.ReadTurnNumber < *body.ThroughTurnNumber {
				state.ReadTurnNumber = cloneIntPointer(body.ThroughTurnNumber)
			}
		}, userName)
		return currentErr
	})
	if err != nil {
		writeError(w, err, errorStatus)
		return
	}
	writeJSON(w, resourceUserStateSnapshotForState(resourceState))
}

// markResourceReadOnUserMessage clears a resource's unread state when a user
// sends it a message: sending implies the sender has seen every Turn completed
// so far, and the Turn the message triggers becomes the next unread one once
// it completes. The read state is per user, so the acting user must be known.
func (s *server) markResourceReadOnUserMessage(workspacePath, resourceID, userName string) {
	completed, err := s.currentCompletedResourceTurnNumber(workspacePath, resourceID)
	if err != nil || completed <= 0 {
		return
	}
	_, _ = s.mutateResourceUserStateAtPath(workspacePath, resourceID, func(state *resourceUserState) {
		if state.ReadTurnNumber == nil || *state.ReadTurnNumber < completed {
			state.ReadTurnNumber = cloneIntPointer(&completed)
		}
	}, userName)
}

func (s *server) resourceUserStateForResource(path, resourceID string, userNames ...string) (resourceUserState, error) {
	resourceStates, err := s.loadResourceStatesAtPath(path, userNames...)
	if err != nil {
		return resourceUserState{}, err
	}
	return resourceStates[normalizedResourceID(resourceID)], nil
}

func validateAttentionResource(path, resourceID string) error {
	exists, archived, _, err := resourceExistsAndArchived(path, normalizedResourceID(resourceID))
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("resource not found: %s", resourceID)
	}
	if archived {
		return fmt.Errorf("resource %s is archived", resourceID)
	}
	return nil
}

func latestTurnGenerationByResource(records []generationRecord) map[string]generationRecord {
	byResourceID := make(map[string]generationRecord)
	for _, record := range records {
		if strings.TrimSpace(record.GenerationID) == "" || !isAgentHubGeneration(record) || record.TurnNumber <= 0 {
			continue
		}
		resourceID := normalizedResourceID(record.ResourceID)
		if resourceID == "" {
			resourceID = "workspace"
		}
		if current, ok := byResourceID[resourceID]; !ok || record.TurnNumber > current.TurnNumber ||
			record.TurnNumber == current.TurnNumber && resourceRuntimeGenerationNewer(record, current) {
			byResourceID[resourceID] = record
		}
	}
	return byResourceID
}

type completedResourceTurn struct {
	Record     generationRecord
	TurnNumber int
	Exact      bool
}

func completedTurnNumberForGeneration(record generationRecord) int {
	turnNumber := record.TurnNumber
	if generationHasActiveTurn(record) && turnNumber > 0 {
		turnNumber--
	}
	return turnNumber
}

func latestCompletedTurnByResource(records []generationRecord) map[string]completedResourceTurn {
	byResourceID := make(map[string]completedResourceTurn)
	for _, record := range records {
		if strings.TrimSpace(record.GenerationID) == "" || !isAgentHubGeneration(record) {
			continue
		}
		turnNumber := completedTurnNumberForGeneration(record)
		if turnNumber <= 0 {
			continue
		}
		resourceID := normalizedResourceID(record.ResourceID)
		if resourceID == "" {
			resourceID = "workspace"
		}
		candidate := completedResourceTurn{Record: record, TurnNumber: turnNumber, Exact: !generationHasActiveTurn(record)}
		current, ok := byResourceID[resourceID]
		if !ok || candidate.TurnNumber > current.TurnNumber ||
			candidate.TurnNumber == current.TurnNumber && candidate.Exact && !current.Exact ||
			candidate.TurnNumber == current.TurnNumber && candidate.Exact == current.Exact && resourceRuntimeGenerationNewer(candidate.Record, current.Record) {
			byResourceID[resourceID] = candidate
		}
	}
	return byResourceID
}

func completedTurnNumbersByResource(records []generationRecord) map[string]int {
	completed := latestCompletedTurnByResource(records)
	turnNumbers := make(map[string]int, len(completed))
	for resourceID, turn := range completed {
		turnNumbers[resourceID] = turn.TurnNumber
	}
	return turnNumbers
}

func completedTurnBaseline(turnNumbers map[string]int, records []generationRecord) map[string]int {
	baseline := make(map[string]int, len(turnNumbers))
	for resourceID, turnNumber := range turnNumbers {
		baseline[resourceID] = turnNumber
	}
	completed := completedTurnNumbersByResource(records)
	for resourceID := range latestTurnGenerationByResource(records) {
		if turnNumber := completed[resourceID]; turnNumber > 0 {
			baseline[resourceID] = turnNumber
		} else {
			delete(baseline, resourceID)
		}
	}
	return baseline
}

func (s *server) currentCompletedResourceTurnNumber(workspacePath, resourceID string) (int, error) {
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return 0, err
	}
	resourceID = normalizedResourceID(resourceID)
	if turn, ok := latestCompletedTurnByResource(records)[resourceID]; ok {
		return turn.TurnNumber, nil
	}
	return 0, nil
}

func resourceRuntimeSnapshotForGeneration(record generationRecord) *resourceRuntimeSnapshot {
	return &resourceRuntimeSnapshot{
		Generation: record.Generation, GenerationID: record.GenerationID, Status: record.Status,
		SessionState: publicSessionState(false, "", &resourceGenerationStatus{Status: record.Status}, nil, ""),
		AgentName:    record.AgentHubAgentName, UpdatedAt: record.UpdatedAt, LastOutputAt: record.LastOutputAt,
		CompletionMarker: record.CompletionMarker, CompletionState: record.CompletionState, CompletionHasFinalReply: record.CompletionHasFinalReply,
		CompletionAt: record.CompletionAt, ReplacementPending: record.ReplacementPending,
		Resumable:         (record.Status == "stopped" || record.Status == "idle-suspended") && record.AgentHubSessionID != "" && !record.SessionResumeUnavailable && !record.ReplacementPending && !record.ArchivedTaskStopRequested,
		IdleSuspended:     record.Status == "idle-suspended" || (record.IdleSleepStopRequested && record.Status == "stopped"),
		ResumeUnavailable: record.SessionResumeUnavailable,
		TurnNumber:        record.TurnNumber, ActiveTurn: generationHasActiveTurn(record), TurnStartedAt: record.TurnStartedAt,
	}
}

func resourceActivitySortTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func sortResourceActivity(items []resourceSnapshot, timestamp func(resourceSnapshot) string) {
	sort.SliceStable(items, func(i, j int) bool {
		leftTime := resourceActivitySortTime(timestamp(items[i]))
		rightTime := resourceActivitySortTime(timestamp(items[j]))
		if !leftTime.Equal(rightTime) {
			return leftTime.After(rightTime)
		}
		if items[i].Title != items[j].Title {
			return items[i].Title < items[j].Title
		}
		return items[i].ID < items[j].ID
	})
}

func (s *server) enrichTreeResourceActivity(workspacePath string, tree *workspaceTree, userNames ...string) error {
	resourceStates, err := s.loadResourceStatesAtPath(workspacePath, userNames...)
	if err != nil {
		return fmt.Errorf("load user resource state for tree: %w", err)
	}
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return fmt.Errorf("load resource generations for activity: %w", err)
	}
	latestCompletedTurns := latestCompletedTurnByResource(records)
	var applyState func(*resourceSnapshot)
	applyState = func(item *resourceSnapshot) {
		resourceID := normalizedResourceID(item.ID)
		state, hasState := resourceStates[resourceID]
		if hasState {
			item.UserState = resourceUserStateSnapshotForState(state)
		}
		if completed, ok := latestCompletedTurns[resourceID]; ok {
			item.LatestTurnNumber = completed.TurnNumber
			record := completed.Record
			item.LatestTurnAt = record.CompletionAt
			if item.LatestTurnAt == "" {
				item.LatestTurnAt = record.TurnStartedAt
			}
			item.LatestAgentName = record.AgentHubAgentName
		}
		readTurnNumber := 0
		if state.ReadTurnNumber != nil {
			readTurnNumber = *state.ReadTurnNumber
		}
		// The Scheduler is an automation entry point rather than a
		// conversation the user reads, so it never counts as unread.
		if resourceID != app.SchedulerResourceID && item.LatestTurnNumber > readTurnNumber {
			item.UnreadCount = item.LatestTurnNumber - readTurnNumber
		}
		for i := range item.Children {
			applyState(&item.Children[i])
		}
	}
	applyState(&tree.Workspace)
	applyState(&tree.Scheduler)
	for i := range tree.Projects {
		applyState(&tree.Projects[i])
	}
	candidates := make([]resourceSnapshot, 0, 2+len(tree.Projects))
	candidates = append(candidates, tree.Workspace, tree.Scheduler)
	for _, project := range tree.Projects {
		project.Children = append([]resourceSnapshot(nil), project.Children...)
		candidates = append(candidates, project)
		candidates = append(candidates, project.Children...)
	}
	tree.Activity = resourceActivityLists{
		Running: make([]resourceSnapshot, 0),
		Unread:  make([]resourceSnapshot, 0), Problems: make([]resourceSnapshot, 0),
	}
	for _, item := range candidates {
		if item.Archived {
			continue
		}
		item.Children = nil
		if item.Runtime != nil && item.Runtime.ActiveTurn {
			tree.Activity.Running = append(tree.Activity.Running, item)
		}
		if item.UnreadCount > 0 {
			tree.Activity.Unread = append(tree.Activity.Unread, item)
		}
		if item.Type == "task" && (item.State == app.TaskStateBlocked || item.State == app.TaskStateError) {
			tree.Activity.Problems = append(tree.Activity.Problems, item)
		}
	}
	sortResourceActivity(tree.Activity.Running, func(item resourceSnapshot) string {
		if item.Runtime != nil {
			return item.Runtime.TurnStartedAt
		}
		return ""
	})
	sortResourceActivity(tree.Activity.Unread, func(item resourceSnapshot) string { return item.LatestTurnAt })
	sortResourceActivity(tree.Activity.Problems, func(item resourceSnapshot) string { return item.StateUpdatedAt })
	return nil
}
