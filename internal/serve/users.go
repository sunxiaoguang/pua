package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	osuser "os/user"
	"path/filepath"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

const workspaceUserHeader = "X-PUA-User"

func (s *server) workspaceUserName(r *http.Request, workspacePath string) (string, error) {
	name := strings.TrimSpace(r.Header.Get(workspaceUserHeader))
	if name == "" {
		return "", &resourceAPIError{Code: "user_required", Message: "select a Workspace user before accessing personal data"}
	}
	if err := app.ValidateUserName(name); err != nil {
		return "", &resourceAPIError{Code: "invalid_request", Message: err.Error()}
	}
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return "", err
	}
	if _, err := workspace.User(name); err != nil {
		return "", &resourceAPIError{Code: "user_not_found", Message: fmt.Sprintf("Workspace user not found: %s", name)}
	}
	return name, nil
}

func suggestedSystemUserName() string {
	current, err := osuser.Current()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(current.Username)
	if app.ValidateUserName(name) != nil {
		return ""
	}
	return name
}

func (s *server) handleUsers(w http.ResponseWriter, r *http.Request, workspaceID string, parts []string) {
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	if len(parts) >= 2 && parts[0] != "" && parts[1] == "messages" {
		s.agents.handleUserMessages(w, r, workspaceID, parts[0], parts[2:])
		return
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			users, err := puaWorkspace.Users()
			if err != nil {
				writeError(w, err, http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"users": users})
		case http.MethodPost:
			var body struct {
				Name string `json:"name"`
			}
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body); err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			var profile app.UserProfile
			err := s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
				opened, openErr := app.OpenWorkspace(current.Path)
				if openErr != nil {
					return openErr
				}
				profile, openErr = opened.RegisterUser(body.Name)
				if openErr != nil {
					return openErr
				}
				return s.ensureUserUIStateBaseline(current.Path, profile.Name)
			})
			if err != nil {
				writeError(w, err, http.StatusBadRequest)
				return
			}
			writeJSON(w, profile)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) != 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	name := parts[0]
	if err := app.ValidateUserName(name); err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Preference string `json:"preference"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		var profile app.UserProfile
		err := s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
			opened, openErr := app.OpenWorkspace(current.Path)
			if openErr != nil {
				return openErr
			}
			profile, openErr = opened.UpdateUserPreference(name, body.Preference)
			return openErr
		})
		if err != nil {
			writeError(w, err, http.StatusNotFound)
			return
		}
		writeJSON(w, profile)
	case http.MethodDelete:
		err := s.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
			opened, openErr := app.OpenWorkspace(current.Path)
			if openErr != nil {
				return openErr
			}
			return opened.DeleteUser(name)
		})
		if err != nil {
			if errors.Is(err, app.ErrLastUser) {
				writeError(w, &resourceAPIError{Code: "last_user", Message: app.ErrLastUser.Error()}, http.StatusConflict)
				return
			}
			writeError(w, err, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *server) ensureWorkspaceUsersAndMigrateUIState(workspacePath string) error {
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return err
	}

	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	shared, err := loadResourceStateFile(resourceStatePath(workspacePath))
	if err != nil {
		return err
	}
	sharedChanged := false
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return err
	}
	for resourceID, record := range latestTurnGenerationByResource(records) {
		if record.TurnNumber > shared.TurnNumbers[resourceID] {
			shared.TurnNumbers[resourceID] = record.TurnNumber
			sharedChanged = true
		}
	}
	legacyPaths := []string{uiStatePath(workspacePath), filepath.Join(workspacepath.ControlDir(workspacePath), "gui-state.json")}
	migratedPaths := make([]string, 0, len(legacyPaths))
	var legacyDefaultState *uiState
	for _, legacy := range legacyPaths {
		if _, err := os.Stat(legacy); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		migratedPaths = append(migratedPaths, legacy)
		state, err := loadUIStateFile(legacy)
		if err != nil {
			return fmt.Errorf("migrate legacy UI state: %w", err)
		}
		for resourceID, attention := range state.Attention {
			if attention.TurnNumber > shared.TurnNumbers[resourceID] {
				shared.TurnNumbers[resourceID] = attention.TurnNumber
				sharedChanged = true
			}
		}
		if legacyDefaultState == nil {
			cloned := state
			legacyDefaultState = &cloned
		}
	}
	if sharedChanged {
		if err := saveResourceStateFile(resourceStatePath(workspacePath), shared); err != nil {
			return err
		}
	}
	completedTurnNumbers := completedTurnBaseline(shared.TurnNumbers, records)
	users, err := workspace.Users()
	if err != nil {
		return err
	}
	legacyTarget := ""
	for _, user := range users {
		if user.Name == app.LegacyDefaultUserName {
			legacyTarget = user.Name
			break
		}
	}
	if legacyTarget == "" && len(users) == 1 {
		legacyTarget = users[0].Name
	}
	legacyConsumed := legacyDefaultState == nil
	for _, user := range users {
		statePath := userUIStatePath(workspacePath, user.Name)
		_, statErr := os.Stat(statePath)
		stateExists := statErr == nil
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		var state uiState
		if !stateExists && user.Name == legacyTarget && legacyDefaultState != nil {
			state = *legacyDefaultState
			legacyConsumed = true
		} else {
			state, err = loadUIStateFile(statePath)
			if err != nil {
				return err
			}
		}
		if !stateExists || state.Version < 2 {
			seedUnreadBaseline(&state, completedTurnNumbers)
			if err := saveUIStateFile(statePath, state); err != nil {
				return err
			}
		}
	}
	if legacyConsumed {
		for _, legacy := range migratedPaths {
			_ = os.Remove(legacy)
		}
	}
	return nil
}

func seedUnreadBaseline(state *uiState, turnNumbers map[string]int) {
	if state.ResourceStates == nil {
		state.ResourceStates = map[string]resourceUserState{}
	}
	for resourceID, turnNumber := range turnNumbers {
		if turnNumber <= 0 {
			continue
		}
		if _, exists := state.ResourceStates[resourceID]; exists {
			continue
		}
		state.ResourceStates[resourceID] = resourceUserState{ReadTurnNumber: cloneIntPointer(&turnNumber)}
	}
}

func (s *server) ensureUserUIStateBaseline(workspacePath, userName string) error {
	workspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return err
	}
	users, err := workspace.Users()
	if err != nil {
		return err
	}
	consumeLegacy := userName == app.LegacyDefaultUserName || len(users) == 1

	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	statePath := userUIStatePath(workspacePath, userName)
	_, statErr := os.Stat(statePath)
	stateExists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return statErr
	}
	state, err := loadUIStateFile(statePath)
	if err != nil {
		return err
	}
	if stateExists && state.Version >= 2 {
		return nil
	}
	legacyPaths := []string{uiStatePath(workspacePath), filepath.Join(workspacepath.ControlDir(workspacePath), "gui-state.json")}
	migratedPaths := make([]string, 0, len(legacyPaths))
	if !stateExists && consumeLegacy {
		for _, legacy := range legacyPaths {
			if _, statErr := os.Stat(legacy); os.IsNotExist(statErr) {
				continue
			} else if statErr != nil {
				return statErr
			}
			legacyState, loadErr := loadUIStateFile(legacy)
			if loadErr != nil {
				return fmt.Errorf("migrate legacy UI state: %w", loadErr)
			}
			migratedPaths = append(migratedPaths, legacy)
			if len(migratedPaths) == 1 {
				state = legacyState
			}
		}
	}
	shared, err := loadResourceStateFile(resourceStatePath(workspacePath))
	if err != nil {
		return err
	}
	sharedChanged := false
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return err
	}
	for resourceID, record := range latestTurnGenerationByResource(records) {
		if record.TurnNumber > shared.TurnNumbers[resourceID] {
			shared.TurnNumbers[resourceID] = record.TurnNumber
			sharedChanged = true
		}
	}
	for resourceID, attention := range state.Attention {
		if attention.TurnNumber > shared.TurnNumbers[resourceID] {
			shared.TurnNumbers[resourceID] = attention.TurnNumber
			sharedChanged = true
		}
	}
	if sharedChanged {
		if err := saveResourceStateFile(resourceStatePath(workspacePath), shared); err != nil {
			return err
		}
	}
	seedUnreadBaseline(&state, completedTurnBaseline(shared.TurnNumbers, records))
	if err := saveUIStateFile(statePath, state); err != nil {
		return err
	}
	for _, legacy := range migratedPaths {
		_ = os.Remove(legacy)
	}
	return nil
}

func userUIStatePath(workspacePath, userName string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "users", userName, "ui-state.json")
}

func resourceStatePath(workspacePath string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "resource-state.json")
}
