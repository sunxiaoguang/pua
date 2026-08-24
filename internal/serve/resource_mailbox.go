package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

const resourceMailboxVersion = 2

const (
	resourceMessageModeSteer     = "steer"
	resourceMessageModeEnqueue   = "enqueue"
	resourceMessageModeInterrupt = "interrupt"

	resourceMessageQueued          = "queued"
	resourceMessageDelivering      = "delivering"
	resourceMessageInterrupting    = "interrupting"
	resourceMessageDelivered       = "delivered"
	resourceMessageCancelled       = "cancelled"
	resourceMessageUndeliverable   = "undeliverable"
	resourceMessageDeliveryUnknown = "delivery_unknown"
)

const (
	resourceResultSubscriptionPending  = "pending"
	resourceResultSubscriptionDisabled = "disabled"
	resourceResultSubscriptionNone     = "none"
	resourceResultSubscriptionComplete = "complete"

	resourceMessageTypeTurnResult         = "turn_result"
	resourceMessageTypeDeliveryTerminal   = "delivery_terminal_notice"
	resourceMessageTypeSchedulerTick      = "scheduler_tick"
	resourceMessageTypeScheduleOccurrence = "schedule_occurrence"
	resourceMessageTypeScheduleMigration  = "schedule_migration"
	resourceMessageTypeTaskContinuation   = "task_state_continuation"
	resourceMessageTypeTurnStallRecovery  = "turn_stall_recovery"

	resourceNotificationWaiting   = "waiting"
	resourceNotificationAccepted  = "accepted"
	resourceNotificationDelivered = "delivered"
	resourceNotificationTerminal  = "terminal"
)

const (
	resourceMessageReasonNoActiveTurn        = "no_active_turn"
	resourceMessageReasonSteerUnsupported    = "steer_unsupported"
	resourceMessageReasonGenerationReplacing = "generation_replacing"
	resourceMessageReasonResourceArchived    = "resource_archived"
	resourceMessageReasonRecoveredCanonical  = "recovered_canonical_mode"
	resourceMessageReasonTurnStopped         = "cancelled_by_turn_stop"
)

type resourceMailbox struct {
	Version      int                      `json:"version"`
	NextSequence uint64                   `json:"nextSequence"`
	Messages     []resourceMailboxMessage `json:"messages"`
}

// providerMessageContext freezes the PUA-owned prompt presentation chosen at
// the delivery boundary. It is deliberately separate from puaMessagePayload:
// AgentHub persists that payload opaquely for history, while this context is
// only PUA's deterministic retry input.
type providerMessageContext struct {
	Language       string                 `json:"language"`
	TurnID         string                 `json:"turnId,omitempty"`
	OpenerRole     string                 `json:"openerRole"`
	OpenerSender   *agentHubMessageSender `json:"openerSender,omitempty"`
	OpenerResponse string                 `json:"openerResponse"`
	OpenerUnknown  bool                   `json:"openerUnknown,omitempty"`
}

// resourceMailboxMessage is the durable PUA-side ownership record for one
// accepted resource message. Delivery is complete when AgentHub has durably
// accepted the stable message id and assumed its at-least-once responsibility;
// it does not mean the resulting Turn has completed.
type resourceMailboxMessage struct {
	ID                        string                       `json:"id"`
	Sequence                  uint64                       `json:"sequence"`
	ResourceID                string                       `json:"resourceId"`
	Text                      string                       `json:"text"`
	Role                      string                       `json:"role"`
	Sender                    *agentHubMessageSender       `json:"sender,omitempty"`
	SenderWorkspaceInstanceID string                       `json:"senderWorkspaceInstanceId,omitempty"`
	SubscribeResult           bool                         `json:"subscribeResult"`
	ResultSubscriptionStatus  string                       `json:"resultSubscriptionStatus,omitempty"`
	ResultOperationID         string                       `json:"resultOperationId,omitempty"`
	Type                      string                       `json:"type,omitempty"`
	Causation                 *resourceMessageCausation    `json:"causation,omitempty"`
	Notification              *resourceNotificationReceipt `json:"notification,omitempty"`
	RequestedMode             string                       `json:"requestedMode"`
	ActualMode                string                       `json:"actualMode"`
	ModeFrozen                bool                         `json:"modeFrozen,omitempty"`
	NonPromotable             bool                         `json:"nonPromotable,omitempty"`
	DowngradeReason           string                       `json:"downgradeReason,omitempty"`
	Status                    string                       `json:"status"`
	AcceptedAt                string                       `json:"acceptedAt"`
	UpdatedAt                 string                       `json:"updatedAt"`
	DeliveredAt               string                       `json:"deliveredAt,omitempty"`
	TerminalAt                string                       `json:"terminalAt,omitempty"`
	GenerationID              string                       `json:"generationId,omitempty"`
	AgentHubSessionID         string                       `json:"agentHubSessionId,omitempty"`
	TurnID                    string                       `json:"turnId,omitempty"`
	ProviderContext           *providerMessageContext      `json:"providerContext,omitempty"`
	TurnTerminalAt            string                       `json:"turnTerminalAt,omitempty"`
	InterruptTurnID           string                       `json:"interruptTurnId,omitempty"`
	InterruptAt               string                       `json:"interruptAt,omitempty"`
	PromotedAt                string                       `json:"promotedAt,omitempty"`
	AttemptCount              int                          `json:"attemptCount,omitempty"`
	TaskStartFailureCount     int                          `json:"taskStartFailureCount,omitempty"`
	LastAttemptAt             string                       `json:"lastAttemptAt,omitempty"`
	LastError                 string                       `json:"lastError,omitempty"`
	LastErrorCode             string                       `json:"lastErrorCode,omitempty"`
	receipt                   bool
	subscribeResultPresent    bool
}

// UnmarshalJSON keeps the public default (omitted subscribeResult means true)
// for messages written by older PUA versions while preserving explicit false.
func (message *resourceMailboxMessage) UnmarshalJSON(data []byte) error {
	type mailboxMessageAlias resourceMailboxMessage
	var decoded mailboxMessageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, present := fields["subscribeResult"]
	decoded.subscribeResultPresent = present
	if !present {
		decoded.SubscribeResult = true
	}
	if decoded.Role == "system" {
		decoded.SubscribeResult = false
		decoded.ResultSubscriptionStatus = resourceResultSubscriptionDisabled
	}
	*message = resourceMailboxMessage(decoded)
	return nil
}

type resourceMessageCausation struct {
	Type                      string   `json:"type"`
	SourceWorkspaceInstanceID string   `json:"sourceWorkspaceInstanceId"`
	SourceResourceID          string   `json:"sourceResourceId"`
	MessageID                 string   `json:"messageId,omitempty"`
	SourceMessageIDs          []string `json:"sourceMessageIds,omitempty"`
	GenerationID              string   `json:"generationId,omitempty"`
	TurnID                    string   `json:"turnId,omitempty"`
	TurnReference             string   `json:"turnReference,omitempty"`
	TurnStatus                string   `json:"turnStatus,omitempty"`
	HistoryUnavailable        bool     `json:"historyUnavailable,omitempty"`
	TerminalCode              string   `json:"terminalCode,omitempty"`
	Reason                    string   `json:"reason,omitempty"`
	ScheduleDigest            string   `json:"scheduleDigest,omitempty"`
	ScheduleID                string   `json:"scheduleId,omitempty"`
	ScheduleRevision          uint64   `json:"scheduleRevision,omitempty"`
	OccurrenceID              string   `json:"occurrenceId,omitempty"`
	ScheduledFor              string   `json:"scheduledFor,omitempty"`
	CoalescedFrom             string   `json:"coalescedFrom,omitempty"`
	CoalescedThrough          string   `json:"coalescedThrough,omitempty"`
	CoalescedCount            int      `json:"coalescedCount,omitempty"`
	CronEnumerationCapped     bool     `json:"cronEnumerationCapped,omitempty"`
	EnumeratedThrough         string   `json:"enumeratedThrough,omitempty"`
	EnumeratedCount           int      `json:"enumeratedCount,omitempty"`
	RecoveryCutoff            string   `json:"recoveryCutoff,omitempty"`
}

type resourceNotificationReceipt struct {
	ID                        string `json:"id"`
	Type                      string `json:"type"`
	Status                    string `json:"status"`
	TargetWorkspaceInstanceID string `json:"targetWorkspaceInstanceId"`
	TargetResourceID          string `json:"targetResourceId"`
	CreatedAt                 string `json:"createdAt"`
	UpdatedAt                 string `json:"updatedAt"`
	AcceptedAt                string `json:"acceptedAt,omitempty"`
	DeliveryStatus            string `json:"deliveryStatus,omitempty"`
	DeliveredAt               string `json:"deliveredAt,omitempty"`
	TerminalAt                string `json:"terminalAt,omitempty"`
	LastError                 string `json:"lastError,omitempty"`
	LastErrorCode             string `json:"lastErrorCode,omitempty"`
}

type resourceMailboxCounts struct {
	Waiting         int `json:"waiting"`
	Delivering      int `json:"delivering"`
	Interrupting    int `json:"interrupting"`
	Delivered       int `json:"delivered"`
	Cancelled       int `json:"cancelled"`
	Undeliverable   int `json:"undeliverable"`
	DeliveryUnknown int `json:"deliveryUnknown"`
}

type resourceGenerationStatus struct {
	Generation              int    `json:"generation"`
	GenerationID            string `json:"generationId"`
	Status                  string `json:"status"`
	CompletionState         string `json:"completionState,omitempty"`
	CompletionHasFinalReply bool   `json:"completionHasFinalReply"`
	ReplacementPending      bool   `json:"replacementPending"`
	Resumable               bool   `json:"resumable,omitempty"`
	IdleSuspended           bool   `json:"idleSuspended,omitempty"`
	ResumeUnavailable       bool   `json:"resumeUnavailable,omitempty"`
	IdleSinceAt             string `json:"idleSinceAt,omitempty"`
	IdleDeadlineAt          string `json:"idleDeadlineAt,omitempty"`
	IdleSleepRequested      bool   `json:"idleSleepRequested,omitempty"`
	TurnNumber              int    `json:"turnNumber,omitempty"`
	AgentHubSessionID       string `json:"agentHubSessionId,omitempty"`
}

type resourceSessionStatus struct {
	ID                string                    `json:"id,omitempty"`
	State             string                    `json:"state,omitempty"`
	CurrentTurnID     string                    `json:"currentTurnId,omitempty"`
	InputCapabilities agentHubInputCapabilities `json:"inputCapabilities"`
}

type resourceStatusResponse struct {
	ResourceID      string                    `json:"resourceId"`
	SessionState    string                    `json:"sessionState"`
	Exists          bool                      `json:"exists"`
	Archived        bool                      `json:"archived"`
	AcceptsMessages bool                      `json:"acceptsMessages"`
	Binding         app.AgentBinding          `json:"binding"`
	ResolvedAgent   string                    `json:"resolvedAgent,omitempty"`
	ResolvedProfile string                    `json:"resolvedProfile,omitempty"`
	ConfigError     string                    `json:"configError,omitempty"`
	Generation      *resourceGenerationStatus `json:"generation,omitempty"`
	Session         *resourceSessionStatus    `json:"session,omitempty"`
	Messages        resourceMailboxCounts     `json:"messages"`
	WaitingMessages []resourceMessageResponse `json:"waitingMessages"`
	CanSteerWaiting bool                      `json:"canSteerWaiting"`
	LastError       string                    `json:"lastError,omitempty"`
	LastErrorCode   string                    `json:"lastErrorCode,omitempty"`
}

type resourceMessageRequest struct {
	Text                      string                 `json:"text"`
	Mode                      string                 `json:"mode,omitempty"`
	Role                      string                 `json:"role,omitempty"`
	Sender                    *agentHubMessageSender `json:"sender,omitempty"`
	SenderWorkspaceInstanceID string                 `json:"senderWorkspaceInstanceId,omitempty"`
	SubscribeResult           *bool                  `json:"subscribeResult,omitempty"`
}

type resourceMessageResponse struct {
	MessageID                string                       `json:"messageId"`
	ResourceID               string                       `json:"resourceId"`
	Text                     string                       `json:"text,omitempty"`
	Receipt                  bool                         `json:"receipt,omitempty"`
	RequestedMode            string                       `json:"requestedMode"`
	ActualMode               string                       `json:"actualMode"`
	DowngradeReason          string                       `json:"downgradeReason,omitempty"`
	Status                   string                       `json:"status"`
	AcceptedAt               string                       `json:"acceptedAt"`
	PromotedAt               string                       `json:"promotedAt,omitempty"`
	Reference                string                       `json:"reference"`
	GenerationID             string                       `json:"generationId,omitempty"`
	AgentHubSessionID        string                       `json:"agentHubSessionId,omitempty"`
	TurnID                   string                       `json:"turnId,omitempty"`
	SubscribeResult          bool                         `json:"subscribeResult"`
	ResultSubscriptionStatus string                       `json:"resultSubscriptionStatus,omitempty"`
	ResultOperationID        string                       `json:"resultOperationId,omitempty"`
	LastError                string                       `json:"lastError,omitempty"`
	LastErrorCode            string                       `json:"lastErrorCode,omitempty"`
	Type                     string                       `json:"type,omitempty"`
	Causation                *resourceMessageCausation    `json:"causation,omitempty"`
	Notification             *resourceNotificationReceipt `json:"notification,omitempty"`
	CanPromote               bool                         `json:"canPromote"`
}

type resourceAPIError struct {
	Code    string
	Message string
}

func (e *resourceAPIError) Error() string { return e.Message }

func isStablePUAResourceID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "workspace" || value == app.SchedulerResourceID {
		return true
	}
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 2 || !numericResourcePart(parts[0], "project") {
		return false
	}
	return len(parts) == 1 || numericResourcePart(parts[1], "task")
}

func numericResourcePart(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	digits := strings.TrimPrefix(value, prefix)
	if digits == "" {
		return false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func bindMailboxResultSubscription(message *resourceMailboxMessage, turnID string) {
	if !message.SubscribeResult {
		message.ResultSubscriptionStatus = resourceResultSubscriptionDisabled
		message.ResultOperationID = ""
		return
	}
	// A steer joins an already-running Turn; it does not own that Turn's final
	// result. Callers that need a direct reply to a steer must receive a new
	// explicit resource message from the target agent.
	if message.ActualMode == resourceMessageModeSteer {
		message.ResultSubscriptionStatus = resourceResultSubscriptionNone
		message.ResultOperationID = ""
		return
	}
	if message.Role != "agent" || message.Sender == nil || !isStablePUAResourceID(message.Sender.ID) ||
		strings.TrimSpace(message.SenderWorkspaceInstanceID) == "" || strings.TrimSpace(turnID) == "" {
		message.ResultSubscriptionStatus = resourceResultSubscriptionNone
		message.ResultOperationID = ""
		return
	}
	message.ResultSubscriptionStatus = resourceResultSubscriptionPending
	message.ResultOperationID = ""
}

func normalizeResourceMessageMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return resourceMessageModeSteer, nil
	}
	switch mode {
	case resourceMessageModeSteer, resourceMessageModeEnqueue, resourceMessageModeInterrupt:
		return mode, nil
	default:
		return "", &resourceAPIError{Code: "invalid_request", Message: "mode must be steer, enqueue, or interrupt"}
	}
}

func normalizeResourceMessageRole(role string) (string, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return "user", nil
	}
	switch role {
	case "user", "agent", "system":
		return role, nil
	default:
		return "", &resourceAPIError{Code: "invalid_request", Message: "role must be user, agent, or system"}
	}
}

func cloneMailboxMessage(message resourceMailboxMessage) resourceMailboxMessage {
	cloned := message
	if message.Sender != nil {
		sender := *message.Sender
		cloned.Sender = &sender
	}
	if message.Causation != nil {
		causation := *message.Causation
		cloned.Causation = &causation
	}
	if message.Notification != nil {
		notification := *message.Notification
		cloned.Notification = &notification
	}
	if message.ProviderContext != nil {
		context := *message.ProviderContext
		if context.OpenerSender != nil {
			sender := *context.OpenerSender
			context.OpenerSender = &sender
		}
		cloned.ProviderContext = &context
	}
	return cloned
}

func normalizedResourceID(resourceID string) string {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return "workspace"
	}
	return resourceID
}

func mailboxMessageResponse(message resourceMailboxMessage) resourceMessageResponse {
	return resourceMessageResponse{
		MessageID: message.ID, ResourceID: message.ResourceID,
		Text: message.Text, Receipt: message.receipt,
		RequestedMode: message.RequestedMode, ActualMode: message.ActualMode,
		DowngradeReason: message.DowngradeReason, Status: publicResourceMessageStatus(message.Status),
		AcceptedAt: message.AcceptedAt, PromotedAt: message.PromotedAt,
		Reference: "messages/" + message.ID, GenerationID: message.GenerationID,
		AgentHubSessionID: message.AgentHubSessionID, TurnID: message.TurnID,
		SubscribeResult: message.SubscribeResult, ResultSubscriptionStatus: message.ResultSubscriptionStatus, ResultOperationID: message.ResultOperationID,
		LastError: message.LastError, LastErrorCode: message.LastErrorCode,
		Type: message.Type, Causation: message.Causation, Notification: message.Notification,
		CanPromote: message.Status == resourceMessageQueued && !message.NonPromotable,
	}
}

func publicResourceMessageStatus(status string) string {
	if status == resourceMessageQueued {
		return "waiting"
	}
	return status
}

func mailboxCounts(mailbox resourceMailbox, resourceID string) (resourceMailboxCounts, string, string) {
	resourceID = normalizedResourceID(resourceID)
	var counts resourceMailboxCounts
	lastError, lastErrorCode := "", ""
	for _, message := range mailbox.Messages {
		if normalizedResourceID(message.ResourceID) != resourceID {
			continue
		}
		switch message.Status {
		case resourceMessageQueued:
			counts.Waiting++
		case resourceMessageDelivering:
			counts.Delivering++
		case resourceMessageInterrupting:
			counts.Interrupting++
		case resourceMessageDelivered:
			counts.Delivered++
		case resourceMessageCancelled:
			counts.Cancelled++
		case resourceMessageUndeliverable:
			counts.Undeliverable++
		case resourceMessageDeliveryUnknown:
			counts.DeliveryUnknown++
		}
		if strings.TrimSpace(message.LastError) != "" {
			lastError = message.LastError
			lastErrorCode = message.LastErrorCode
		}
	}
	return counts, lastError, lastErrorCode
}

type cancelledResourceMessages struct {
	Count int
	IDs   []string
}

// cancelPendingSteerMessages records the stop policy at the mailbox boundary.
// Only queued steer requests are cancelled: a delivering or delivered message
// may already have crossed the AgentHub acceptance boundary and must not be
// described as cancelled without a canonical receipt proving that fact.
func cancelPendingSteerMessages(workspacePath, resourceID string) (cancelledResourceMessages, error) {
	resourceID = normalizedResourceID(resourceID)
	now := time.Now().Format(time.RFC3339Nano)
	result := cancelledResourceMessages{IDs: []string{}}
	_, err := mutateResourceMailboxForResource(workspacePath, resourceID, func(mailbox *resourceMailbox) error {
		for index := range mailbox.Messages {
			message := &mailbox.Messages[index]
			if normalizedResourceID(message.ResourceID) != resourceID || message.Status != resourceMessageQueued || message.RequestedMode != resourceMessageModeSteer {
				continue
			}
			message.Status = resourceMessageCancelled
			message.TerminalAt = now
			message.LastErrorCode = resourceMessageReasonTurnStopped
			message.LastError = "This steer was cancelled because the current turn was stopped before it was consumed."
			result.Count++
			result.IDs = append(result.IDs, message.ID)
		}
		return nil
	})
	return result, err
}

func mailboxPendingForResource(workspacePath, resourceID string) (bool, error) {
	mailbox, err := loadHotResourceMailbox(workspacePath, resourceID)
	if err != nil {
		return false, err
	}
	for _, message := range mailbox.Messages {
		if message.Status == resourceMessageQueued || message.Status == resourceMessageDelivering || message.Status == resourceMessageInterrupting {
			return true, nil
		}
	}
	return false, nil
}

func acceptMailboxMessage(workspacePath, resourceID string, request resourceMessageRequest) (resourceMailboxMessage, error) {
	mode, err := normalizeResourceMessageMode(request.Mode)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	role, err := normalizeResourceMessageRole(request.Role)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	text := strings.TrimSpace(request.Text)
	if text == "" {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "invalid_request", Message: "text is required"}
	}
	resourceID = normalizedResourceID(resourceID)
	now := time.Now().Format(time.RFC3339Nano)
	subscribeResult := true
	if request.SubscribeResult != nil {
		subscribeResult = *request.SubscribeResult
	}
	if role == "system" {
		subscribeResult = false
	}
	resultSubscriptionStatus := ""
	if !subscribeResult {
		resultSubscriptionStatus = resourceResultSubscriptionDisabled
	}
	message := resourceMailboxMessage{
		ID: "msg-" + newGenerationRecordID(), ResourceID: resourceID, Text: text,
		Role: role, Sender: request.Sender, SenderWorkspaceInstanceID: strings.TrimSpace(request.SenderWorkspaceInstanceID), SubscribeResult: subscribeResult,
		ResultSubscriptionStatus: resultSubscriptionStatus, RequestedMode: mode, ActualMode: mode,
		subscribeResultPresent: true,
		Status:                 resourceMessageQueued, AcceptedAt: now, UpdatedAt: now,
	}
	_, err = mutateResourceMailboxForResource(workspacePath, resourceID, func(mailbox *resourceMailbox) error {
		mailbox.NextSequence++
		message.Sequence = mailbox.NextSequence
		mailbox.Messages = append(mailbox.Messages, cloneMailboxMessage(message))
		return nil
	})
	return message, err
}

// acceptGeneratedMailboxMessage persists a Server-generated system message
// using a deterministic id. Replays are accepted only when every immutable
// field matches, so a crash between target acceptance and source receipt
// update is both retryable and conflict-safe.
func acceptGeneratedMailboxMessage(workspacePath string, expected resourceMailboxMessage) (resourceMailboxMessage, error) {
	expected.ID = strings.TrimSpace(expected.ID)
	expected.ResourceID = normalizedResourceID(expected.ResourceID)
	expected.Text = strings.TrimSpace(expected.Text)
	requestedMode, modeErr := normalizeResourceMessageMode(expected.RequestedMode)
	if modeErr != nil {
		return resourceMailboxMessage{}, modeErr
	}
	if requestedMode == resourceMessageModeInterrupt {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "invalid_request", Message: "generated mailbox messages may request only steer or enqueue"}
	}
	actualModeValue := strings.TrimSpace(expected.ActualMode)
	if actualModeValue == "" {
		actualModeValue = requestedMode
	}
	actualMode, modeErr := normalizeResourceMessageMode(actualModeValue)
	if modeErr != nil {
		return resourceMailboxMessage{}, modeErr
	}
	if actualMode == resourceMessageModeInterrupt {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "invalid_request", Message: "generated mailbox messages may use only steer or enqueue"}
	}
	expected.Role = "system"
	expected.SubscribeResult = false
	expected.ResultSubscriptionStatus = resourceResultSubscriptionDisabled
	expected.ResultOperationID = ""
	expected.NonPromotable = true
	expected.subscribeResultPresent = true
	expected.RequestedMode = requestedMode
	if expected.ModeFrozen {
		expected.ActualMode = actualMode
	} else {
		// Durable notification acceptance starts with the caller's requested
		// mode and lets the ordinary mailbox reconciler freeze steer/enqueue
		// after inspecting the target generation and capabilities.
		expected.ActualMode = requestedMode
	}
	expected.Status = resourceMessageQueued
	if expected.ID == "" || expected.Text == "" || expected.Type == "" || expected.Causation == nil {
		return resourceMailboxMessage{}, errors.New("generated mailbox message is incomplete")
	}
	now := time.Now().Format(time.RFC3339Nano)
	if expected.AcceptedAt == "" {
		expected.AcceptedAt = now
	}
	expected.UpdatedAt = now
	var result resourceMailboxMessage
	_, err := mutateResourceMailboxForResource(workspacePath, expected.ResourceID, func(mailbox *resourceMailbox) error {
		for _, current := range mailbox.Messages {
			if current.ID != expected.ID {
				continue
			}
			if current.ResourceID != expected.ResourceID || (current.Text != "" && current.Text != expected.Text) || current.Role != expected.Role ||
				current.Type != expected.Type || current.SenderWorkspaceInstanceID != expected.SenderWorkspaceInstanceID ||
				!reflect.DeepEqual(current.Sender, expected.Sender) || !reflect.DeepEqual(current.Causation, expected.Causation) {
				return &resourceAPIError{Code: "message_conflict", Message: "stable generated message id conflicts with a different mailbox message"}
			}
			result = cloneMailboxMessage(current)
			return nil
		}
		mailbox.NextSequence++
		expected.Sequence = mailbox.NextSequence
		mailbox.Messages = append(mailbox.Messages, cloneMailboxMessage(expected))
		result = cloneMailboxMessage(expected)
		return nil
	})
	return result, err
}

func markResourceMailboxArchived(workspacePath, resourceID string) error {
	resourceID = normalizedResourceID(resourceID)
	_, err := mutateResourceMailboxForResource(workspacePath, resourceID, func(mailbox *resourceMailbox) error {
		now := time.Now().Format(time.RFC3339Nano)
		for index := range mailbox.Messages {
			message := &mailbox.Messages[index]
			if normalizedResourceID(message.ResourceID) != resourceID || message.receipt ||
				(message.Status != resourceMessageQueued && message.Status != resourceMessageDelivering && message.Status != resourceMessageInterrupting) {
				continue
			}
			if message.Status == resourceMessageDelivering {
				message.Status = resourceMessageDeliveryUnknown
				message.LastError = "target resource was archived before the delivery outcome could be confirmed"
			} else {
				message.Status = resourceMessageUndeliverable
				message.LastError = "target resource was archived before delivery began"
			}
			message.DowngradeReason = resourceMessageReasonResourceArchived
			message.LastErrorCode = "resource_archived"
			message.UpdatedAt = now
			message.TerminalAt = now
		}
		return nil
	})
	return err
}

func resourceExistsAndArchived(workspacePath, resourceID string) (bool, bool, app.AgentBinding, error) {
	resourceID = normalizedResourceID(resourceID)
	puaWorkspace, err := app.OpenWorkspace(workspacePath)
	if err != nil {
		return false, false, app.AgentBinding{}, err
	}
	if resourceID == "workspace" {
		binding, err := puaWorkspace.ResourceAgentBinding(resourceID)
		return err == nil, false, binding, err
	}
	if resourceID == app.SchedulerResourceID {
		binding, err := puaWorkspace.ResourceAgentBinding(resourceID)
		return err == nil, false, binding, err
	}
	value, err := puaWorkspace.ResourceValue(resourceID)
	if err != nil {
		return false, false, app.AgentBinding{}, err
	}
	var binding app.AgentBinding
	if value.Project != nil {
		binding = value.Project.AgentBinding
	} else if value.Task != nil {
		binding = value.Task.AgentBinding
	}
	return true, value.Archived, binding, nil
}

func waitingMailboxMessages(mailbox resourceMailbox, resourceID string) []resourceMessageResponse {
	resourceID = normalizedResourceID(resourceID)
	messages := make([]resourceMessageResponse, 0)
	for _, message := range mailbox.Messages {
		if normalizedResourceID(message.ResourceID) == resourceID && message.Status == resourceMessageQueued {
			messages = append(messages, mailboxMessageResponse(message))
		}
	}
	return messages
}

func publicSessionState(archived bool, unavailableReason string, generation *resourceGenerationStatus, session *resourceSessionStatus, runtimeError string) string {
	if archived {
		return "archived"
	}
	if strings.TrimSpace(unavailableReason) != "" || strings.TrimSpace(runtimeError) != "" {
		return "unavailable"
	}
	if session != nil {
		if session.State == "waiting_approval" {
			return "attention_required"
		}
		if session.State == "running" {
			return "working"
		}
	}
	if generation != nil {
		switch generation.Status {
		case "starting", "running", "stopping", "recovering":
			return "working"
		case "waiting_approval":
			return "attention_required"
		case "failed":
			return "unavailable"
		}
	}
	return "idle"
}

func (m *agentManager) resourceStatus(ctx context.Context, workspace serveWorkspace, resourceID string) (resourceStatusResponse, error) {
	resourceID = normalizedResourceID(resourceID)
	exists, archived, binding, err := resourceExistsAndArchived(workspace.Path, resourceID)
	if err != nil {
		return resourceStatusResponse{}, &resourceAPIError{Code: "resource_not_found", Message: err.Error()}
	}
	status := resourceStatusResponse{ResourceID: resourceID, Exists: exists, Archived: archived, AcceptsMessages: exists && !archived, Binding: binding}
	mailbox, err := loadResourceMailboxForResource(workspace.Path, resourceID)
	if err != nil {
		return resourceStatusResponse{}, err
	}
	status.Messages, status.LastError, status.LastErrorCode = mailboxCounts(mailbox, resourceID)
	status.WaitingMessages = waitingMailboxMessages(mailbox, resourceID)
	cfg, client, cfgErr := m.agentHubRuntimeConfig()
	unavailableReason := ""
	if cfgErr == nil {
		resolved, resolveErr := m.resolveResourceAgent(workspace, resourceID, cfg)
		status.ResolvedAgent = resolved.AgentName
		status.ResolvedProfile = resolved.ResolvedProfile
		status.ConfigError = resolved.ConfigError
		if resolveErr != nil && status.ConfigError == "" {
			status.ConfigError = resolveErr.Error()
		}
		if resolveErr != nil {
			unavailableReason = resolveErr.Error()
		}
	} else {
		status.ConfigError = cfgErr.Error()
		unavailableReason = cfgErr.Error()
	}
	record, found, loadErr := currentResourceGeneration(workspace.Path, resourceID)
	if loadErr != nil {
		return resourceStatusResponse{}, loadErr
	}
	if !found {
		status.SessionState = publicSessionState(archived, unavailableReason, nil, nil, "")
		return status, nil
	}
	status.Generation = &resourceGenerationStatus{
		Generation: record.Generation, GenerationID: record.GenerationID,
		Status: record.Status, ReplacementPending: record.ReplacementPending,
		CompletionState: record.CompletionState, CompletionHasFinalReply: record.CompletionHasFinalReply,
		IdleSuspended:     record.Status == "idle-suspended" || (record.IdleSleepStopRequested && record.Status == "stopped"),
		ResumeUnavailable: record.SessionResumeUnavailable,
		IdleSinceAt:       record.IdleSinceAt, IdleDeadlineAt: record.IdleDeadlineAt,
		IdleSleepRequested: record.IdleSleepStopRequested,
		TurnNumber:         record.TurnNumber,
		AgentHubSessionID:  record.AgentHubSessionID,
	}
	if strings.TrimSpace(record.AgentHubSessionID) == "" || cfgErr != nil {
		status.SessionState = publicSessionState(archived, unavailableReason, status.Generation, nil, "")
		return status, nil
	}
	session, sessionErr := client.GetSession(ctx, record.AgentHubSessionID)
	if sessionErr != nil {
		if status.LastError == "" {
			status.LastError = sessionErr.Error()
		}
		status.SessionState = publicSessionState(archived, unavailableReason, status.Generation, nil, sessionErr.Error())
		return status, nil
	}
	status.Session = &resourceSessionStatus{
		ID: session.ID, State: session.State, CurrentTurnID: session.CurrentTurnID,
		InputCapabilities: session.InputCapabilities,
	}
	status.Generation.Resumable = session.State == "stopped" && !record.SessionResumeUnavailable && !record.ReplacementPending && !record.ArchivedTaskStopRequested
	status.CanSteerWaiting = !archived && !record.ReplacementPending && (session.State == "running" || session.State == "waiting_approval") && session.InputCapabilities.Steer
	status.SessionState = publicSessionState(archived, unavailableReason, status.Generation, status.Session, "")
	return status, nil
}

func mailboxPriority(message resourceMailboxMessage) int {
	if message.Status == resourceMessageDelivering {
		return -2
	}
	if message.Status == resourceMessageInterrupting {
		return -1
	}
	if message.PromotedAt != "" {
		return 1
	}
	switch message.RequestedMode {
	case resourceMessageModeInterrupt:
		return 0
	case resourceMessageModeSteer:
		return 2
	default:
		return 3
	}
}

// findCanonicalAgentHubMessage locates the durable message.input event for
// expected by scanning semantic frames after the given cursor. A delivery
// that just appended its canonical event passes the pre-delivery
// LastEventID so large sessions do not pay a full-history rescan on every
// message; recovery paths that may look for an event written by an earlier
// attempt must pass 0.
func findCanonicalAgentHubMessage(ctx context.Context, client *agentHubClient, sessionID string, expected resourceMailboxMessage, after int64) (agentHubInboundMessage, bool, error) {
	cursor := after
	for {
		frames, latest, err := client.SessionFrames(ctx, sessionID, cursor, agentHubEventMaxCount)
		if err != nil {
			return agentHubInboundMessage{}, false, err
		}
		for _, frame := range frames {
			for _, event := range frame.Events {
				if event.Type != "message.input" {
					continue
				}
				var canonical agentHubInboundMessage
				if json.Unmarshal(event.Data, &canonical) != nil || canonical.MessageID != expected.ID {
					continue
				}
				if !canonicalAgentHubMessageMatches(canonical, expected) {
					return agentHubInboundMessage{}, false, &resourceAPIError{Code: "message_conflict", Message: "stable message id conflicts with a different canonical AgentHub input"}
				}
				canonical.TurnID = event.TurnID
				return canonical, true, nil
			}
		}
		if len(frames) == 0 || frames[len(frames)-1].Cursor <= cursor || frames[len(frames)-1].Cursor >= latest {
			return agentHubInboundMessage{}, false, nil
		}
		cursor = frames[len(frames)-1].Cursor
	}
}

// deliveredMailboxTurnID prefers the Turn ID attached to AgentHub's canonical
// message.input event. The session response is a useful fallback for daemons
// that return the current Turn projection but do not expose event metadata on
// the delivery response. The pre-delivery mailbox Turn is only a last resort
// for a steer that was already bound to an active Turn; an enqueue without an
// exact response remains unsubscribed until a later canonical observation.
func deliveredMailboxTurnID(ctx context.Context, client *agentHubClient, sessionID string, message resourceMailboxMessage, delivered, before agentHubSession) string {
	if client != nil && strings.TrimSpace(sessionID) != "" {
		// The canonical message.input event was appended by the delivery that
		// just returned, so it must sit after the pre-delivery cursor.
		if canonical, found, err := findCanonicalAgentHubMessage(ctx, client, sessionID, message, before.LastEventID); err == nil && found {
			if turnID := strings.TrimSpace(canonical.TurnID); turnID != "" {
				return turnID
			}
		}
	}
	if turnID := strings.TrimSpace(delivered.CurrentTurnID); turnID != "" {
		return turnID
	}
	if turnID := strings.TrimSpace(before.CurrentTurnID); turnID != "" && message.ActualMode == resourceMessageModeSteer {
		return turnID
	}
	return ""
}

func selectPendingMailboxMessage(mailbox resourceMailbox, resourceID string) (resourceMailboxMessage, bool) {
	resourceID = normalizedResourceID(resourceID)
	var selected resourceMailboxMessage
	found := false
	for _, message := range mailbox.Messages {
		if normalizedResourceID(message.ResourceID) != resourceID ||
			(message.Status != resourceMessageQueued && message.Status != resourceMessageDelivering && message.Status != resourceMessageInterrupting) {
			continue
		}
		if !found || mailboxPriority(message) < mailboxPriority(selected) ||
			(mailboxPriority(message) == mailboxPriority(selected) && message.Sequence < selected.Sequence) {
			selected, found = cloneMailboxMessage(message), true
		}
	}
	return selected, found
}

func mailboxAttemptDue(message resourceMailboxMessage, interval time.Duration) bool {
	last := generationTime(message.LastAttemptAt)
	return last.IsZero() || time.Since(last) >= interval
}

func generationRecordByID(workspacePath, generationID string) (generationRecord, bool, error) {
	records, err := loadGenerationRecords(workspacePath)
	if err != nil {
		return generationRecord{}, false, err
	}
	for _, record := range records {
		if record.GenerationID == strings.TrimSpace(generationID) {
			return record, true, nil
		}
	}
	return generationRecord{}, false, nil
}

// currentGenerationRecordByID is the lifecycle-facing lookup. History and
// notification code may inspect retired manifests through generationRecordByID,
// but a retired or cold generation must never be reintroduced as a mutable
// mailbox execution target.
func currentGenerationRecordByID(workspacePath, resourceID, generationID string) (generationRecord, bool, error) {
	record, found, err := currentResourceGeneration(workspacePath, resourceID)
	if err != nil || !found {
		return generationRecord{}, found, err
	}
	if record.GenerationID != strings.TrimSpace(generationID) || record.Retired {
		return generationRecord{}, false, nil
	}
	return record, true, nil
}

func (m *agentManager) ensureRuntime(workspace serveWorkspace, record generationRecord, client *agentHubClient) *agentRuntime {
	rt := m.runtimeByID(record.ID)
	if rt != nil {
		return rt
	}
	rt = newAgentHubRuntime(m, workspace, record, client)
	rt.agentHubState = agentHubStateForPUAStatus(record.Status)
	m.registerRuntime(rt)
	return rt
}

// resolveMailboxGenerationAgent is the authoritative binding/profile/catalog
// preflight for mailbox delivery that may need to create a generation. Only
// semantic binding or catalog unavailability is permanent; configuration I/O
// and AgentHub transport failures remain retryable errors.
func (m *agentManager) resolveMailboxGenerationAgent(ctx context.Context, workspace serveWorkspace, resourceID string) (config, *agentHubClient, resolvedResourceAgent, error) {
	cfg, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return config{}, nil, resolvedResourceAgent{}, err
	}
	resolved, err := m.resolveResourceAgent(workspace, resourceID, cfg)
	if err != nil {
		if isResourceAgentBindingUnavailable(err) {
			err = &resourceAPIError{Code: "binding_unavailable", Message: err.Error()}
		}
		return config{}, client, resolved, err
	}
	resolved.AgentName, err = validateAgentHubGenerationAgent(ctx, client, resolved.AgentName)
	if err != nil {
		var unavailable *agentHubGenerationAgentUnavailableError
		if errors.As(err, &unavailable) {
			err = &resourceAPIError{Code: "binding_unavailable", Message: err.Error()}
		}
		return config{}, client, resolved, err
	}
	return cfg, client, resolved, nil
}

func (m *agentManager) ensureMailboxGeneration(ctx context.Context, workspace serveWorkspace, resourceID string) (generationRecord, *agentRuntime, *agentHubClient, error) {
	if record, found, err := currentResourceGeneration(workspace.Path, resourceID); err != nil {
		return generationRecord{}, nil, nil, err
	} else if found {
		_, client, cfgErr := m.agentHubRuntimeConfig()
		if cfgErr != nil {
			return record, nil, nil, cfgErr
		}
		return record, m.ensureRuntime(workspace, record, client), client, nil
	}
	cfg, client, resolved, err := m.resolveMailboxGenerationAgent(ctx, workspace, resourceID)
	if err != nil {
		return generationRecord{}, nil, client, err
	}
	cwd, err := m.generationCwd(ctx, workspace, resourceID, "")
	if err != nil {
		return generationRecord{}, nil, client, err
	}
	created, err := m.createResourceGeneration(ctx, workspace, resourceID, cwd, cfg, client, resolved)
	if err != nil {
		return created, m.runtimeByID(created.ID), client, err
	}
	return created, m.runtimeByID(created.ID), client, nil
}

func resourceDeliveryErrorCode(err error) string {
	var apiErr *resourceAPIError
	if errors.As(err, &apiErr) && strings.TrimSpace(apiErr.Code) != "" {
		return apiErr.Code
	}
	return "temporarily_undeliverable"
}

func recordMailboxFailure(workspacePath, messageID string, err error) {
	if err == nil {
		return
	}
	code := resourceDeliveryErrorCode(err)
	// Ownership loss is a handoff boundary, not a delivery failure. A stale
	// controller must not annotate the mailbox after another Server can own it.
	if code == "workspace_not_owned" {
		return
	}
	if current, found, loadErr := mailboxMessageByID(workspacePath, messageID); loadErr == nil && found &&
		current.LastError == err.Error() && current.LastErrorCode == code {
		return
	}
	_, _ = updateMailboxMessage(workspacePath, messageID, func(message *resourceMailboxMessage) {
		message.LastError = err.Error()
		message.LastErrorCode = code
	})
}

func (m *agentManager) acceptResourceMessage(ctx context.Context, workspace serveWorkspace, resourceID string, request resourceMessageRequest) (resourceMailboxMessage, error) {
	resourceID = normalizedResourceID(resourceID)
	var message resourceMailboxMessage
	err := m.withResourceController(ctx, workspace, resourceID, func() error {
		var err error
		message, err = m.acceptResourceMessageDurable(ctx, workspace, resourceID, request)
		if err != nil {
			return err
		}
		if reconcileErr := m.reconcileResourceMailboxLocked(ctx, workspace, resourceID); reconcileErr != nil {
			recordMailboxFailure(workspace.Path, message.ID, reconcileErr)
		}
		if updated, found, loadErr := mailboxMessageByID(workspace.Path, message.ID); loadErr == nil && found {
			message = updated
		}
		return nil
	})
	if err == nil {
		m.requestReconcile(reconcileNotifications)
	}
	return message, err
}

// acceptResourceMessageDurable validates the resource and persists the
// mailbox acceptance. It deliberately does not contact AgentHub; callers must
// wake the resource controller after this short durable boundary.
func (m *agentManager) acceptResourceMessageDurable(ctx context.Context, workspace serveWorkspace, resourceID string, request resourceMessageRequest) (resourceMailboxMessage, error) {
	resourceID = normalizedResourceID(resourceID)
	if m.server != nil {
		if _, err := m.server.requireWorkspaceInstanceOwnership(workspace); err != nil {
			return resourceMailboxMessage{}, err
		}
	}
	exists, archived, _, err := resourceExistsAndArchived(workspace.Path, resourceID)
	if err != nil || !exists {
		message := fmt.Sprintf("resource not found: %s", resourceID)
		if err != nil {
			message = err.Error()
		}
		return resourceMailboxMessage{}, &resourceAPIError{Code: "resource_not_found", Message: message}
	}
	if archived {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "resource_archived", Message: fmt.Sprintf("resource %s is archived and no longer accepts messages", resourceID)}
	}
	message, err := acceptMailboxMessage(workspace.Path, resourceID, request)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	_ = ctx
	return message, nil
}

func (m *agentManager) promoteWaitingMessage(ctx context.Context, workspace serveWorkspace, messageID string) (resourceMailboxMessage, error) {
	resourceID, err := mailboxMessageResourceID(workspace.Path, messageID)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	var message resourceMailboxMessage
	err = m.withResourceController(ctx, workspace, resourceID, func() error {
		var err error
		message, err = m.promoteWaitingMessageLocked(ctx, workspace, messageID)
		if err != nil {
			return err
		}
		if reconcileErr := m.reconcileResourceMailboxLocked(ctx, workspace, message.ResourceID); reconcileErr != nil {
			recordMailboxFailure(workspace.Path, message.ID, reconcileErr)
		}
		if updated, found, loadErr := mailboxMessageByID(workspace.Path, message.ID); loadErr == nil && found {
			message = updated
		}
		return nil
	})
	if err == nil {
		m.requestReconcile(reconcileNotifications)
	}
	return message, err
}

// mailboxMessageResourceID reads the stable resource address before a
// controller operation begins. The mailbox lookup is durable and does not
// contact AgentHub.
func mailboxMessageResourceID(workspacePath, messageID string) (string, error) {
	message, found, err := mailboxMessageByID(workspacePath, messageID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", &resourceAPIError{Code: "message_not_found", Message: fmt.Sprintf("mailbox message not found: %s", messageID)}
	}
	return normalizedResourceID(message.ResourceID), nil
}

func (m *agentManager) promoteWaitingMessageLocked(ctx context.Context, workspace serveWorkspace, messageID string) (resourceMailboxMessage, error) {
	if m.server != nil {
		if _, err := m.server.requireWorkspaceInstanceOwnership(workspace); err != nil {
			return resourceMailboxMessage{}, err
		}
	}
	message, found, err := mailboxMessageByID(workspace.Path, messageID)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	if !found {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "message_not_found", Message: fmt.Sprintf("mailbox message not found: %s", messageID)}
	}
	if message.Status != resourceMessageQueued {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "message_not_waiting", Message: fmt.Sprintf("message %s is not waiting", messageID)}
	}
	if message.NonPromotable {
		return resourceMailboxMessage{}, &resourceAPIError{
			Code: "message_not_promotable", Message: fmt.Sprintf("message %s cannot be promoted", messageID),
		}
	}
	_, archived, _, resourceErr := resourceExistsAndArchived(workspace.Path, message.ResourceID)
	if resourceErr != nil {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "resource_not_found", Message: resourceErr.Error()}
	}
	if archived {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "resource_archived", Message: fmt.Sprintf("resource %s is archived", message.ResourceID)}
	}
	record, recordFound, err := currentResourceGeneration(workspace.Path, message.ResourceID)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	if !recordFound || record.ReplacementPending || strings.TrimSpace(record.AgentHubSessionID) == "" {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "steer_unavailable", Message: "the target task does not have an active steer-capable turn"}
	}
	_, client, err := m.agentHubRuntimeConfig()
	if err != nil {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "steer_unavailable", Message: err.Error()}
	}
	session, err := client.GetSession(ctx, record.AgentHubSessionID)
	if err != nil {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "steer_unavailable", Message: err.Error()}
	}
	active := session.State == "running" || session.State == "waiting_approval"
	if !active || !session.InputCapabilities.Steer {
		return resourceMailboxMessage{}, &resourceAPIError{Code: "steer_unavailable", Message: "the target task does not have an active steer-capable turn"}
	}
	now := time.Now().Format(time.RFC3339Nano)
	message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
		current.ActualMode = resourceMessageModeSteer
		current.ModeFrozen = true
		current.DowngradeReason = ""
		current.PromotedAt = now
		current.LastError = ""
		current.LastErrorCode = ""
	})
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	return message, nil
}

func (m *agentManager) reconcileResourceMailboxLocked(ctx context.Context, workspace serveWorkspace, resourceID string) error {
	// Production reconciliation runs inside both its resource controller and
	// the Workspace handoff barrier. Revalidate before even the Scheduler
	// legacy-tick cleanup as defense in depth for isolated direct callers, which
	// intentionally bypass production controller wiring.
	if m.server != nil {
		if _, err := m.server.requireWorkspaceInstanceOwnership(workspace); err != nil {
			return err
		}
	}
	resourceID = normalizedResourceID(resourceID)
	if resourceID == app.SchedulerResourceID {
		if err := newNativeScheduler(m, workspace).cancelLegacyTicks(ctx); err != nil {
			return err
		}
	}
	_, archived, _, resourceErr := resourceExistsAndArchived(workspace.Path, resourceID)
	if resourceErr != nil {
		return resourceErr
	}
	if archived {
		mailbox, mailboxErr := loadHotResourceMailbox(workspace.Path, resourceID)
		if mailboxErr != nil {
			return mailboxErr
		}
		pending, next := mailboxFacts(mailbox, resourceID)
		plan := PlanGeneration(GenerationLifecycleFacts{
			ResourceID: resourceID, ResourceArchived: true, MailboxPending: pending, NextMessage: next,
		})
		if plan.Operation == GenerationOperationFinalizeArchivedMailbox {
			return markResourceMailboxArchived(workspace.Path, resourceID)
		}
		return nil
	}
	for iteration := 0; iteration < 32; iteration++ {
		mailbox, err := loadHotResourceMailbox(workspace.Path, resourceID)
		if err != nil {
			return err
		}
		message, found := selectPendingMailboxMessage(mailbox, resourceID)
		if !found {
			return nil
		}
		var record generationRecord
		var rt *agentRuntime
		var client *agentHubClient
		if message.GenerationID != "" && (message.Status == resourceMessageDelivering || message.Status == resourceMessageInterrupting) {
			associated, associatedFound, associatedErr := currentGenerationRecordByID(workspace.Path, resourceID, message.GenerationID)
			if associatedErr != nil {
				return associatedErr
			}
			if associatedFound {
				record = associated
				_, client, err = m.agentHubRuntimeConfig()
				if err != nil {
					return err
				}
				rt = m.ensureRuntime(workspace, record, client)
			}
		}
		if rt == nil {
			if message.GenerationID != "" && (message.Status == resourceMessageDelivering || message.Status == resourceMessageInterrupting) {
				now := time.Now().Format(time.RFC3339Nano)
				if _, err := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.Status = resourceMessageDeliveryUnknown
					current.UpdatedAt = now
					current.TerminalAt = now
					current.LastErrorCode = "generation_retired"
					current.LastError = "the generation was retired before the delivery outcome could be confirmed"
				}); err != nil {
					return err
				}
				continue
			}
			record, rt, client, err = m.ensureMailboxGeneration(ctx, workspace, resourceID)
			if err != nil {
				recordMailboxFailure(workspace.Path, message.ID, err)
				exhausted, stateErr := m.recordTaskStartFailure(workspace, message, err)
				if stateErr != nil {
					return stateErr
				}
				if exhausted {
					return nil
				}
				return err
			}
		}
		// Replacement/archive owns the old generation until its terminal
		// sequence completes. Idle sleep is different: the stopped current
		// Session is the exact target of an on-demand Resume.
		if resourceGenerationLifecyclePending(record) {
			return nil
		}
		if record.ReplacementPending && message.Status == resourceMessageQueued {
			if message.RequestedMode == resourceMessageModeSteer {
				_, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.ActualMode = resourceMessageModeEnqueue
					current.ModeFrozen = true
					current.DowngradeReason = resourceMessageReasonGenerationReplacing
				})
				if err != nil {
					return err
				}
			}
			// A fresh interrupt must still stop the old active Turn. Once that
			// has happened, or for every other mode, mailbox ownership waits for
			// the replacement generation and never delivers into the old one.
			if message.RequestedMode != resourceMessageModeInterrupt || message.InterruptAt != "" {
				return nil
			}
		}
		session, err := client.GetSession(ctx, record.AgentHubSessionID)
		if err != nil {
			if isTerminalResumeError(err) {
				if retireErr := m.retireUnresumableGenerationLocked(ctx, rt, client, err); retireErr != nil {
					return retireErr
				}
				continue
			}
			recordMailboxFailure(workspace.Path, message.ID, err)
			return err
		}
		cfg, _, cfgErr := m.agentHubRuntimeConfig()
		if cfgErr != nil {
			return cfgErr
		}
		if !agentHubSessionExactlyMatchesGeneration(cfg, record, session) {
			if retireErr := m.retireUnresumableGenerationLocked(ctx, rt, client, fmt.Errorf("AgentHub Session %s source does not match generation %s", session.ID, record.GenerationID)); retireErr != nil {
				return retireErr
			}
			continue
		}
		rt.applyAgentHubSessionState(m, session)
		record = rt.snapshotGeneration()
		active := session.State == "running" || session.State == "waiting_approval"
		// A queued non-steer input at an inactive boundary is about to create a
		// new Turn (including a steer that must be downgraded, and an interrupt
		// after its old Turn has stopped). Evaluate the generation budget and
		// resolve Profile routing here rather than from the periodic poller or
		// settings write path. A true active-Turn steer deliberately keeps using
		// the current generation.
		startsNewTurn := message.Status == resourceMessageQueued && !active &&
			(!message.ModeFrozen || message.ActualMode != resourceMessageModeSteer)
		if startsNewTurn {
			replaced, prepareErr := m.prepareResourceGenerationForNewTurnLocked(ctx, workspace, record, session, rt, client)
			if prepareErr != nil {
				recordMailboxFailure(workspace.Path, message.ID, prepareErr)
				return prepareErr
			}
			if replaced {
				return nil
			}
			record = rt.snapshotGeneration()
		}
		lifecyclePlan := PlanGeneration(AdaptLegacyGenerationFacts(LegacyGenerationLifecycleInput{
			Generation: record, Session: &session, Mailbox: mailbox, Now: m.resourceNow(), Revision: record.UpdatedAt,
		}))
		switch lifecyclePlan.Operation {
		case GenerationOperationResumeSession:
			resumed, terminal, resumeErr := m.resumeStoppedGenerationLocked(ctx, workspace, record, rt, client, lifecyclePlan)
			if terminal {
				if retireErr := m.retireUnresumableGenerationLocked(ctx, rt, client, resumeErr); retireErr != nil {
					return retireErr
				}
				continue
			}
			if resumeErr != nil {
				recordMailboxFailure(workspace.Path, message.ID, resumeErr)
				return resumeErr
			}
			if resumed {
				continue
			}
			return nil
		case GenerationOperationStopSession, GenerationOperationWaitForStopped, GenerationOperationArchiveSession,
			GenerationOperationWaitForSession:
			return nil
		case GenerationOperationRetireGeneration:
			if session.State == "archived" {
				// A message can observe an externally archived Session before the
				// poller does. Treat the exact archived Session as the terminal
				// Resume boundary; never revive it or create a replacement from a
				// different source tuple.
				if retireErr := m.retireUnresumableGenerationLocked(ctx, rt, client, fmt.Errorf("AgentHub Session %s is archived and cannot be resumed", session.ID)); retireErr != nil {
					return retireErr
				}
				if _, currentFound, currentErr := currentResourceGeneration(workspace.Path, resourceID); currentErr != nil {
					return currentErr
				} else if !currentFound {
					// The archived proof retired the old current generation. Let
					// the same mailbox pass create and deliver the next generation.
					continue
				}
			}
			return nil
		}

		if message.Status == resourceMessageInterrupting {
			if active && session.CurrentTurnID == message.InterruptTurnID {
				if !mailboxAttemptDue(message, 5*time.Second) {
					return nil
				}
				attempted, persistErr := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.AttemptCount++
					current.LastAttemptAt = time.Now().Format(time.RFC3339Nano)
					current.LastError = ""
					current.LastErrorCode = ""
				})
				if persistErr != nil {
					return persistErr
				}
				_ = attempted
				interrupted, interruptErr := client.Interrupt(ctx, session.ID)
				if interruptErr != nil {
					recordMailboxFailure(workspace.Path, message.ID, interruptErr)
					return interruptErr
				}
				stillCurrent, guardErr := legacyLifecyclePlanStillCurrent(workspace, lifecyclePlan, &interrupted)
				if guardErr != nil {
					return guardErr
				}
				if !stillCurrent {
					return nil
				}
				rt.applyAgentHubSessionState(m, interrupted)
				session = interrupted
				active = session.State == "running" || session.State == "waiting_approval"
			}
			if active {
				return nil
			}
			message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
				current.Status = resourceMessageQueued
				current.InterruptAt = time.Now().Format(time.RFC3339Nano)
				current.LastError = ""
				current.LastErrorCode = ""
			})
			if err != nil {
				return err
			}
			if record.ReplacementPending {
				return nil
			}
			// Re-enter from the now-inactive queued boundary so Profile routing is
			// checked before the interrupt request starts its replacement Turn.
			continue
		}

		if message.Status == resourceMessageQueued && !message.ModeFrozen {
			switch message.RequestedMode {
			case resourceMessageModeInterrupt:
				if message.InterruptAt != "" {
					break
				}
				if record.ReplacementPending && !active {
					_, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
						current.ActualMode = resourceMessageModeEnqueue
						current.ModeFrozen = true
						current.DowngradeReason = resourceMessageReasonGenerationReplacing
					})
					if err != nil {
						return err
					}
					return nil
				}
				if active {
					message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
						current.ActualMode = resourceMessageModeInterrupt
						current.ModeFrozen = true
						current.Status = resourceMessageInterrupting
						current.GenerationID = record.GenerationID
						current.AgentHubSessionID = session.ID
						current.InterruptTurnID = session.CurrentTurnID
						current.AttemptCount++
						current.LastAttemptAt = time.Now().Format(time.RFC3339Nano)
						current.LastError = ""
						current.LastErrorCode = ""
					})
					if err != nil {
						return err
					}
					interrupted, interruptErr := client.Interrupt(ctx, session.ID)
					if interruptErr != nil {
						recordMailboxFailure(workspace.Path, message.ID, interruptErr)
						return interruptErr
					}
					stillCurrent, guardErr := legacyLifecyclePlanStillCurrent(workspace, lifecyclePlan, &interrupted)
					if guardErr != nil {
						return guardErr
					}
					if !stillCurrent {
						return nil
					}
					rt.applyAgentHubSessionState(m, interrupted)
					continue
				}
				message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.ActualMode = resourceMessageModeEnqueue
					current.ModeFrozen = true
					current.DowngradeReason = resourceMessageReasonNoActiveTurn
				})
			case resourceMessageModeSteer:
				if active && session.InputCapabilities.Steer {
					message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
						current.ActualMode = resourceMessageModeSteer
						current.ModeFrozen = true
						current.DowngradeReason = ""
					})
				} else {
					reason := resourceMessageReasonNoActiveTurn
					if active {
						reason = resourceMessageReasonSteerUnsupported
					}
					message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
						current.ActualMode = resourceMessageModeEnqueue
						current.ModeFrozen = true
						current.DowngradeReason = reason
					})
				}
			case resourceMessageModeEnqueue:
				// An ordinary enqueue stays promotable while it waits behind an
				// active Turn. Freeze it only at the inactive boundary where this
				// pass can deliver it as the next Turn opener.
				if active {
					break
				}
				message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.ActualMode = resourceMessageModeEnqueue
					current.ModeFrozen = true
					current.DowngradeReason = ""
				})
			}
			if err != nil {
				return err
			}
		}

		if message.Status != resourceMessageDelivering && active && message.ActualMode != resourceMessageModeSteer {
			return nil
		}
		if message.Status != resourceMessageDelivering && session.State != "ready" && message.ActualMode != resourceMessageModeSteer {
			return nil
		}
		message, err = m.ensureProviderMessageContext(ctx, workspace, client, session, record.GenerationID, message)
		if err != nil {
			return err
		}
		deliveryPlan := lifecyclePlan
		if err := m.prepareTaskWorkChain(workspace, message, rt); err != nil {
			return err
		}
		message, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
			current.Status = resourceMessageDelivering
			current.GenerationID = record.GenerationID
			current.AgentHubSessionID = session.ID
			current.TurnID = session.CurrentTurnID
			current.AttemptCount++
			current.LastAttemptAt = time.Now().Format(time.RFC3339Nano)
			current.LastError = ""
			current.LastErrorCode = ""
		})
		if err != nil {
			return err
		}
		outbound, outboundErr := agentHubMailboxMessage(message)
		if outboundErr != nil {
			return outboundErr
		}
		delivered, deliveryErr := client.Message(ctx, session.ID, outbound)
		if deliveryErr != nil {
			stillCurrent, guardErr := legacyLifecyclePlanStillCurrent(workspace, deliveryPlan, &session)
			if guardErr != nil {
				return guardErr
			}
			if !stillCurrent {
				return nil
			}
			var deliveryAPIError *agentHubAPIError
			conflict := errors.As(deliveryErr, &deliveryAPIError) && deliveryAPIError.StatusCode == http.StatusConflict &&
				strings.Contains(deliveryAPIError.Message, "message id conflicts with an existing input")
			var canonical agentHubInboundMessage
			var canonicalFound bool
			var canonicalErr error
			if conflict {
				// The conflicting canonical event may come from an earlier
				// attempt that predates the session snapshot taken by this pass,
				// so the recovery scan cannot use the pre-delivery cursor hint.
				canonical, canonicalFound, canonicalErr = findCanonicalAgentHubMessage(ctx, client, session.ID, message, 0)
			}
			if canonicalErr == nil && canonicalFound {
				stillCurrent, guardErr := legacyLifecyclePlanStillCurrent(workspace, deliveryPlan, &session)
				if guardErr != nil {
					return guardErr
				}
				if !stillCurrent {
					return nil
				}
				_, persistErr := updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
					current.Status = resourceMessageDelivered
					current.DeliveredAt = time.Now().Format(time.RFC3339Nano)
					current.TerminalAt = current.DeliveredAt
					current.LastError = ""
					current.LastErrorCode = ""
					if canonical.Steer {
						current.ActualMode = resourceMessageModeSteer
					} else {
						current.ActualMode = resourceMessageModeEnqueue
						if current.RequestedMode == resourceMessageModeSteer && current.DowngradeReason == "" {
							current.DowngradeReason = resourceMessageReasonRecoveredCanonical
						}
					}
					turnID := strings.TrimSpace(canonical.TurnID)
					if turnID == "" {
						turnID = strings.TrimSpace(current.TurnID)
					}
					if turnID == "" && canonical.Steer {
						turnID = strings.TrimSpace(session.CurrentTurnID)
					}
					current.TurnID = turnID
					bindMailboxResultSubscription(current, turnID)
				})
				if persistErr != nil {
					return persistErr
				}
				continue
			}
			if canonicalErr != nil {
				deliveryErr = fmt.Errorf("%w; inspect canonical input: %v", deliveryErr, canonicalErr)
			}
			recordMailboxFailure(workspace.Path, message.ID, deliveryErr)
			return deliveryErr
		}
		stillCurrent, guardErr := legacyLifecyclePlanStillCurrent(workspace, deliveryPlan, &delivered)
		if guardErr != nil {
			return guardErr
		}
		if !stillCurrent {
			return nil
		}
		turnID := deliveredMailboxTurnID(ctx, client, session.ID, message, delivered, session)
		_, err = updateMailboxMessage(workspace.Path, message.ID, func(current *resourceMailboxMessage) {
			current.Status = resourceMessageDelivered
			current.DeliveredAt = time.Now().Format(time.RFC3339Nano)
			current.TerminalAt = current.DeliveredAt
			current.TurnID = turnID
			bindMailboxResultSubscription(current, turnID)
			current.LastError = ""
			current.LastErrorCode = ""
		})
		if err != nil {
			return err
		}
		rt.applyAgentHubSessionState(m, delivered)
		if delivered.State == "running" || delivered.State == "waiting_approval" {
			// More eligible steer inputs may enter this Turn; enqueue inputs wait.
			continue
		}
	}
	return errors.New("resource mailbox reconciliation exceeded its bounded iteration limit")
}

func (m *agentManager) reconcileWorkspaceMailboxes(ctx context.Context, workspace serveWorkspace) error {
	resourceIDs, err := listHotResourceMailboxResourceIDs(workspace.Path)
	if err != nil {
		return err
	}
	type result struct {
		resourceID string
		err        error
	}
	results := make(chan result, len(resourceIDs))
	for _, id := range resourceIDs {
		resourceID := id
		go func() {
			err := m.withResourceController(ctx, workspace, resourceID, func() error {
				return m.reconcileResourceMailboxLocked(ctx, workspace, resourceID)
			})
			results <- result{resourceID: resourceID, err: err}
		}()
	}
	var failures []string
	for range resourceIDs {
		outcome := <-results
		if outcome.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", outcome.resourceID, outcome.err))
		}
	}
	sort.Strings(failures)
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func resourceErrorStatus(err error) int {
	var apiErr *resourceAPIError
	if !errors.As(err, &apiErr) {
		return http.StatusInternalServerError
	}
	switch apiErr.Code {
	case "invalid_request", "invalid_history_cursor", "invalid_history_reference":
		return http.StatusBadRequest
	case "resource_not_found", "message_not_found", "history_reference_not_found", "history_turn_not_found", "history_event_not_found", "session_missing":
		return http.StatusNotFound
	case "message_receipt_expired":
		return http.StatusGone
	case "resource_archived", "message_not_waiting", "message_not_promotable", "steer_unavailable", "generation_unavailable", "generation_changed", "active_turn":
		return http.StatusConflict
	case "workspace_not_owned":
		return http.StatusConflict
	case "binding_unavailable":
		return http.StatusUnprocessableEntity
	case "temporarily_undeliverable", "history_unavailable":
		return http.StatusServiceUnavailable
	case "message_conflict":
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (m *agentManager) handleResourceStatus(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	resourceID = normalizedResourceID(resourceID)
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var status resourceStatusResponse
	statusErr := m.withResourceController(r.Context(), workspace, resourceID, func() error {
		var err error
		status, err = m.resourceStatus(r.Context(), workspace, resourceID)
		return err
	})
	if statusErr != nil {
		writeError(w, statusErr, resourceErrorStatus(statusErr))
		return
	}
	writeJSON(w, status)
}

type taskStateResponse struct {
	ResourceID     string        `json:"resourceId"`
	State          app.TaskState `json:"state,omitempty"`
	Note           string        `json:"note,omitempty"`
	StateUpdatedAt string        `json:"stateUpdatedAt,omitempty"`
}

func taskStateFromDetail(detail app.ResourceDetailView) (taskStateResponse, error) {
	if detail.Type != "task" {
		return taskStateResponse{}, &resourceAPIError{Code: "invalid_request", Message: "task state is supported only for Task resources"}
	}
	return taskStateResponse{ResourceID: detail.ID, State: detail.State, Note: detail.StateNote, StateUpdatedAt: detail.StateUpdatedAt}, nil
}

func (m *agentManager) handleTaskState(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	resourceID = normalizedResourceID(resourceID)
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var response taskStateResponse
	err = m.withResourceController(r.Context(), workspace, resourceID, func() error {
		puaWorkspace, openErr := app.OpenWorkspace(workspace.Path)
		if openErr != nil {
			return openErr
		}
		if r.Method == http.MethodGet {
			detail, detailErr := puaWorkspace.Resource(resourceID)
			if detailErr != nil {
				return &resourceAPIError{Code: "resource_not_found", Message: detailErr.Error()}
			}
			response, detailErr = taskStateFromDetail(detail)
			return detailErr
		}
		var body struct {
			State app.TaskState `json:"state"`
			Note  string        `json:"note"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&body); decodeErr != nil {
			return &resourceAPIError{Code: "invalid_request", Message: decodeErr.Error()}
		}
		if !app.IsAgentTaskState(body.State) {
			return &resourceAPIError{Code: "invalid_request", Message: "Agent may set task state only to waiting, blocked, paused, or completed"}
		}
		task, setErr := puaWorkspace.SetTaskState(resourceID, body.State, body.Note)
		if setErr != nil {
			return &resourceAPIError{Code: "invalid_request", Message: setErr.Error()}
		}
		response = taskStateResponse{ResourceID: task.ID, State: task.State, Note: task.StateNote, StateUpdatedAt: task.StateUpdatedAt}
		return nil
	})
	if err != nil {
		writeError(w, err, resourceErrorStatus(err))
		return
	}
	writeJSON(w, response)
}

func (m *agentManager) handleResourceMessages(w http.ResponseWriter, r *http.Request, workspaceID, resourceID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	var request resourceMessageRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, &resourceAPIError{Code: "invalid_request", Message: err.Error()}, http.StatusBadRequest)
		return
	}
	resourceID = normalizedResourceID(resourceID)
	var message resourceMailboxMessage
	sendErr := m.withResourceController(r.Context(), workspace, resourceID, func() error {
		var err error
		message, err = m.acceptResourceMessageDurable(r.Context(), workspace, resourceID, request)
		return err
	})
	if sendErr != nil {
		writeError(w, sendErr, resourceErrorStatus(sendErr))
		return
	}
	// A user sending a message has seen every completed Turn so far, so the
	// send implicitly marks the resource read; the Turn it triggers becomes
	// the next unread one when it completes. Agent-to-agent messages do not
	// touch the user's read cursor.
	if message.Role == "user" {
		if userName, userErr := m.server.workspaceUserName(r, workspace.Path); userErr == nil {
			_ = m.server.withWorkspaceMutation(context.WithoutCancel(r.Context()), workspace, resourceID, func(current serveWorkspace) error {
				m.server.markResourceReadOnUserMessage(current.Path, resourceID, userName)
				return nil
			})
		}
	}
	if wakeErr := m.enqueueResourceController(workspace, resourceID, func() error {
		if err := m.reconcileResourceMailboxLocked(context.Background(), workspace, resourceID); err != nil {
			recordMailboxFailure(workspace.Path, message.ID, err)
		}
		m.requestReconcile(reconcileNotifications)
		return nil
	}); wakeErr != nil {
		recordMailboxFailure(workspace.Path, message.ID, wakeErr)
		m.requestReconcile(reconcileNotifications)
	}
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspaceID, message.ID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(response)
}

func (m *agentManager) handleResourceMessage(w http.ResponseWriter, r *http.Request, workspaceID, messageID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	message, found, err := mailboxMessageByID(workspace.Path, messageID)
	if err != nil {
		writeError(w, err, resourceErrorStatus(err))
		return
	}
	if !found {
		notFound := &resourceAPIError{Code: "message_not_found", Message: fmt.Sprintf("mailbox message not found: %s", messageID)}
		writeError(w, notFound, http.StatusNotFound)
		return
	}
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspaceID, message.ID)
	writeJSON(w, response)
}

func (m *agentManager) handleResourceMessageSteer(w http.ResponseWriter, r *http.Request, workspaceID, messageID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	workspace, err := m.server.workspace(workspaceID)
	if err != nil {
		writeError(w, &resourceAPIError{Code: "workspace_not_owned", Message: err.Error()}, http.StatusNotFound)
		return
	}
	message, promoteErr := m.promoteWaitingMessage(r.Context(), workspace, messageID)
	if promoteErr != nil {
		writeError(w, promoteErr, resourceErrorStatus(promoteErr))
		return
	}
	response := mailboxMessageResponse(message)
	response.Reference = fmt.Sprintf("/api/workspaces/%s/messages/%s", workspaceID, message.ID)
	if message.Status != resourceMessageDelivered {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	writeJSON(w, response)
}
