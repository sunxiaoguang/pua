package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/workspacepath"
)

// userInbox is the durable per-user store for agent-to-user messages. It is
// the outbound counterpart of the resource mailbox: delivery completes when
// the user reads the message in the Web UI, and a reply re-enters the ordinary
// resource mailbox as a role=user message addressed to the source resource.
const userInboxVersion = 1

// maxUserInboxMessages bounds the retained inbox. Oldest read messages are
// pruned first; unread messages are never dropped by retention.
const maxUserInboxMessages = 200

type userInbox struct {
	Version      int                `json:"version"`
	NextSequence uint64             `json:"nextSequence"`
	Messages     []userInboxMessage `json:"messages"`
}

type userInboxMessage struct {
	ID                        string `json:"id"`
	Sequence                  uint64 `json:"sequence"`
	Text                      string `json:"text"`
	SourceResourceID          string `json:"sourceResourceId"`
	SourceWorkspaceInstanceID string `json:"sourceWorkspaceInstanceId,omitempty"`
	SenderName                string `json:"senderName,omitempty"`
	CreatedAt                 string `json:"createdAt"`
	ReadAt                    string `json:"readAt,omitempty"`
	RepliedAt                 string `json:"repliedAt,omitempty"`
}

type userInboxMessageResponse struct {
	MessageID        string `json:"messageId"`
	User             string `json:"user"`
	Text             string `json:"text"`
	SourceResourceID string `json:"sourceResourceId"`
	SenderName       string `json:"senderName,omitempty"`
	CreatedAt        string `json:"createdAt"`
	ReadAt           string `json:"readAt,omitempty"`
	RepliedAt        string `json:"repliedAt,omitempty"`
	Unread           bool   `json:"unread"`
	Reference        string `json:"reference"`
}

type userInboxSendRequest struct {
	Text                      string                 `json:"text"`
	Sender                    *agentHubMessageSender `json:"sender,omitempty"`
	SenderWorkspaceInstanceID string                 `json:"senderWorkspaceInstanceId,omitempty"`
}

var userInboxMu sync.Mutex

func userInboxPath(workspacePath, userName string) string {
	return filepath.Join(workspacepath.ControlDir(workspacePath), "users", userName, "inbox.json")
}

func loadUserInboxLocked(workspacePath, userName string) (userInbox, error) {
	data, err := os.ReadFile(userInboxPath(workspacePath, userName))
	if err != nil {
		if os.IsNotExist(err) {
			return userInbox{Version: userInboxVersion, Messages: []userInboxMessage{}}, nil
		}
		return userInbox{}, err
	}
	var inbox userInbox
	if err := json.Unmarshal(data, &inbox); err != nil {
		return userInbox{}, fmt.Errorf("read user inbox: %w", err)
	}
	if inbox.Version != userInboxVersion {
		return userInbox{}, fmt.Errorf("unsupported user inbox version %d", inbox.Version)
	}
	if inbox.Messages == nil {
		inbox.Messages = []userInboxMessage{}
	}
	for _, message := range inbox.Messages {
		if message.Sequence > inbox.NextSequence {
			inbox.NextSequence = message.Sequence
		}
	}
	return inbox, nil
}

func writeUserInboxLocked(workspacePath, userName string, inbox userInbox) error {
	if err := os.MkdirAll(filepath.Dir(userInboxPath(workspacePath, userName)), 0o755); err != nil {
		return err
	}
	inbox.Version = userInboxVersion
	if inbox.Messages == nil {
		inbox.Messages = []userInboxMessage{}
	}
	sort.SliceStable(inbox.Messages, func(i, j int) bool {
		if inbox.Messages[i].Sequence != inbox.Messages[j].Sequence {
			return inbox.Messages[i].Sequence < inbox.Messages[j].Sequence
		}
		return inbox.Messages[i].ID < inbox.Messages[j].ID
	})
	data, err := json.MarshalIndent(inbox, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := userInboxPath(workspacePath, userName)
	tmp := path + "." + newGenerationRecordID() + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(tmp)
		}
	}()
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	remove = false
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func mutateUserInbox(workspacePath, userName string, mutate func(*userInbox) error) (userInbox, error) {
	userInboxMu.Lock()
	defer userInboxMu.Unlock()
	inbox, err := loadUserInboxLocked(workspacePath, userName)
	if err != nil {
		return userInbox{}, err
	}
	if err := mutate(&inbox); err != nil {
		return userInbox{}, err
	}
	if err := writeUserInboxLocked(workspacePath, userName, inbox); err != nil {
		return userInbox{}, err
	}
	return inbox, nil
}

// pruneUserInboxLocked keeps at most maxUserInboxMessages entries, dropping
// the oldest read messages first so unread delivery is never lost to
// retention.
func pruneUserInboxLocked(inbox *userInbox) {
	for len(inbox.Messages) > maxUserInboxMessages {
		oldestRead := -1
		for index, message := range inbox.Messages {
			if message.ReadAt != "" {
				oldestRead = index
				break
			}
		}
		if oldestRead < 0 {
			oldestRead = 0
		}
		inbox.Messages = append(inbox.Messages[:oldestRead], inbox.Messages[oldestRead+1:]...)
	}
}

func buildUserInboxMessageResponse(workspaceID, userName string, message userInboxMessage) userInboxMessageResponse {
	return userInboxMessageResponse{
		MessageID: message.ID, User: userName,
		Text: message.Text, SourceResourceID: message.SourceResourceID, SenderName: message.SenderName,
		CreatedAt: message.CreatedAt, ReadAt: message.ReadAt, RepliedAt: message.RepliedAt,
		Unread:    message.ReadAt == "",
		Reference: fmt.Sprintf("/api/workspaces/%s/users/%s/messages/%s", workspaceID, userName, message.ID),
	}
}

func (m *agentManager) handleUserMessages(w http.ResponseWriter, r *http.Request, workspaceID, userName string, parts []string) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	if err := app.ValidateUserName(userName); err != nil {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodGet:
			m.listUserInboxMessages(w, workspaceID, workspace.Path, userName)
		case http.MethodPost:
			m.acceptUserInboxMessage(w, r, workspaceID, workspace, userName)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
		return
	}
	if len(parts) == 2 && parts[1] == "read" && r.Method == http.MethodPut {
		m.markUserInboxMessageRead(w, r, workspaceID, workspace, userName, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "reply" && r.Method == http.MethodPost {
		m.replyUserInboxMessage(w, r, workspaceID, workspace, userName, parts[0])
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		m.deleteUserInboxMessage(w, r, workspace, userName, parts[0])
		return
	}
	http.NotFound(w, r)
}

func (m *agentManager) listUserInboxMessages(w http.ResponseWriter, workspaceID, workspacePath, userName string) {
	userInboxMu.Lock()
	inbox, err := loadUserInboxLocked(workspacePath, userName)
	userInboxMu.Unlock()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	messages := make([]userInboxMessageResponse, 0, len(inbox.Messages))
	for _, message := range inbox.Messages {
		messages = append(messages, buildUserInboxMessageResponse(workspaceID, userName, message))
	}
	writeJSON(w, map[string]any{"user": userName, "messages": messages})
}

// acceptUserInboxMessage is the agent-to-user send boundary. The sender must
// be a stable resource of this Workspace with matching instance provenance;
// the message is durably appended to the target user's inbox. Delivery to the
// user completes when the Web UI marks the message read.
func (m *agentManager) acceptUserInboxMessage(w http.ResponseWriter, r *http.Request, workspaceID string, workspace serveWorkspace, userName string) {
	if err := m.server.requireWorkspaceOwnership(workspace.Path); err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusForbidden)
		return
	}
	var request userInboxSendRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(request.Text)
	if text == "" {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: "text is required"}, http.StatusBadRequest)
		return
	}
	if request.Sender == nil || !isStablePUAResourceID(strings.TrimSpace(request.Sender.ID)) {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: "sender.id must be a stable PUA resource id"}, http.StatusBadRequest)
		return
	}
	instanceID := strings.TrimSpace(request.SenderWorkspaceInstanceID)
	if instanceID == "" {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: "senderWorkspaceInstanceId is required"}, http.StatusBadRequest)
		return
	}
	currentInstanceID, err := workspaceInstanceID(workspace.Path)
	if err != nil || currentInstanceID != instanceID {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: "sender workspace instance does not match this workspace"}, http.StatusBadRequest)
		return
	}
	sourceResourceID := normalizedResourceID(request.Sender.ID)
	exists, archived, _, err := resourceExistsAndArchived(workspace.Path, sourceResourceID)
	if err != nil || !exists {
		writeError(w, &resourceAPIError{Code: "resource_not_found", Message: fmt.Sprintf("sender resource not found: %s", sourceResourceID)}, http.StatusNotFound)
		return
	}
	if archived {
		writeError(w, &resourceAPIError{Code: "resource_archived", Message: fmt.Sprintf("sender resource %s is archived", sourceResourceID)}, http.StatusBadRequest)
		return
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if _, err := puaWorkspace.User(userName); err != nil {
		writeError(w, &resourceAPIError{Code: "user_not_found", Message: fmt.Sprintf("user not found: %s", userName)}, http.StatusNotFound)
		return
	}
	now := time.Now().Format(time.RFC3339Nano)
	senderName := strings.TrimSpace(request.Sender.Name)
	if senderName == "" {
		senderName = sourceResourceID
	}
	message := userInboxMessage{
		ID: "umsg-" + newGenerationRecordID(), Text: text,
		SourceResourceID: sourceResourceID, SourceWorkspaceInstanceID: instanceID,
		SenderName: senderName, CreatedAt: now,
	}
	err = m.server.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		_, mutateErr := mutateUserInbox(current.Path, userName, func(inbox *userInbox) error {
			inbox.NextSequence++
			message.Sequence = inbox.NextSequence
			inbox.Messages = append(inbox.Messages, message)
			pruneUserInboxLocked(inbox)
			return nil
		})
		return mutateErr
	})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	response := buildUserInboxMessageResponse(workspaceID, userName, message)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}

func (m *agentManager) markUserInboxMessageRead(w http.ResponseWriter, r *http.Request, workspaceID string, workspace serveWorkspace, userName, messageID string) {
	messageID = strings.TrimSpace(messageID)
	now := time.Now().Format(time.RFC3339Nano)
	var updated userInboxMessage
	found := false
	err := m.server.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		_, mutateErr := mutateUserInbox(current.Path, userName, func(inbox *userInbox) error {
			for index := range inbox.Messages {
				if inbox.Messages[index].ID != messageID {
					continue
				}
				if inbox.Messages[index].ReadAt == "" {
					inbox.Messages[index].ReadAt = now
				}
				updated, found = inbox.Messages[index], true
				return nil
			}
			return nil
		})
		return mutateErr
	})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if !found {
		writeError(w, &resourceAPIError{Code: "message_not_found", Message: fmt.Sprintf("user inbox message not found: %s", messageID)}, http.StatusNotFound)
		return
	}
	writeJSON(w, buildUserInboxMessageResponse(workspaceID, userName, updated))
}

// replyUserInboxMessage turns a user's inline reply into an ordinary
// role=user resource mailbox message addressed to the source resource, so
// delivery, generation wake-up, and steer/enqueue handling reuse the existing
// mailbox pipeline unchanged. The delivered text quotes the original message
// so the receiving agent can tell which inbox message is being answered. The
// inbox entry records the reply time.
func (m *agentManager) replyUserInboxMessage(w http.ResponseWriter, r *http.Request, workspaceID string, workspace serveWorkspace, userName, messageID string) {
	var request struct {
		Text string `json:"text"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: "text is required"}, http.StatusBadRequest)
		return
	}
	messageID = strings.TrimSpace(messageID)
	userInboxMu.Lock()
	inbox, err := loadUserInboxLocked(workspace.Path, userName)
	userInboxMu.Unlock()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	var source userInboxMessage
	found := false
	for _, message := range inbox.Messages {
		if message.ID == messageID {
			source, found = message, true
			break
		}
	}
	if !found {
		writeError(w, &resourceAPIError{Code: "message_not_found", Message: fmt.Sprintf("user inbox message not found: %s", messageID)}, http.StatusNotFound)
		return
	}
	accepted, err := m.acceptResourceMessage(r.Context(), workspace, source.SourceResourceID, resourceMessageRequest{
		Text: userInboxReplyText(source, request.Text), Role: "user",
		Sender: &agentHubMessageSender{Name: userName},
	})
	if err != nil {
		var apiErr *resourceAPIError
		if errors.As(err, &apiErr) && apiErr.Code == "resource_archived" {
			writeError(w, &resourceAPIError{Code: "resource_archived", Message: "the source resource is archived and can no longer receive replies"}, http.StatusBadRequest)
			return
		}
		writeError(w, err, resourceErrorStatus(err))
		return
	}
	now := time.Now().Format(time.RFC3339Nano)
	_ = m.server.withWorkspaceMutation(context.WithoutCancel(r.Context()), workspace, "workspace", func(current serveWorkspace) error {
		_, mutateErr := mutateUserInbox(current.Path, userName, func(inbox *userInbox) error {
			for index := range inbox.Messages {
				if inbox.Messages[index].ID == messageID {
					if inbox.Messages[index].RepliedAt == "" {
						inbox.Messages[index].RepliedAt = now
					}
					if inbox.Messages[index].ReadAt == "" {
						inbox.Messages[index].ReadAt = now
					}
				}
			}
			return nil
		})
		return mutateErr
	})
	response := mailboxMessageResponse(accepted)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspaceID, accepted.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}

func (m *agentManager) deleteUserInboxMessage(w http.ResponseWriter, r *http.Request, workspace serveWorkspace, userName, messageID string) {
	messageID = strings.TrimSpace(messageID)
	found := false
	err := m.server.withWorkspaceMutation(r.Context(), workspace, "workspace", func(current serveWorkspace) error {
		_, mutateErr := mutateUserInbox(current.Path, userName, func(inbox *userInbox) error {
			for index := range inbox.Messages {
				if inbox.Messages[index].ID != messageID {
					continue
				}
				inbox.Messages = append(inbox.Messages[:index], inbox.Messages[index+1:]...)
				found = true
				return nil
			}
			return nil
		})
		return mutateErr
	})
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	if !found {
		writeError(w, &resourceAPIError{Code: "message_not_found", Message: fmt.Sprintf("user inbox message not found: %s", messageID)}, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxInboxReplyQuoteBytes bounds the original-message excerpt embedded in a
// reply so a very long inbox message cannot flood the agent's prompt.
const maxInboxReplyQuoteBytes = 1024

// userInboxReplyText prefixes the user's reply with a quoted excerpt of the
// original agent message. Agents only ever see message text, so the quote is
// the reliable way to show which inbox message the reply answers.
func userInboxReplyText(source userInboxMessage, reply string) string {
	quote := strings.TrimSpace(source.Text)
	if len(quote) > maxInboxReplyQuoteBytes {
		quote = quote[:maxInboxReplyQuoteBytes] + "\n[original message truncated]"
	}
	lines := strings.Split(quote, "\n")
	for index, line := range lines {
		lines[index] = "> " + line
	}
	return fmt.Sprintf("[Reply to your Inbox message %s]\n%s\n\n%s", source.ID, strings.Join(lines, "\n"), strings.TrimSpace(reply))
}
