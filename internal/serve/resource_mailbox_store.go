package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/disksing/pua/internal/app"
)

const (
	resourceMailboxStoreVersion    = 1
	resourceMailboxExpiredState    = "expired"
	resourceMailboxHotIndexVersion = 1
)

// These are variables so focused tests can shrink the retention window without
// changing the production contract.
var (
	resourceMailboxReceiptRetentionCount  = 2048
	resourceMailboxReceiptRetentionWindow = 7 * 24 * time.Hour
	resourceMailboxExpiredRetentionCount  = 2048
	resourceMailboxExpiredRetentionWindow = 30 * 24 * time.Hour
)

type resourceMailboxStore struct {
	ResourceID  string
	InstanceID  string
	ResourceKey string
	Directory   string
	Mailbox     resourceMailbox
	Receipts    resourceMailboxReceiptDocument
	Outbox      resourceMailboxOutboxDocument
	Scheduler   resourceSchedulerCheckpoint
}

type resourceMailboxHotDocument struct {
	Version      int                      `json:"version"`
	ResourceID   string                   `json:"resourceId"`
	NextSequence uint64                   `json:"nextSequence"`
	Messages     []resourceMailboxMessage `json:"messages"`
}

type resourceMailboxReceiptDocument struct {
	Version    int                           `json:"version"`
	ResourceID string                        `json:"resourceId"`
	Receipts   []resourceMailboxReceipt      `json:"receipts"`
	Expired    []resourceMailboxExpiredEntry `json:"expired,omitempty"`
}

type resourceMailboxReceipt struct {
	ID                        string                       `json:"id"`
	Sequence                  uint64                       `json:"sequence"`
	ResourceID                string                       `json:"resourceId"`
	Role                      string                       `json:"role,omitempty"`
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
	TurnTerminalAt            string                       `json:"turnTerminalAt,omitempty"`
	GenerationID              string                       `json:"generationId,omitempty"`
	AgentHubSessionID         string                       `json:"agentHubSessionId,omitempty"`
	TurnID                    string                       `json:"turnId,omitempty"`
	ProviderContext           *providerMessageContext      `json:"providerContext,omitempty"`
	InterruptTurnID           string                       `json:"interruptTurnId,omitempty"`
	InterruptAt               string                       `json:"interruptAt,omitempty"`
	PromotedAt                string                       `json:"promotedAt,omitempty"`
	LastError                 string                       `json:"lastError,omitempty"`
	LastErrorCode             string                       `json:"lastErrorCode,omitempty"`
	subscribeResultPresent    bool
}

// UnmarshalJSON applies the pre-subscribeResult default to old receipt files
// while retaining an explicitly persisted false value.
func (receipt *resourceMailboxReceipt) UnmarshalJSON(data []byte) error {
	type mailboxReceiptAlias resourceMailboxReceipt
	var decoded mailboxReceiptAlias
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
	message := mailboxMessageFromReceipt(resourceMailboxReceipt(decoded))
	decoded.Type = message.Type
	decoded.Causation = cloneMailboxCausation(message.Causation)
	decoded.Notification = cloneNotificationReceipt(message.Notification)
	*receipt = resourceMailboxReceipt(decoded)
	return nil
}

type resourceMailboxExpiredEntry struct {
	ID        string `json:"id"`
	ExpiredAt string `json:"expiredAt"`
}

type resourceMailboxOutboxDocument struct {
	Version    int                             `json:"version"`
	ResourceID string                          `json:"resourceId"`
	Operations []resourceMailboxNotificationOp `json:"operations"`
}

// resourceMailboxNotificationOp intentionally keeps the generated body only
// while a notification is pending. Once the target accepts it, the source
// receipt retains only the diagnostic fields needed to inspect the outcome.
type resourceMailboxNotificationOp struct {
	ID                        string                    `json:"id"`
	Type                      string                    `json:"type"`
	SourceMessageID           string                    `json:"sourceMessageId"`
	SourceMessageIDs          []string                  `json:"sourceMessageIds,omitempty"`
	SourceResourceID          string                    `json:"sourceResourceId"`
	SourceWorkspaceInstanceID string                    `json:"sourceWorkspaceInstanceId"`
	TargetWorkspaceInstanceID string                    `json:"targetWorkspaceInstanceId"`
	TargetResourceID          string                    `json:"targetResourceId"`
	GeneratedMessageID        string                    `json:"generatedMessageId"`
	GeneratedText             string                    `json:"generatedText,omitempty"`
	GeneratedSender           *agentHubMessageSender    `json:"generatedSender,omitempty"`
	GeneratedCausation        *resourceMessageCausation `json:"generatedCausation,omitempty"`
	GeneratedRequestedMode    string                    `json:"generatedRequestedMode,omitempty"`
	GeneratedActualMode       string                    `json:"generatedActualMode,omitempty"`
	GeneratedModeFrozen       bool                      `json:"generatedModeFrozen,omitempty"`
	GeneratedDowngradeReason  string                    `json:"generatedDowngradeReason,omitempty"`
	Status                    string                    `json:"status"`
	AcceptedAt                string                    `json:"acceptedAt,omitempty"`
	UpdatedAt                 string                    `json:"updatedAt"`
	DeliveredAt               string                    `json:"deliveredAt,omitempty"`
	TerminalAt                string                    `json:"terminalAt,omitempty"`
	DeliveryStatus            string                    `json:"deliveryStatus,omitempty"`
	LastError                 string                    `json:"lastError,omitempty"`
	LastErrorCode             string                    `json:"lastErrorCode,omitempty"`
	GenerationID              string                    `json:"generationId,omitempty"`
	TurnID                    string                    `json:"turnId,omitempty"`
	TurnReference             string                    `json:"turnReference,omitempty"`
	TurnStatus                string                    `json:"turnStatus,omitempty"`
	HistoryUnavailable        bool                      `json:"historyUnavailable,omitempty"`
}

type resourceSchedulerCheckpoint struct {
	Version         int                                 `json:"version"`
	ResourceID      string                              `json:"resourceId"`
	MigrationDigest string                              `json:"migrationDigest,omitempty"`
	Schedules       map[string]schedulerScheduleRuntime `json:"schedules,omitempty"`
}

type schedulerScheduleRuntime struct {
	Revision           uint64                       `json:"revision"`
	ActivationRevision uint64                       `json:"activationRevision,omitempty"`
	TriggerDigest      string                       `json:"triggerDigest,omitempty"`
	Target             string                       `json:"target,omitempty"`
	EffectiveState     string                       `json:"effectiveState,omitempty"`
	NextRunAt          string                       `json:"nextRunAt,omitempty"`
	LastOccurrenceAt   string                       `json:"lastOccurrenceAt,omitempty"`
	LastOutcome        string                       `json:"lastOutcome,omitempty"`
	LastError          string                       `json:"lastError,omitempty"`
	AttentionTarget    string                       `json:"attentionTarget,omitempty"`
	RetryAt            string                       `json:"retryAt,omitempty"`
	RetryCount         int                          `json:"retryCount,omitempty"`
	Prepared           *schedulerPreparedOccurrence `json:"preparedOccurrence,omitempty"`
}

type schedulerPreparedOccurrence struct {
	ScheduleID            string                    `json:"scheduleId"`
	ScheduleRevision      uint64                    `json:"scheduleRevision"`
	OccurrenceID          string                    `json:"occurrenceId"`
	MessageID             string                    `json:"messageId"`
	Target                string                    `json:"target"`
	Text                  string                    `json:"text"`
	ScheduledFor          string                    `json:"scheduledFor"`
	CoalescedThrough      string                    `json:"coalescedThrough,omitempty"`
	CoalescedCount        int                       `json:"coalescedCount,omitempty"`
	CronEnumerationCapped bool                      `json:"cronEnumerationCapped,omitempty"`
	EnumeratedThrough     string                    `json:"enumeratedThrough,omitempty"`
	EnumeratedCount       int                       `json:"enumeratedCount,omitempty"`
	RecoveryCutoff        string                    `json:"recoveryCutoff,omitempty"`
	NextRunAt             string                    `json:"nextRunAt,omitempty"`
	Reason                string                    `json:"reason"`
	Causation             *resourceMessageCausation `json:"causation"`
}

type resourceMailboxMeta struct {
	Version     int    `json:"version"`
	InstanceID  string `json:"instanceId"`
	ResourceID  string `json:"resourceId"`
	ResourceKey string `json:"resourceKey"`
}

type resourceMailboxLocator struct {
	Version     int    `json:"version"`
	MessageID   string `json:"messageId"`
	ResourceID  string `json:"resourceId"`
	ResourceKey string `json:"resourceKey"`
	State       string `json:"state"`
	UpdatedAt   string `json:"updatedAt"`
}

type resourceMailboxTxn struct {
	Version       int    `json:"version"`
	HotTemp       string `json:"hotTemp"`
	ReceiptTemp   string `json:"receiptTemp"`
	OutboxTemp    string `json:"outboxTemp"`
	SchedulerTemp string `json:"schedulerTemp"`
}

// resourceMailboxHotMarker is the durable membership record for a resource
// whose mailbox still needs reconciliation. Marker files are deliberately
// independent per resource so concurrent mailbox mutations do not need to
// read-modify-write a workspace-wide JSON index.
type resourceMailboxHotMarker struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instanceId"`
	ResourceID string `json:"resourceId"`
}

type resourceMailboxHotIndexReady struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instanceId"`
}

var (
	resourceMailboxLocks       sync.Map
	resourceMailboxAggregateMu sync.Mutex
	resourceMailboxHotIndexMu  sync.Mutex
)

func resourceMailboxResourcesRoot(workspacePath string) string {
	return filepath.Join(agentRoot(workspacePath), "resources")
}

func resourceMailboxLocatorRoot(workspacePath string) string {
	return filepath.Join(resourceMailboxResourcesRoot(workspacePath), ".message-locations")
}

func resourceMailboxHotIndexRoot(workspacePath string) string {
	return filepath.Join(resourceMailboxResourcesRoot(workspacePath), ".hot")
}

func resourceMailboxHotMarkerPath(workspacePath, resourceID string) string {
	return filepath.Join(resourceMailboxHotIndexRoot(workspacePath), resourceMailboxKey(mailboxInstanceID(workspacePath), resourceID)+".json")
}

func resourceMailboxHotIndexReadyPath(workspacePath string) string {
	return filepath.Join(resourceMailboxHotIndexRoot(workspacePath), "ready.json")
}

func mailboxInstanceID(workspacePath string) string {
	if instanceID, err := workspaceInstanceID(workspacePath); err == nil && strings.TrimSpace(instanceID) != "" {
		return strings.TrimSpace(instanceID)
	}
	abs, err := filepath.Abs(workspacePath)
	if err != nil {
		abs = workspacePath
	}
	sum := sha256.Sum256([]byte("uninitialized:" + filepath.Clean(abs)))
	return "uninitialized-" + hex.EncodeToString(sum[:8])
}

func resourceMailboxKey(instanceID, resourceID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(instanceID) + "\x00" + normalizedResourceID(resourceID)))
	return hex.EncodeToString(sum[:])
}

func resourceMailboxDirectory(workspacePath, resourceID string) (string, string, string, error) {
	instanceID := mailboxInstanceID(workspacePath)
	resourceID = normalizedResourceID(resourceID)
	key := resourceMailboxKey(instanceID, resourceID)
	return filepath.Join(resourceMailboxResourcesRoot(workspacePath), key), instanceID, key, nil
}

func resourceMailboxHotPath(directory string) string { return filepath.Join(directory, "hot.json") }
func resourceMailboxReceiptPath(directory string) string {
	return filepath.Join(directory, "receipts.json")
}
func resourceMailboxOutboxPath(directory string) string {
	return filepath.Join(directory, "outbox.json")
}
func resourceMailboxSchedulerPath(directory string) string {
	return filepath.Join(directory, "scheduler.json")
}
func resourceMailboxMetaPath(directory string) string { return filepath.Join(directory, "meta.json") }
func resourceMailboxTxnPath(directory string) string  { return filepath.Join(directory, "commit.json") }

func resourceMailboxLocatorPath(workspacePath, messageID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(messageID)))
	return filepath.Join(resourceMailboxLocatorRoot(workspacePath), hex.EncodeToString(sum[:])+".json")
}

func resourceMailboxLock(workspacePath, resourceID string) *sync.Mutex {
	key := workspacePath + "\x00" + normalizedResourceID(resourceID)
	value, _ := resourceMailboxLocks.LoadOrStore(key, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func syncResourceMailboxDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func writeResourceMailboxFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + "." + newGenerationRecordID() + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
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
	return syncResourceMailboxDirectory(filepath.Dir(path))
}

func readResourceMailboxJSON(path string, target any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	return true, nil
}

func writeResourceMailboxJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeResourceMailboxFile(path, data, 0o600)
}

// Locator files are only an acceleration index. The resource documents and
// their commit marker are authoritative, and mailboxMessageByID can rebuild
// the index by scanning the bounded per-resource stores. Keep locator writes
// atomic, but do not add a directory fsync to every mailbox state transition.
func writeResourceMailboxIndexJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + "." + newGenerationRecordID() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func recoverResourceMailboxTransaction(directory string) error {
	var transaction resourceMailboxTxn
	found, err := readResourceMailboxJSON(resourceMailboxTxnPath(directory), &transaction)
	if err != nil || !found {
		return err
	}
	for _, item := range []struct{ temp, final string }{
		{transaction.HotTemp, resourceMailboxHotPath(directory)},
		{transaction.ReceiptTemp, resourceMailboxReceiptPath(directory)},
		{transaction.OutboxTemp, resourceMailboxOutboxPath(directory)},
		{transaction.SchedulerTemp, resourceMailboxSchedulerPath(directory)},
	} {
		if strings.TrimSpace(item.temp) == "" {
			continue
		}
		if _, statErr := os.Stat(item.temp); statErr == nil {
			if renameErr := os.Rename(item.temp, item.final); renameErr != nil {
				return renameErr
			}
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	if err := os.Remove(resourceMailboxTxnPath(directory)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncResourceMailboxDirectory(directory)
}

func cloneNotificationReceipt(receipt *resourceNotificationReceipt) *resourceNotificationReceipt {
	if receipt == nil {
		return nil
	}
	cloned := *receipt
	return &cloned
}

func cloneMailboxCausation(causation *resourceMessageCausation) *resourceMessageCausation {
	if causation == nil {
		return nil
	}
	cloned := *causation
	return &cloned
}

func cloneProviderMessageContext(context *providerMessageContext) *providerMessageContext {
	if context == nil {
		return nil
	}
	cloned := *context
	if context.OpenerSender != nil {
		sender := *context.OpenerSender
		cloned.OpenerSender = &sender
	}
	return &cloned
}

func receiptFromMailboxMessage(message resourceMailboxMessage) resourceMailboxReceipt {
	var sender *agentHubMessageSender
	if message.Sender != nil {
		value := *message.Sender
		sender = &value
	}
	return resourceMailboxReceipt{
		ID: message.ID, Sequence: message.Sequence, ResourceID: normalizedResourceID(message.ResourceID),
		Role: message.Role, Sender: sender, SenderWorkspaceInstanceID: message.SenderWorkspaceInstanceID,
		SubscribeResult: message.SubscribeResult, ResultSubscriptionStatus: message.ResultSubscriptionStatus, ResultOperationID: message.ResultOperationID,
		Type: message.Type, Causation: cloneMailboxCausation(message.Causation), Notification: cloneNotificationReceipt(message.Notification),
		RequestedMode: message.RequestedMode, ActualMode: message.ActualMode, ModeFrozen: message.ModeFrozen, NonPromotable: message.NonPromotable,
		DowngradeReason: message.DowngradeReason, Status: message.Status, AcceptedAt: message.AcceptedAt,
		UpdatedAt: message.UpdatedAt, DeliveredAt: message.DeliveredAt, TerminalAt: message.TerminalAt,
		TurnTerminalAt: message.TurnTerminalAt, GenerationID: message.GenerationID,
		AgentHubSessionID: message.AgentHubSessionID, TurnID: message.TurnID, ProviderContext: cloneProviderMessageContext(message.ProviderContext), InterruptTurnID: message.InterruptTurnID,
		InterruptAt: message.InterruptAt, PromotedAt: message.PromotedAt, LastError: message.LastError,
		LastErrorCode:          message.LastErrorCode,
		subscribeResultPresent: message.subscribeResultPresent,
	}
}

func mailboxMessageFromReceipt(receipt resourceMailboxReceipt) resourceMailboxMessage {
	var sender *agentHubMessageSender
	if receipt.Sender != nil {
		value := *receipt.Sender
		sender = &value
	}
	return resourceMailboxMessage{
		ID: receipt.ID, Sequence: receipt.Sequence, ResourceID: normalizedResourceID(receipt.ResourceID),
		Role: receipt.Role, Sender: sender, SenderWorkspaceInstanceID: receipt.SenderWorkspaceInstanceID,
		SubscribeResult: receipt.SubscribeResult, ResultSubscriptionStatus: receipt.ResultSubscriptionStatus, ResultOperationID: receipt.ResultOperationID,
		Type: receipt.Type, Causation: cloneMailboxCausation(receipt.Causation), Notification: cloneNotificationReceipt(receipt.Notification),
		RequestedMode: receipt.RequestedMode, ActualMode: receipt.ActualMode, ModeFrozen: receipt.ModeFrozen, NonPromotable: receipt.NonPromotable,
		DowngradeReason: receipt.DowngradeReason, Status: receipt.Status, AcceptedAt: receipt.AcceptedAt,
		UpdatedAt: receipt.UpdatedAt, DeliveredAt: receipt.DeliveredAt, TerminalAt: receipt.TerminalAt,
		TurnTerminalAt: receipt.TurnTerminalAt, GenerationID: receipt.GenerationID,
		AgentHubSessionID: receipt.AgentHubSessionID, TurnID: receipt.TurnID, ProviderContext: cloneProviderMessageContext(receipt.ProviderContext), InterruptTurnID: receipt.InterruptTurnID,
		InterruptAt: receipt.InterruptAt, PromotedAt: receipt.PromotedAt, LastError: receipt.LastError,
		LastErrorCode: receipt.LastErrorCode, receipt: true,
		subscribeResultPresent: receipt.subscribeResultPresent,
	}
}

func cloneMailboxOperation(operation resourceMailboxNotificationOp) resourceMailboxNotificationOp {
	cloned := operation
	cloned.SourceMessageIDs = append([]string(nil), operation.SourceMessageIDs...)
	if operation.GeneratedSender != nil {
		value := *operation.GeneratedSender
		cloned.GeneratedSender = &value
	}
	cloned.GeneratedCausation = cloneMailboxCausation(operation.GeneratedCausation)
	return cloned
}

func mailboxOperationSourceIDs(operation resourceMailboxNotificationOp) []string {
	ids := make([]string, 0, 1+len(operation.SourceMessageIDs))
	seen := make(map[string]bool, 1+len(operation.SourceMessageIDs))
	appendID := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		ids = append(ids, value)
	}
	appendID(operation.SourceMessageID)
	for _, value := range operation.SourceMessageIDs {
		appendID(value)
	}
	return ids
}

func mailboxOperationHasSource(operation resourceMailboxNotificationOp, messageID string) bool {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false
	}
	for _, sourceID := range mailboxOperationSourceIDs(operation) {
		if sourceID == messageID {
			return true
		}
	}
	return false
}

func normalizeMailboxOperationSources(operation *resourceMailboxNotificationOp) {
	ids := mailboxOperationSourceIDs(*operation)
	operation.SourceMessageIDs = ids
	if len(ids) > 0 {
		operation.SourceMessageID = ids[0]
	}
}

func cloneResourceMailboxReceipt(receipt resourceMailboxReceipt) resourceMailboxReceipt {
	return receiptFromMailboxMessage(mailboxMessageFromReceipt(receipt))
}

func cloneResourceMailboxStore(store resourceMailboxStore) resourceMailboxStore {
	cloned := store
	cloned.Mailbox.Messages = make([]resourceMailboxMessage, 0, len(store.Mailbox.Messages))
	for _, message := range store.Mailbox.Messages {
		cloned.Mailbox.Messages = append(cloned.Mailbox.Messages, cloneMailboxMessage(message))
	}
	cloned.Receipts.Receipts = make([]resourceMailboxReceipt, 0, len(store.Receipts.Receipts))
	for _, receipt := range store.Receipts.Receipts {
		cloned.Receipts.Receipts = append(cloned.Receipts.Receipts, cloneResourceMailboxReceipt(receipt))
	}
	cloned.Receipts.Expired = append([]resourceMailboxExpiredEntry(nil), store.Receipts.Expired...)
	cloned.Outbox.Operations = make([]resourceMailboxNotificationOp, 0, len(store.Outbox.Operations))
	for _, operation := range store.Outbox.Operations {
		cloned.Outbox.Operations = append(cloned.Outbox.Operations, cloneMailboxOperation(operation))
	}
	cloned.Scheduler.Schedules = cloneSchedulerRuntimes(store.Scheduler.Schedules)
	return cloned
}

func cloneSchedulerRuntimes(values map[string]schedulerScheduleRuntime) map[string]schedulerScheduleRuntime {
	if values == nil {
		return nil
	}
	result := make(map[string]schedulerScheduleRuntime, len(values))
	for id, runtime := range values {
		if runtime.Prepared != nil {
			prepared := *runtime.Prepared
			prepared.Causation = cloneMailboxCausation(runtime.Prepared.Causation)
			runtime.Prepared = &prepared
		}
		result[id] = runtime
	}
	return result
}

func defaultResourceMailboxStore(workspacePath, resourceID string) resourceMailboxStore {
	directory, instanceID, key, _ := resourceMailboxDirectory(workspacePath, resourceID)
	resourceID = normalizedResourceID(resourceID)
	return resourceMailboxStore{
		ResourceID: resourceID, InstanceID: instanceID, ResourceKey: key, Directory: directory,
		Mailbox:   resourceMailbox{Version: resourceMailboxVersion, Messages: []resourceMailboxMessage{}},
		Receipts:  resourceMailboxReceiptDocument{Version: resourceMailboxStoreVersion, ResourceID: resourceID, Receipts: []resourceMailboxReceipt{}, Expired: []resourceMailboxExpiredEntry{}},
		Outbox:    resourceMailboxOutboxDocument{Version: resourceMailboxStoreVersion, ResourceID: resourceID, Operations: []resourceMailboxNotificationOp{}},
		Scheduler: resourceSchedulerCheckpoint{Version: resourceMailboxStoreVersion, ResourceID: resourceID},
	}
}

func loadResourceMailboxStoreInternal(workspacePath, resourceID string) (resourceMailboxStore, error) {
	store := defaultResourceMailboxStore(workspacePath, resourceID)
	if _, err := os.Stat(store.Directory); os.IsNotExist(err) {
		return store, nil
	} else if err != nil {
		return store, err
	}
	if err := recoverResourceMailboxTransaction(store.Directory); err != nil {
		return store, fmt.Errorf("recover mailbox store %s: %w", store.ResourceID, err)
	}
	var meta resourceMailboxMeta
	if found, err := readResourceMailboxJSON(resourceMailboxMetaPath(store.Directory), &meta); err != nil {
		return store, err
	} else if found {
		if meta.Version != resourceMailboxStoreVersion || meta.ResourceID != store.ResourceID || meta.InstanceID != store.InstanceID {
			return store, fmt.Errorf("resource mailbox metadata mismatch for %s", store.ResourceID)
		}
	}
	var hot resourceMailboxHotDocument
	if found, err := readResourceMailboxJSON(resourceMailboxHotPath(store.Directory), &hot); err != nil {
		return store, err
	} else if found {
		if hot.Version != resourceMailboxStoreVersion {
			return store, fmt.Errorf("unsupported hot mailbox version %d", hot.Version)
		}
		store.Mailbox.NextSequence = hot.NextSequence
		for _, message := range hot.Messages {
			message.ResourceID = normalizedResourceID(message.ResourceID)
			message.receipt = false
			normalizeStoredMailboxMessage(&message)
			store.Mailbox.Messages = append(store.Mailbox.Messages, cloneMailboxMessage(message))
			if message.Sequence > store.Mailbox.NextSequence {
				store.Mailbox.NextSequence = message.Sequence
			}
		}
	}
	if found, err := readResourceMailboxJSON(resourceMailboxReceiptPath(store.Directory), &store.Receipts); err != nil {
		return store, err
	} else if found {
		if store.Receipts.Version != resourceMailboxStoreVersion {
			return store, fmt.Errorf("unsupported receipt store version %d", store.Receipts.Version)
		}
		if store.Receipts.Receipts == nil {
			store.Receipts.Receipts = []resourceMailboxReceipt{}
		}
		if store.Receipts.Expired == nil {
			store.Receipts.Expired = []resourceMailboxExpiredEntry{}
		}
		for index := range store.Receipts.Receipts {
			normalizeStoredMailboxReceipt(&store.Receipts.Receipts[index])
		}
	}
	if found, err := readResourceMailboxJSON(resourceMailboxOutboxPath(store.Directory), &store.Outbox); err != nil {
		return store, err
	} else if found {
		if store.Outbox.Version != resourceMailboxStoreVersion {
			return store, fmt.Errorf("unsupported outbox store version %d", store.Outbox.Version)
		}
		if store.Outbox.Operations == nil {
			store.Outbox.Operations = []resourceMailboxNotificationOp{}
		}
		for i := range store.Outbox.Operations {
			store.Outbox.Operations[i] = cloneMailboxOperation(store.Outbox.Operations[i])
		}
	}
	if found, err := readResourceMailboxJSON(resourceMailboxSchedulerPath(store.Directory), &store.Scheduler); err != nil {
		return store, err
	} else if found && store.Scheduler.Version != resourceMailboxStoreVersion {
		return store, fmt.Errorf("unsupported scheduler checkpoint version %d", store.Scheduler.Version)
	}

	// A crash after the receipt document was committed but before hot.json was
	// renamed can expose the same ID in both files. Hot wins because it is the
	// conservative, retryable state.
	byID := make(map[string]resourceMailboxMessage, len(store.Mailbox.Messages)+len(store.Receipts.Receipts))
	for _, message := range store.Mailbox.Messages {
		byID[message.ID] = cloneMailboxMessage(message)
	}
	for _, receipt := range store.Receipts.Receipts {
		if _, exists := byID[receipt.ID]; exists {
			continue
		}
		normalizeStoredMailboxReceipt(&receipt)
		byID[receipt.ID] = mailboxMessageFromReceipt(receipt)
		if receipt.Sequence > store.Mailbox.NextSequence {
			store.Mailbox.NextSequence = receipt.Sequence
		}
	}
	store.Mailbox.Messages = store.Mailbox.Messages[:0]
	for _, message := range byID {
		store.Mailbox.Messages = append(store.Mailbox.Messages, message)
	}
	sort.SliceStable(store.Mailbox.Messages, func(i, j int) bool {
		if store.Mailbox.Messages[i].Sequence != store.Mailbox.Messages[j].Sequence {
			return store.Mailbox.Messages[i].Sequence < store.Mailbox.Messages[j].Sequence
		}
		return store.Mailbox.Messages[i].ID < store.Mailbox.Messages[j].ID
	})
	return store, nil
}

func mailboxMessageNeedsHot(message resourceMailboxMessage) bool {
	if message.receipt {
		return false
	}
	switch message.Status {
	case resourceMessageQueued, resourceMessageDelivering, resourceMessageInterrupting:
		return true
	}
	if message.Notification != nil && message.Notification.Status != resourceNotificationDelivered && message.Notification.Status != resourceNotificationTerminal {
		return true
	}
	if message.ResultSubscriptionStatus == resourceResultSubscriptionPending && message.Notification == nil {
		return true
	}
	if message.Type == "" && message.Status == resourceMessageDelivered && message.SubscribeResult &&
		message.ResultSubscriptionStatus == "" && message.Notification == nil && message.ActualMode != resourceMessageModeSteer &&
		message.Sender != nil && isStablePUAResourceID(message.Sender.ID) && strings.TrimSpace(message.SenderWorkspaceInstanceID) != "" &&
		strings.TrimSpace(message.GenerationID) != "" {
		return true
	}
	if message.Type == "" && (message.Status == resourceMessageCancelled || message.Status == resourceMessageUndeliverable || message.Status == resourceMessageDeliveryUnknown) &&
		message.Role == "agent" && message.Sender != nil && strings.TrimSpace(message.Sender.ID) != "" && strings.TrimSpace(message.SenderWorkspaceInstanceID) != "" && message.Notification == nil {
		return true
	}
	return false
}

func resourceMailboxStoreNeedsHotWork(store resourceMailboxStore) bool {
	for _, message := range store.Mailbox.Messages {
		if message.receipt {
			continue
		}
		if mailboxMessageNeedsHot(message) {
			return true
		}
	}
	for _, operation := range store.Outbox.Operations {
		if operation.Status != resourceNotificationDelivered && operation.Status != resourceNotificationTerminal {
			return true
		}
	}
	return false
}

// resourceMailboxHasHotWork reports whether the mailbox controller still owns
// any work for a resource. Keep callers on the same predicate that controls
// hot-store persistence and hot-index membership so new controller obligations
// cannot silently drift from resource busy checks.
func resourceMailboxHasHotWork(workspacePath, resourceID string) (bool, error) {
	store, err := loadResourceMailboxStoreForRead(workspacePath, resourceID)
	if err != nil {
		return false, err
	}
	return resourceMailboxStoreNeedsHotWork(store), nil
}

func resourceMailboxStoreNeedsCompaction(store resourceMailboxStore) bool {
	for _, message := range store.Mailbox.Messages {
		if message.receipt || mailboxMessageNeedsHot(message) {
			continue
		}
		return true
	}
	return false
}

func writeResourceMailboxHotMarkerLocked(workspacePath, resourceID string) error {
	resourceID = normalizedResourceID(resourceID)
	if resourceID == "" {
		return errors.New("resource mailbox hot marker requires a resource id")
	}
	return writeResourceMailboxJSON(resourceMailboxHotMarkerPath(workspacePath, resourceID), resourceMailboxHotMarker{
		Version: resourceMailboxHotIndexVersion, InstanceID: mailboxInstanceID(workspacePath), ResourceID: resourceID,
	})
}

func removeResourceMailboxHotMarkerLocked(workspacePath, resourceID string) error {
	path := resourceMailboxHotMarkerPath(workspacePath, resourceID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func setResourceMailboxHotMarker(workspacePath, resourceID string, active bool) error {
	resourceMailboxHotIndexMu.Lock()
	defer resourceMailboxHotIndexMu.Unlock()
	if active {
		return writeResourceMailboxHotMarkerLocked(workspacePath, resourceID)
	}
	return removeResourceMailboxHotMarkerLocked(workspacePath, resourceID)
}

// readResourceMailboxHotIndexLocked returns the active set only when the
// workspace marker directory has a valid ready marker. A malformed or partial
// index is treated as missing so the caller can rebuild it from authoritative
// resource stores.
func readResourceMailboxHotIndexLocked(workspacePath string) ([]string, bool, error) {
	root := resourceMailboxHotIndexRoot(workspacePath)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		// An uninitialized workspace has no mailbox stores and is already known to
		// have an empty active set. If resource stores do exist, however, a missing
		// hot directory is an incomplete index and must be rebuilt.
		if _, resourceErr := os.Stat(resourceMailboxResourcesRoot(workspacePath)); os.IsNotExist(resourceErr) {
			return []string{}, true, nil
		}
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}

	var ready resourceMailboxHotIndexReady
	found, err := readResourceMailboxJSON(resourceMailboxHotIndexReadyPath(workspacePath), &ready)
	if err != nil {
		return nil, false, nil
	}
	if !found || ready.Version != resourceMailboxHotIndexVersion || ready.InstanceID != mailboxInstanceID(workspacePath) {
		return nil, false, nil
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "ready.json" {
			continue
		}
		var marker resourceMailboxHotMarker
		found, markerErr := readResourceMailboxJSON(filepath.Join(root, entry.Name()), &marker)
		if markerErr != nil || !found || marker.Version != resourceMailboxHotIndexVersion || marker.InstanceID != mailboxInstanceID(workspacePath) {
			return nil, false, nil
		}
		resourceID := normalizedResourceID(marker.ResourceID)
		if resourceID == "" || filepath.Base(resourceMailboxHotMarkerPath(workspacePath, resourceID)) != entry.Name() || seen[resourceID] {
			return nil, false, nil
		}
		seen[resourceID] = true
		ids = append(ids, resourceID)
	}
	sort.Strings(ids)
	return ids, true, nil
}

// rebuildResourceMailboxHotIndex is the one permitted full mailbox audit. It
// runs during startup recovery or when the durable marker set is absent/corrupt
// and reconstructs only the small per-resource active markers.
func rebuildResourceMailboxHotIndex(workspacePath string) ([]string, error) {
	resourceMailboxHotIndexMu.Lock()
	defer resourceMailboxHotIndexMu.Unlock()

	root := resourceMailboxHotIndexRoot(workspacePath)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		if _, resourceErr := os.Stat(resourceMailboxResourcesRoot(workspacePath)); os.IsNotExist(resourceErr) {
			return []string{}, nil
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	resourceIDs, err := listResourceMailboxResourceIDs(workspacePath)
	if err != nil {
		return nil, err
	}
	active := make(map[string]bool, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		store, loadErr := loadResourceMailboxStoreInternal(workspacePath, resourceID)
		if loadErr != nil {
			return nil, loadErr
		}
		if resourceMailboxStoreNeedsHotWork(store) {
			active[normalizedResourceID(resourceID)] = true
		}
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(root, entry.Name())); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	ids := make([]string, 0, len(active))
	for _, resourceID := range resourceIDs {
		resourceID = normalizedResourceID(resourceID)
		if !active[resourceID] {
			continue
		}
		if err := writeResourceMailboxHotMarkerLocked(workspacePath, resourceID); err != nil {
			return nil, err
		}
		ids = append(ids, resourceID)
	}
	if err := writeResourceMailboxJSON(resourceMailboxHotIndexReadyPath(workspacePath), resourceMailboxHotIndexReady{
		Version: resourceMailboxHotIndexVersion, InstanceID: mailboxInstanceID(workspacePath),
	}); err != nil {
		return nil, err
	}
	if err := syncResourceMailboxDirectory(root); err != nil {
		return nil, err
	}
	return ids, nil
}

func listHotResourceMailboxResourceIDs(workspacePath string) ([]string, error) {
	resourceMailboxHotIndexMu.Lock()
	ids, ready, err := readResourceMailboxHotIndexLocked(workspacePath)
	resourceMailboxHotIndexMu.Unlock()
	if err != nil {
		return nil, err
	}
	if ready {
		return ids, nil
	}
	return rebuildResourceMailboxHotIndex(workspacePath)
}

// normalizeStoredMailboxMessage applies compatibility rules that must not
// change public JSON decoding. It prevents legacy durable inputs from gaining
// subscriptions that the current runtime would not create.
func normalizeStoredMailboxMessage(message *resourceMailboxMessage) {
	if message == nil {
		return
	}
	// Fixed-base generated messages predate the explicit promotion policy.
	// Their system role plus typed causation is the durable signature imposed
	// by acceptGeneratedMailboxMessage, while ordinary persisted enqueues have
	// neither. ModeFrozen cannot be used here: older ordinary enqueues waiting
	// behind an active Turn were persisted frozen but remained promotable.
	if message.Role == "system" && message.Type != "" && message.Causation != nil {
		message.NonPromotable = true
	}
	if !message.subscribeResultPresent && message.Status == resourceMessageDelivered && message.Notification == nil && message.Type == "" {
		message.SubscribeResult = false
		message.ResultSubscriptionStatus = resourceResultSubscriptionNone
	}
	// Older versions subscribed every agent input bound to a Turn, including
	// steer inputs. Suppress only subscriptions that have not created a durable
	// callback operation yet; an operation already in flight must finish its
	// crash-safe delivery protocol.
	if message.Status == resourceMessageDelivered && message.Notification == nil && message.Type == "" &&
		message.ActualMode == resourceMessageModeSteer &&
		(message.ResultSubscriptionStatus == "" || message.ResultSubscriptionStatus == resourceResultSubscriptionPending) {
		message.ResultSubscriptionStatus = resourceResultSubscriptionNone
		message.ResultOperationID = ""
	}
}

func normalizeStoredMailboxReceipt(receipt *resourceMailboxReceipt) {
	if receipt == nil {
		return
	}
	if receipt.Role == "system" && receipt.Type != "" && receipt.Causation != nil {
		receipt.NonPromotable = true
	}
	if !receipt.subscribeResultPresent && receipt.Status == resourceMessageDelivered && receipt.Notification == nil && receipt.Type == "" {
		receipt.SubscribeResult = false
		receipt.ResultSubscriptionStatus = resourceResultSubscriptionNone
	}
	if receipt.Status == resourceMessageDelivered && receipt.Notification == nil && receipt.Type == "" &&
		receipt.ActualMode == resourceMessageModeSteer &&
		(receipt.ResultSubscriptionStatus == "" || receipt.ResultSubscriptionStatus == resourceResultSubscriptionPending) {
		receipt.ResultSubscriptionStatus = resourceResultSubscriptionNone
		receipt.ResultOperationID = ""
	}
}

func mailboxMessageRetentionTime(message resourceMailboxMessage) time.Time {
	for _, value := range []string{message.TerminalAt, message.UpdatedAt, message.AcceptedAt} {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func mailboxReceiptRetentionTime(receipt resourceMailboxReceipt) time.Time {
	return mailboxMessageRetentionTime(mailboxMessageFromReceipt(receipt))
}

func uniqueMailboxReceipts(receipts []resourceMailboxReceipt) []resourceMailboxReceipt {
	byID := make(map[string]resourceMailboxReceipt, len(receipts))
	for _, receipt := range receipts {
		if strings.TrimSpace(receipt.ID) == "" {
			continue
		}
		byID[receipt.ID] = receipt
	}
	result := make([]resourceMailboxReceipt, 0, len(byID))
	for _, receipt := range byID {
		result = append(result, receipt)
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := mailboxReceiptRetentionTime(result[i]), mailboxReceiptRetentionTime(result[j])
		if !left.Equal(right) {
			return left.After(right)
		}
		if result[i].Sequence != result[j].Sequence {
			return result[i].Sequence > result[j].Sequence
		}
		return result[i].ID > result[j].ID
	})
	return result
}

func schedulerPreparedMessagePins(workspacePath string, store resourceMailboxStore) (map[string]bool, error) {
	checkpoint := store.Scheduler
	if store.ResourceID != app.SchedulerResourceID {
		// Target persistence already holds this resource's mailbox lock. Read the
		// Scheduler's atomically renamed checkpoint directly so delivery never
		// acquires the source lock in the opposite order. An in-progress source
		// commit exposes the older final file, which only retains evidence longer.
		schedulerStore := defaultResourceMailboxStore(workspacePath, app.SchedulerResourceID)
		var persisted resourceSchedulerCheckpoint
		found, err := readResourceMailboxJSON(resourceMailboxSchedulerPath(schedulerStore.Directory), &persisted)
		if err != nil {
			return nil, err
		}
		if !found {
			return map[string]bool{}, nil
		}
		checkpoint = persisted
	}

	pins := make(map[string]bool)
	if store.ResourceID == app.SchedulerResourceID {
		for _, message := range store.Mailbox.Messages {
			if message.Type != resourceMessageTypeSchedulerTick || strings.TrimSpace(message.TurnTerminalAt) != "" {
				continue
			}
			switch message.Status {
			case resourceMessageDelivering, resourceMessageInterrupting, resourceMessageDeliveryUnknown, resourceMessageDelivered:
				// Native migration must reconcile uncertain acceptance and
				// delivered Turns against AgentHub before discarding their
				// identity. Otherwise startup compaction can erase the only
				// receipt that distinguishes an obsolete active Turn from an
				// unrelated Scheduler chat.
				pins[strings.TrimSpace(message.ID)] = true
			}
		}
	}
	for _, runtime := range checkpoint.Schedules {
		prepared := runtime.Prepared
		if prepared == nil || strings.TrimSpace(prepared.MessageID) == "" {
			continue
		}
		target := strings.TrimSpace(prepared.Target)
		if target == "" {
			target = strings.TrimSpace(runtime.Target)
		}
		if target == "" || normalizedResourceID(target) != store.ResourceID {
			continue
		}
		pins[strings.TrimSpace(prepared.MessageID)] = true
	}
	return pins, nil
}

func prepareResourceMailboxDocuments(store resourceMailboxStore, pinnedMessageIDs map[string]bool) (resourceMailboxHotDocument, resourceMailboxReceiptDocument, resourceMailboxOutboxDocument, resourceSchedulerCheckpoint) {
	byID := make(map[string]resourceMailboxMessage, len(store.Mailbox.Messages))
	for _, message := range store.Mailbox.Messages {
		if strings.TrimSpace(message.ID) == "" {
			continue
		}
		message.ResourceID = normalizedResourceID(message.ResourceID)
		byID[message.ID] = cloneMailboxMessage(message)
	}
	messages := make([]resourceMailboxMessage, 0, len(byID))
	for _, message := range byID {
		messages = append(messages, message)
	}
	hot := resourceMailboxHotDocument{Version: resourceMailboxStoreVersion, ResourceID: store.ResourceID, NextSequence: store.Mailbox.NextSequence, Messages: []resourceMailboxMessage{}}
	receipts := append([]resourceMailboxReceipt(nil), store.Receipts.Receipts...)
	hotIDs := make(map[string]bool)
	for _, message := range byID {
		if mailboxMessageNeedsHot(message) {
			message.receipt = false
			hot.Messages = append(hot.Messages, message)
			hotIDs[message.ID] = true
			continue
		}
		receipts = append(receipts, receiptFromMailboxMessage(message))
	}
	if len(hotIDs) > 0 {
		filtered := receipts[:0]
		for _, receipt := range receipts {
			if !hotIDs[receipt.ID] {
				filtered = append(filtered, receipt)
			}
		}
		receipts = filtered
	}
	for _, message := range hot.Messages {
		if message.Sequence > hot.NextSequence {
			hot.NextSequence = message.Sequence
		}
	}
	receipts = uniqueMailboxReceipts(receipts)
	now := time.Now()
	retained := make([]resourceMailboxReceipt, 0, len(receipts))
	dropped := make([]resourceMailboxExpiredEntry, 0)
	ordinaryReceiptIndex := 0
	for _, receipt := range receipts {
		pinned := pinnedMessageIDs[receipt.ID]
		receiptTime := mailboxReceiptRetentionTime(receipt)
		keepByTime := pinned || receiptTime.IsZero() || resourceMailboxReceiptRetentionWindow <= 0 || now.Sub(receiptTime) <= resourceMailboxReceiptRetentionWindow
		keepByCount := pinned || resourceMailboxReceiptRetentionCount <= 0 || ordinaryReceiptIndex < resourceMailboxReceiptRetentionCount
		if !pinned {
			ordinaryReceiptIndex++
		}
		if keepByTime && keepByCount {
			retained = append(retained, receipt)
		} else {
			dropped = append(dropped, resourceMailboxExpiredEntry{ID: receipt.ID, ExpiredAt: now.Format(time.RFC3339Nano)})
		}
	}
	expired := append([]resourceMailboxExpiredEntry(nil), store.Receipts.Expired...)
	expired = append(expired, dropped...)
	expiredByID := make(map[string]resourceMailboxExpiredEntry, len(expired))
	for _, entry := range expired {
		if strings.TrimSpace(entry.ID) == "" {
			continue
		}
		expiredByID[entry.ID] = entry
	}
	expired = expired[:0]
	for _, entry := range expiredByID {
		parsed, parseErr := time.Parse(time.RFC3339Nano, entry.ExpiredAt)
		if !pinnedMessageIDs[entry.ID] && parseErr == nil && resourceMailboxExpiredRetentionWindow > 0 && now.Sub(parsed) > resourceMailboxExpiredRetentionWindow {
			continue
		}
		expired = append(expired, entry)
	}
	knownIDs := make(map[string]bool, len(hot.Messages)+len(retained))
	for _, message := range hot.Messages {
		knownIDs[message.ID] = true
	}
	for _, receipt := range retained {
		knownIDs[receipt.ID] = true
	}
	filteredExpired := expired[:0]
	for _, entry := range expired {
		if !knownIDs[entry.ID] {
			filteredExpired = append(filteredExpired, entry)
		}
	}
	expired = filteredExpired
	sort.SliceStable(expired, func(i, j int) bool { return expired[i].ExpiredAt > expired[j].ExpiredAt })
	if resourceMailboxExpiredRetentionCount > 0 {
		bounded := make([]resourceMailboxExpiredEntry, 0, len(expired))
		ordinaryExpiredCount := 0
		for _, entry := range expired {
			if pinnedMessageIDs[entry.ID] || ordinaryExpiredCount < resourceMailboxExpiredRetentionCount {
				bounded = append(bounded, entry)
			}
			if !pinnedMessageIDs[entry.ID] {
				ordinaryExpiredCount++
			}
		}
		expired = bounded
	}
	receiptDoc := resourceMailboxReceiptDocument{Version: resourceMailboxStoreVersion, ResourceID: store.ResourceID, Receipts: retained, Expired: expired}
	outbox := store.Outbox
	outbox.Version = resourceMailboxStoreVersion
	outbox.ResourceID = store.ResourceID
	pending := make([]resourceMailboxNotificationOp, 0, len(outbox.Operations))
	for _, operation := range outbox.Operations {
		if operation.Status == resourceNotificationDelivered || operation.Status == resourceNotificationTerminal {
			continue
		}
		pending = append(pending, cloneMailboxOperation(operation))
	}
	outbox.Operations = pending
	scheduler := store.Scheduler
	scheduler.Version = resourceMailboxStoreVersion
	scheduler.ResourceID = store.ResourceID
	sort.SliceStable(hot.Messages, func(i, j int) bool {
		if hot.Messages[i].Sequence != hot.Messages[j].Sequence {
			return hot.Messages[i].Sequence < hot.Messages[j].Sequence
		}
		return hot.Messages[i].ID < hot.Messages[j].ID
	})
	sort.SliceStable(receiptDoc.Receipts, func(i, j int) bool {
		return receiptDoc.Receipts[i].Sequence < receiptDoc.Receipts[j].Sequence
	})
	return hot, receiptDoc, outbox, scheduler
}

func writeResourceMailboxStoreDocuments(directory string, meta resourceMailboxMeta, hot resourceMailboxHotDocument, receipts resourceMailboxReceiptDocument, outbox resourceMailboxOutboxDocument, scheduler resourceSchedulerCheckpoint) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(resourceMailboxMetaPath(directory)); os.IsNotExist(err) {
		if err := writeResourceMailboxMeta(directory, meta); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	encode := func(value any) ([]byte, error) {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(data, '\n'), nil
	}
	values := []struct {
		value any
		path  string
	}{
		{hot, resourceMailboxHotPath(directory)},
		{receipts, resourceMailboxReceiptPath(directory)},
		{outbox, resourceMailboxOutboxPath(directory)},
		{scheduler, resourceMailboxSchedulerPath(directory)},
	}
	temporary := make([]string, 0, len(values))
	removeTemporary := true
	defer func() {
		if removeTemporary {
			for _, path := range temporary {
				_ = os.Remove(path)
			}
		}
	}()
	for _, item := range values {
		data, err := encode(item.value)
		if err != nil {
			return err
		}
		tmp := item.path + "." + newGenerationRecordID() + ".tmp"
		file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
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
		temporary = append(temporary, tmp)
	}
	txn := resourceMailboxTxn{Version: resourceMailboxStoreVersion, HotTemp: temporary[0], ReceiptTemp: temporary[1], OutboxTemp: temporary[2], SchedulerTemp: temporary[3]}
	if err := writeResourceMailboxJSON(resourceMailboxTxnPath(directory), txn); err != nil {
		return err
	}
	for index, item := range values {
		if err := os.Rename(temporary[index], item.path); err != nil {
			return err
		}
	}
	if err := os.Remove(resourceMailboxTxnPath(directory)); err != nil && !os.IsNotExist(err) {
		return err
	}
	removeTemporary = false
	return syncResourceMailboxDirectory(directory)
}

func writeResourceMailboxMeta(directory string, meta resourceMailboxMeta) error {
	return writeResourceMailboxJSON(resourceMailboxMetaPath(directory), meta)
}

func mailboxStoreMeta(store resourceMailboxStore) resourceMailboxMeta {
	return resourceMailboxMeta{Version: resourceMailboxStoreVersion, InstanceID: store.InstanceID, ResourceID: store.ResourceID, ResourceKey: store.ResourceKey}
}

func mailboxStoreMessageIDs(store resourceMailboxStore) map[string]bool {
	ids := make(map[string]bool, len(store.Mailbox.Messages)+len(store.Receipts.Receipts))
	for _, message := range store.Mailbox.Messages {
		ids[message.ID] = true
	}
	for _, receipt := range store.Receipts.Receipts {
		ids[receipt.ID] = true
	}
	for _, expired := range store.Receipts.Expired {
		ids[expired.ID] = true
	}
	return ids
}

func updateResourceMailboxLocators(workspacePath string, store resourceMailboxStore, hot resourceMailboxHotDocument, receipts resourceMailboxReceiptDocument, before resourceMailboxStore) error {
	if err := os.MkdirAll(resourceMailboxLocatorRoot(workspacePath), 0o700); err != nil {
		return err
	}
	current := make(map[string]resourceMailboxLocator, len(hot.Messages)+len(receipts.Receipts)+len(receipts.Expired))
	updatedAt := time.Now().Format(time.RFC3339Nano)
	for _, message := range hot.Messages {
		current[message.ID] = resourceMailboxLocator{Version: resourceMailboxStoreVersion, MessageID: message.ID, ResourceID: store.ResourceID, ResourceKey: store.ResourceKey, State: "hot", UpdatedAt: updatedAt}
	}
	for _, receipt := range receipts.Receipts {
		current[receipt.ID] = resourceMailboxLocator{Version: resourceMailboxStoreVersion, MessageID: receipt.ID, ResourceID: store.ResourceID, ResourceKey: store.ResourceKey, State: "receipt", UpdatedAt: updatedAt}
	}
	for _, expired := range receipts.Expired {
		current[expired.ID] = resourceMailboxLocator{Version: resourceMailboxStoreVersion, MessageID: expired.ID, ResourceID: store.ResourceID, ResourceKey: store.ResourceKey, State: resourceMailboxExpiredState, UpdatedAt: expired.ExpiredAt}
	}
	for id, locator := range current {
		if err := writeResourceMailboxIndexJSON(resourceMailboxLocatorPath(workspacePath, id), locator); err != nil {
			return err
		}
	}
	previous := mailboxStoreMessageIDs(before)
	for id := range previous {
		if _, stillKnown := current[id]; stillKnown {
			continue
		}
		path := resourceMailboxLocatorPath(workspacePath, id)
		var locator resourceMailboxLocator
		found, err := readResourceMailboxJSON(path, &locator)
		if err != nil {
			return err
		}
		if found && locator.ResourceKey == store.ResourceKey {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func persistResourceMailboxStore(workspacePath string, store resourceMailboxStore, before resourceMailboxStore) error {
	pinnedMessageIDs, err := schedulerPreparedMessagePins(workspacePath, store)
	if err != nil {
		return err
	}
	hot, receipts, outbox, scheduler := prepareResourceMailboxDocuments(store, pinnedMessageIDs)
	active := resourceMailboxStoreNeedsHotWork(store)
	if active {
		// Keep the marker and mailbox commit in one ordering boundary. If the
		// process dies after the marker but before the mailbox files are renamed,
		// the next pass only performs a harmless extra read. The reverse ordering
		// could lose a newly accepted message from the periodic reconciler.
		resourceMailboxHotIndexMu.Lock()
		markerErr := writeResourceMailboxHotMarkerLocked(workspacePath, store.ResourceID)
		if markerErr == nil {
			markerErr = writeResourceMailboxStoreDocuments(store.Directory, mailboxStoreMeta(store), hot, receipts, outbox, scheduler)
		}
		resourceMailboxHotIndexMu.Unlock()
		if markerErr != nil {
			return markerErr
		}
	} else {
		if err := writeResourceMailboxStoreDocuments(store.Directory, mailboxStoreMeta(store), hot, receipts, outbox, scheduler); err != nil {
			return err
		}
		// Removal is deliberately best effort after the authoritative mailbox
		// commit. A stale marker is safe: it causes one bounded hot-store read,
		// never loss of a retryable item.
		resourceMailboxHotIndexMu.Lock()
		_ = removeResourceMailboxHotMarkerLocked(workspacePath, store.ResourceID)
		resourceMailboxHotIndexMu.Unlock()
	}
	store.Mailbox.NextSequence = hot.NextSequence
	// Locators are rebuildable and non-authoritative. A failed index update must
	// not turn a durable mailbox commit into a delivery failure; lookup falls
	// back to the bounded resource stores.
	_ = updateResourceMailboxLocators(workspacePath, store, hot, receipts, before)
	return nil
}

func loadResourceMailboxForResourceInternal(workspacePath, resourceID string) (resourceMailbox, error) {
	store, err := loadResourceMailboxStoreInternal(workspacePath, resourceID)
	if err != nil {
		return resourceMailbox{}, err
	}
	return store.Mailbox, nil
}

func loadResourceMailboxStoreForRead(workspacePath, resourceID string) (resourceMailboxStore, error) {
	lock := resourceMailboxLock(workspacePath, resourceID)
	lock.Lock()
	defer lock.Unlock()
	return loadResourceMailboxStoreInternal(workspacePath, resourceID)
}

func loadResourceMailboxForResource(workspacePath, resourceID string) (resourceMailbox, error) {
	store, err := loadResourceMailboxStoreForRead(workspacePath, resourceID)
	if err != nil {
		return resourceMailbox{}, err
	}
	return store.Mailbox, nil
}

func listResourceMailboxResourceIDs(workspacePath string) ([]string, error) {
	entries, err := os.ReadDir(resourceMailboxResourcesRoot(workspacePath))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	resourceIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		var meta resourceMailboxMeta
		found, readErr := readResourceMailboxJSON(resourceMailboxMetaPath(filepath.Join(resourceMailboxResourcesRoot(workspacePath), entry.Name())), &meta)
		if readErr != nil {
			return nil, readErr
		}
		if found && meta.Version == resourceMailboxStoreVersion && strings.TrimSpace(meta.ResourceID) != "" {
			resourceIDs = append(resourceIDs, meta.ResourceID)
		}
	}
	sort.Strings(resourceIDs)
	return resourceIDs, nil
}

// loadResourceMailbox is retained as an in-memory compatibility projection for
// history and tests. Production reconciliation uses the resource-scoped and
// hot-only helpers below, so this bounded aggregate never drives delivery.
func loadResourceMailbox(workspacePath string) (resourceMailbox, error) {
	resourceIDs, err := listResourceMailboxResourceIDs(workspacePath)
	if err != nil {
		return resourceMailbox{}, err
	}
	aggregate := resourceMailbox{Version: resourceMailboxVersion, Messages: []resourceMailboxMessage{}}
	for _, resourceID := range resourceIDs {
		mailbox, loadErr := loadResourceMailboxForResource(workspacePath, resourceID)
		if loadErr != nil {
			return resourceMailbox{}, loadErr
		}
		if mailbox.NextSequence > aggregate.NextSequence {
			aggregate.NextSequence = mailbox.NextSequence
		}
		aggregate.Messages = append(aggregate.Messages, mailbox.Messages...)
	}
	sort.SliceStable(aggregate.Messages, func(i, j int) bool {
		if aggregate.Messages[i].Sequence != aggregate.Messages[j].Sequence {
			return aggregate.Messages[i].Sequence < aggregate.Messages[j].Sequence
		}
		if aggregate.Messages[i].ResourceID != aggregate.Messages[j].ResourceID {
			return aggregate.Messages[i].ResourceID < aggregate.Messages[j].ResourceID
		}
		return aggregate.Messages[i].ID < aggregate.Messages[j].ID
	})
	return aggregate, nil
}

func loadHotResourceMailbox(workspacePath, resourceID string) (resourceMailbox, error) {
	mailbox, err := loadResourceMailboxForResource(workspacePath, resourceID)
	if err != nil {
		return resourceMailbox{}, err
	}
	hot := resourceMailbox{Version: resourceMailboxVersion, NextSequence: mailbox.NextSequence, Messages: []resourceMailboxMessage{}}
	for _, message := range mailbox.Messages {
		if !message.receipt {
			hot.Messages = append(hot.Messages, message)
		}
	}
	return hot, nil
}

func loadAllHotResourceMailboxes(workspacePath string) ([]resourceMailbox, error) {
	resourceIDs, err := listHotResourceMailboxResourceIDs(workspacePath)
	if err != nil {
		return nil, err
	}
	result := make([]resourceMailbox, 0, len(resourceIDs))
	for _, resourceID := range resourceIDs {
		store, loadErr := loadResourceMailboxStoreForRead(workspacePath, resourceID)
		if loadErr != nil {
			return nil, loadErr
		}
		// A predicate change or a crash after committing mailbox documents can
		// leave cold messages behind a valid hot marker. Compact such a store
		// once while it is already in the bounded hot set; subsequent polls do
		// not scan or rewrite it again.
		if resourceMailboxStoreNeedsCompaction(store) {
			if _, compactErr := mutateResourceMailboxStoreForResource(workspacePath, resourceID, func(*resourceMailboxStore) error { return nil }); compactErr != nil {
				return nil, compactErr
			}
			store, loadErr = loadResourceMailboxStoreForRead(workspacePath, resourceID)
			if loadErr != nil {
				return nil, loadErr
			}
		}
		hot := resourceMailbox{Version: resourceMailboxVersion, NextSequence: store.Mailbox.NextSequence, Messages: []resourceMailboxMessage{}}
		for _, message := range store.Mailbox.Messages {
			if !message.receipt {
				hot.Messages = append(hot.Messages, message)
			}
		}
		if len(hot.Messages) > 0 {
			result = append(result, hot)
		}
	}
	return result, nil
}

func mutateResourceMailboxStoreForResource(workspacePath, resourceID string, mutate func(*resourceMailboxStore) error) (resourceMailboxStore, error) {
	resourceID = normalizedResourceID(resourceID)
	lock := resourceMailboxLock(workspacePath, resourceID)
	lock.Lock()
	defer lock.Unlock()
	store, err := loadResourceMailboxStoreInternal(workspacePath, resourceID)
	if err != nil {
		return resourceMailboxStore{}, err
	}
	before := cloneResourceMailboxStore(store)
	if err := mutate(&store); err != nil {
		return resourceMailboxStore{}, err
	}
	if store.Mailbox.NextSequence < before.Mailbox.NextSequence {
		store.Mailbox.NextSequence = before.Mailbox.NextSequence
	}
	if err := persistResourceMailboxStore(workspacePath, store, before); err != nil {
		return resourceMailboxStore{}, err
	}
	return store, nil
}

func mutateResourceMailboxForResource(workspacePath, resourceID string, mutate func(*resourceMailbox) error) (resourceMailbox, error) {
	store, err := mutateResourceMailboxStoreForResource(workspacePath, resourceID, func(store *resourceMailboxStore) error {
		return mutate(&store.Mailbox)
	})
	if err != nil {
		return resourceMailbox{}, err
	}
	return store.Mailbox, nil
}

// mutateResourceMailbox is a compatibility boundary for older in-process
// callers. New production paths use mutateResourceMailboxForResource so a
// mutation does not scan or rewrite unrelated resources.
func mutateResourceMailbox(workspacePath string, mutate func(*resourceMailbox) error) (resourceMailbox, error) {
	resourceMailboxAggregateMu.Lock()
	defer resourceMailboxAggregateMu.Unlock()
	before, err := loadResourceMailbox(workspacePath)
	if err != nil {
		return resourceMailbox{}, err
	}
	if err := mutate(&before); err != nil {
		return resourceMailbox{}, err
	}
	byResource := make(map[string][]resourceMailboxMessage)
	for _, message := range before.Messages {
		resourceID := normalizedResourceID(message.ResourceID)
		byResource[resourceID] = append(byResource[resourceID], message)
	}
	resourceIDs := make([]string, 0, len(byResource))
	for resourceID := range byResource {
		resourceIDs = append(resourceIDs, resourceID)
	}
	sort.Strings(resourceIDs)
	for _, resourceID := range resourceIDs {
		resourceID := resourceID
		messages := append([]resourceMailboxMessage(nil), byResource[resourceID]...)
		if _, err := mutateResourceMailboxForResource(workspacePath, resourceID, func(mailbox *resourceMailbox) error {
			mailbox.Messages = messages
			mailbox.NextSequence = before.NextSequence
			return nil
		}); err != nil {
			return resourceMailbox{}, err
		}
	}
	return before, nil
}

func mailboxNotificationOperationFromGenerated(source resourceMailboxMessage, generated resourceMailboxMessage) resourceMailboxNotificationOp {
	receipt := source.Notification
	operation := resourceMailboxNotificationOp{
		ID: generated.ID, Type: generated.Type, SourceMessageID: source.ID, SourceMessageIDs: []string{source.ID},
		SourceResourceID: normalizedResourceID(source.ResourceID), SourceWorkspaceInstanceID: strings.TrimSpace(generated.SenderWorkspaceInstanceID),
		GeneratedMessageID: generated.ID, GeneratedText: generated.Text, GeneratedSender: generated.Sender,
		GeneratedCausation: generated.Causation, GeneratedRequestedMode: generated.RequestedMode, GeneratedActualMode: generated.ActualMode,
		GeneratedModeFrozen: generated.ModeFrozen, GeneratedDowngradeReason: generated.DowngradeReason,
		Status: resourceNotificationWaiting, UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if receipt != nil {
		operation.TargetWorkspaceInstanceID = receipt.TargetWorkspaceInstanceID
		operation.TargetResourceID = receipt.TargetResourceID
		operation.Status = receipt.Status
		operation.AcceptedAt = receipt.AcceptedAt
		operation.DeliveryStatus = receipt.DeliveryStatus
		operation.DeliveredAt = receipt.DeliveredAt
		operation.TerminalAt = receipt.TerminalAt
		operation.LastError = receipt.LastError
		operation.LastErrorCode = receipt.LastErrorCode
	}
	if operation.SourceWorkspaceInstanceID == "" {
		operation.SourceWorkspaceInstanceID = strings.TrimSpace(source.SenderWorkspaceInstanceID)
	}
	return operation
}

func appendUniqueMailboxOperation(operations []resourceMailboxNotificationOp, operation resourceMailboxNotificationOp) []resourceMailboxNotificationOp {
	normalizeMailboxOperationSources(&operation)
	for index := range operations {
		if operations[index].ID != operation.ID {
			continue
		}
		current := operations[index]
		currentIDs := mailboxOperationSourceIDs(current)
		for _, sourceID := range mailboxOperationSourceIDs(operation) {
			seen := false
			for _, currentID := range currentIDs {
				if currentID == sourceID {
					seen = true
					break
				}
			}
			if !seen {
				currentIDs = append(currentIDs, sourceID)
			}
		}
		current.SourceMessageIDs = currentIDs
		if len(currentIDs) > 0 {
			current.SourceMessageID = currentIDs[0]
		}
		if operation.GeneratedText != "" {
			current.GeneratedText = operation.GeneratedText
		}
		if operation.GeneratedMessageID != "" {
			current.GeneratedMessageID = operation.GeneratedMessageID
		}
		if operation.GeneratedSender != nil {
			current.GeneratedSender = operation.GeneratedSender
		}
		if operation.GeneratedCausation != nil {
			current.GeneratedCausation = operation.GeneratedCausation
		}
		if operation.GeneratedRequestedMode != "" {
			current.GeneratedRequestedMode = operation.GeneratedRequestedMode
		}
		if operation.GeneratedActualMode != "" && !current.GeneratedModeFrozen {
			current.GeneratedActualMode = operation.GeneratedActualMode
		}
		if operation.GeneratedModeFrozen {
			current.GeneratedModeFrozen = true
		}
		if operation.GeneratedDowngradeReason != "" {
			current.GeneratedDowngradeReason = operation.GeneratedDowngradeReason
		}
		if operation.Status != "" {
			current.Status = operation.Status
		}
		if operation.TargetWorkspaceInstanceID != "" {
			current.TargetWorkspaceInstanceID = operation.TargetWorkspaceInstanceID
		}
		if operation.TargetResourceID != "" {
			current.TargetResourceID = operation.TargetResourceID
		}
		current.UpdatedAt = operation.UpdatedAt
		operations[index] = current
		return operations
	}
	return append(operations, cloneMailboxOperation(operation))
}

func readResourceMailboxLocator(workspacePath, messageID string) (resourceMailboxLocator, bool, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return resourceMailboxLocator{}, false, nil
	}
	var locator resourceMailboxLocator
	found, err := readResourceMailboxJSON(resourceMailboxLocatorPath(workspacePath, messageID), &locator)
	if err != nil {
		// The index is rebuildable; a torn or stale index must not hide a
		// canonical message from the bounded resource-store fallback.
		return resourceMailboxLocator{}, false, nil
	}
	if !found {
		return resourceMailboxLocator{}, false, nil
	}
	if locator.Version != resourceMailboxStoreVersion || locator.MessageID != messageID {
		return resourceMailboxLocator{}, false, nil
	}
	return locator, true, nil
}

func mailboxMessageByID(workspacePath, messageID string) (resourceMailboxMessage, bool, error) {
	messageID = strings.TrimSpace(messageID)
	if locator, found, err := readResourceMailboxLocator(workspacePath, messageID); err != nil {
		return resourceMailboxMessage{}, false, err
	} else if found {
		if locator.State == resourceMailboxExpiredState {
			return resourceMailboxMessage{}, false, &resourceAPIError{Code: "message_receipt_expired", Message: fmt.Sprintf("message receipt expired: %s", messageID)}
		}
		mailbox, loadErr := loadResourceMailboxForResource(workspacePath, locator.ResourceID)
		if loadErr != nil {
			return resourceMailboxMessage{}, false, loadErr
		}
		for _, message := range mailbox.Messages {
			if message.ID == messageID {
				return cloneMailboxMessage(message), true, nil
			}
		}
		// A locator can lag a just-committed resource document. Continue with
		// the bounded fallback so a concurrent compaction cannot look missing.
	}

	// Recovery fallback is bounded by the number of resource stores and each
	// store's fixed receipt window. It is not a scan of old delivered mailbox entries.
	resourceIDs, err := listResourceMailboxResourceIDs(workspacePath)
	if err != nil {
		return resourceMailboxMessage{}, false, err
	}
	for _, resourceID := range resourceIDs {
		store, loadErr := loadResourceMailboxStoreForRead(workspacePath, resourceID)
		if loadErr != nil {
			return resourceMailboxMessage{}, false, loadErr
		}
		for _, message := range store.Mailbox.Messages {
			if message.ID == messageID {
				locator := resourceMailboxLocator{Version: resourceMailboxStoreVersion, MessageID: message.ID, ResourceID: resourceID, ResourceKey: resourceMailboxKey(mailboxInstanceID(workspacePath), resourceID), State: "receipt", UpdatedAt: time.Now().Format(time.RFC3339Nano)}
				if !message.receipt {
					locator.State = "hot"
				}
				_ = writeResourceMailboxIndexJSON(resourceMailboxLocatorPath(workspacePath, message.ID), locator)
				return cloneMailboxMessage(message), true, nil
			}
		}
		for _, expired := range store.Receipts.Expired {
			if expired.ID == messageID {
				return resourceMailboxMessage{}, false, &resourceAPIError{Code: "message_receipt_expired", Message: fmt.Sprintf("message receipt expired: %s", messageID)}
			}
		}
	}
	return resourceMailboxMessage{}, false, nil
}

func updateMailboxMessage(workspacePath, messageID string, mutate func(*resourceMailboxMessage)) (resourceMailboxMessage, error) {
	message, found, err := mailboxMessageByID(workspacePath, messageID)
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	if !found {
		return resourceMailboxMessage{}, fmt.Errorf("mailbox message not found: %s", messageID)
	}
	resourceID := normalizedResourceID(message.ResourceID)
	var updated resourceMailboxMessage
	_, err = mutateResourceMailboxForResource(workspacePath, resourceID, func(mailbox *resourceMailbox) error {
		for index := range mailbox.Messages {
			if mailbox.Messages[index].ID != messageID {
				continue
			}
			mutate(&mailbox.Messages[index])
			mailbox.Messages[index].UpdatedAt = time.Now().Format(time.RFC3339Nano)
			updated = cloneMailboxMessage(mailbox.Messages[index])
			return nil
		}
		return fmt.Errorf("mailbox message not found: %s", messageID)
	})
	if err != nil {
		return resourceMailboxMessage{}, err
	}
	return updated, nil
}

func resourceMailboxOperationGeneratedMessage(operation resourceMailboxNotificationOp) resourceMailboxMessage {
	requestedMode := operation.GeneratedRequestedMode
	if requestedMode == "" {
		requestedMode = resourceMessageModeSteer
	}
	actualMode := operation.GeneratedActualMode
	if actualMode == "" {
		actualMode = requestedMode
	}
	return resourceMailboxMessage{
		ID: operation.GeneratedMessageID, ResourceID: operation.TargetResourceID, Text: operation.GeneratedText,
		Role: "system", Sender: operation.GeneratedSender, SenderWorkspaceInstanceID: operation.SourceWorkspaceInstanceID,
		SubscribeResult: false, ResultSubscriptionStatus: resourceResultSubscriptionDisabled,
		Type: operation.Type, Causation: operation.GeneratedCausation,
		RequestedMode: requestedMode, ActualMode: actualMode, ModeFrozen: operation.GeneratedModeFrozen, DowngradeReason: operation.GeneratedDowngradeReason,
		Status: resourceMessageQueued, AcceptedAt: operation.AcceptedAt, UpdatedAt: operation.UpdatedAt,
	}
}

func mailboxNotificationOperation(workspacePath, sourceMessageID string) (resourceMailboxNotificationOp, bool, error) {
	message, found, err := mailboxMessageByID(workspacePath, sourceMessageID)
	if err != nil || !found {
		return resourceMailboxNotificationOp{}, found, err
	}
	resourceID := normalizedResourceID(message.ResourceID)
	store, err := loadResourceMailboxStoreForRead(workspacePath, resourceID)
	if err != nil {
		return resourceMailboxNotificationOp{}, false, err
	}
	for _, operation := range store.Outbox.Operations {
		if mailboxOperationHasSource(operation, sourceMessageID) {
			return cloneMailboxOperation(operation), true, nil
		}
	}
	return resourceMailboxNotificationOp{}, false, nil
}

func upsertMailboxNotificationOperation(workspacePath string, sourceMessageID string, operation resourceMailboxNotificationOp) error {
	return upsertMailboxNotificationOperationForSources(workspacePath, []string{sourceMessageID}, operation)
}

func upsertMailboxNotificationOperationForSources(workspacePath string, sourceMessageIDs []string, operation resourceMailboxNotificationOp) error {
	ids := make([]string, 0, len(sourceMessageIDs)+len(operation.SourceMessageIDs)+1)
	seen := make(map[string]bool)
	appendID := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		ids = append(ids, value)
	}
	for _, sourceID := range sourceMessageIDs {
		appendID(sourceID)
	}
	for _, sourceID := range mailboxOperationSourceIDs(operation) {
		appendID(sourceID)
	}
	if len(ids) == 0 {
		return errors.New("source mailbox message is required")
	}
	sourceID := ids[0]
	source, found, err := mailboxMessageByID(workspacePath, sourceID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("source mailbox message not found: %s", sourceID)
	}
	operation.SourceMessageID = sourceID
	operation.SourceMessageIDs = ids
	operation.SourceResourceID = normalizedResourceID(source.ResourceID)
	if operation.ID == "" && source.Notification != nil {
		operation.ID = source.Notification.ID
	}
	if operation.Status == "" && source.Notification != nil {
		operation.Status = source.Notification.Status
	}
	if operation.UpdatedAt == "" {
		operation.UpdatedAt = time.Now().Format(time.RFC3339Nano)
	}
	_, err = mutateResourceMailboxStoreForResource(workspacePath, source.ResourceID, func(store *resourceMailboxStore) error {
		foundSources := make(map[string]bool, len(ids))
		for index := range store.Mailbox.Messages {
			message := &store.Mailbox.Messages[index]
			if !seen[message.ID] {
				continue
			}
			if normalizedResourceID(message.ResourceID) != operation.SourceResourceID {
				return fmt.Errorf("source mailbox message belongs to another resource: %s", message.ID)
			}
			foundSources[message.ID] = true
			if message.Notification == nil && operation.ID != "" {
				message.Notification = &resourceNotificationReceipt{ID: operation.ID, Type: operation.Type, Status: operation.Status, TargetWorkspaceInstanceID: operation.TargetWorkspaceInstanceID, TargetResourceID: operation.TargetResourceID, CreatedAt: operation.UpdatedAt, UpdatedAt: operation.UpdatedAt}
			}
			if operation.Type == resourceMessageTypeTurnResult && message.ResultSubscriptionStatus != resourceResultSubscriptionComplete {
				message.ResultSubscriptionStatus = resourceResultSubscriptionPending
				message.ResultOperationID = operation.ID
			}
		}
		for _, id := range ids {
			if !foundSources[id] {
				return fmt.Errorf("source mailbox message not found: %s", id)
			}
		}
		store.Outbox.Operations = appendUniqueMailboxOperation(store.Outbox.Operations, operation)
		return nil
	})
	return err
}

func updateMailboxNotificationOperation(workspacePath, sourceMessageID string, mutate func(*resourceMailboxNotificationOp)) error {
	source, found, err := mailboxMessageByID(workspacePath, sourceMessageID)
	if err != nil || !found {
		return err
	}
	lock := resourceMailboxLock(workspacePath, source.ResourceID)
	lock.Lock()
	defer lock.Unlock()
	store, err := loadResourceMailboxStoreInternal(workspacePath, source.ResourceID)
	if err != nil {
		return err
	}
	before := cloneResourceMailboxStore(store)
	for index := range store.Outbox.Operations {
		if mailboxOperationHasSource(store.Outbox.Operations[index], sourceMessageID) {
			mutate(&store.Outbox.Operations[index])
			store.Outbox.Operations[index].UpdatedAt = time.Now().Format(time.RFC3339Nano)
		}
	}
	return persistResourceMailboxStore(workspacePath, store, before)
}

func removeMailboxNotificationOperation(workspacePath, sourceMessageID string) error {
	source, found, err := mailboxMessageByID(workspacePath, sourceMessageID)
	if err != nil || !found {
		return err
	}
	lock := resourceMailboxLock(workspacePath, source.ResourceID)
	lock.Lock()
	defer lock.Unlock()
	store, err := loadResourceMailboxStoreInternal(workspacePath, source.ResourceID)
	if err != nil {
		return err
	}
	before := cloneResourceMailboxStore(store)
	kept := store.Outbox.Operations[:0]
	for _, operation := range store.Outbox.Operations {
		if !mailboxOperationHasSource(operation, sourceMessageID) {
			kept = append(kept, operation)
		}
	}
	store.Outbox.Operations = kept
	return persistResourceMailboxStore(workspacePath, store, before)
}

func pendingResourceMailboxNotificationOperations(workspacePath string) ([]resourceMailboxNotificationOp, error) {
	resourceIDs, err := listHotResourceMailboxResourceIDs(workspacePath)
	if err != nil {
		return nil, err
	}
	operations := make([]resourceMailboxNotificationOp, 0)
	for _, resourceID := range resourceIDs {
		store, loadErr := loadResourceMailboxStoreForRead(workspacePath, resourceID)
		if loadErr != nil {
			return nil, loadErr
		}
		for _, operation := range store.Outbox.Operations {
			if operation.Status != resourceNotificationDelivered && operation.Status != resourceNotificationTerminal {
				operations = append(operations, cloneMailboxOperation(operation))
			}
		}
	}
	sort.SliceStable(operations, func(i, j int) bool {
		if operations[i].UpdatedAt != operations[j].UpdatedAt {
			return operations[i].UpdatedAt < operations[j].UpdatedAt
		}
		return operations[i].ID < operations[j].ID
	})
	return operations, nil
}
