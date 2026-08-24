package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/generation"
)

func TestCurrentGenerationRecordByIDUsesResourceScopedLookup(t *testing.T) {
	_, workspace, _ := newRuntimeTestManager(t, "http://127.0.0.1:1")
	target := idleTestGeneration(workspace, "project1.task1", "gen-target-current", "ses-target-current", time.Now())
	unrelated := idleTestGeneration(workspace, "project1", "gen-unrelated-current", "ses-unrelated-current", time.Now())
	if err := saveGenerationRecord(workspace.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := saveGenerationRecord(workspace.Path, unrelated); err != nil {
		t.Fatal(err)
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	unrelatedKey, err := generation.ResourceKey(runtimeConfig.InstanceID, unrelated.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	unrelatedCurrent := filepath.Join(workspace.Path, ".pua", "runtime", "resources", unrelatedKey, "current.json")
	if err := os.WriteFile(unrelatedCurrent, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	current, found, err := currentGenerationRecordByID(workspace.Path, target.ResourceID, target.GenerationID)
	if err != nil || !found || current.GenerationID != target.GenerationID {
		t.Fatalf("resource-scoped current lookup failed: current=%#v found=%v err=%v", current, found, err)
	}
	if _, found, err := currentGenerationRecordByID(workspace.Path, target.ResourceID, unrelated.GenerationID); err != nil || found {
		t.Fatalf("resource-scoped current lookup accepted another generation: found=%v err=%v", found, err)
	}
}

func acceptTestResourceMessage(t *testing.T, manager *agentManager, workspace serveWorkspace, resourceID, text, mode string, sender *agentHubMessageSender) resourceMailboxMessage {
	t.Helper()
	message, err := manager.acceptResourceMessage(context.Background(), workspace, resourceID, resourceMessageRequest{
		Text: text, Mode: mode, Role: "agent", Sender: sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func TestPublicSessionStateKeepsWaitingOutOfTaskState(t *testing.T) {
	tests := []struct {
		name         string
		archived     bool
		unavailable  string
		generation   *resourceGenerationStatus
		session      *resourceSessionStatus
		runtimeError string
		want         string
	}{
		{name: "idle", want: "idle"},
		{name: "working generation", generation: &resourceGenerationStatus{Status: "starting"}, want: "working"},
		{name: "working turn", session: &resourceSessionStatus{State: "running"}, want: "working"},
		{name: "approval", session: &resourceSessionStatus{State: "waiting_approval"}, want: "attention_required"},
		{name: "configuration", unavailable: "missing route", want: "unavailable"},
		{name: "runtime", runtimeError: "unreachable", want: "unavailable"},
		{name: "archived wins", archived: true, unavailable: "missing route", want: "archived"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := publicSessionState(test.archived, test.unavailable, test.generation, test.session, test.runtimeError); got != test.want {
				t.Fatalf("public state = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResourceMailboxIgnoresLegacyFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentRoot(root), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(agentRoot(root), "mailbox.json")
	legacyData := []byte(`{"version":1,"messages":[{"id":"legacy-message"}]}`)
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resourceMailboxResourcesRoot(root), 0o700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(resourceMailboxResourcesRoot(root), ".mailbox-migration.json")
	if err := os.WriteFile(markerPath, []byte(`not valid json`), 0o600); err != nil {
		t.Fatal(err)
	}
	message, err := acceptMailboxMessage(root, "workspace", resourceMessageRequest{Text: "current"})
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailbox(root)
	if err != nil || len(mailbox.Messages) != 1 || mailbox.Messages[0].ID != message.ID || mailbox.Messages[0].Text != "current" {
		t.Fatalf("current mailbox = %#v, %v", mailbox, err)
	}
	if data, readErr := os.ReadFile(legacyPath); readErr != nil || !bytes.Equal(data, legacyData) {
		t.Fatalf("legacy mailbox changed = %q, %v", data, readErr)
	}
	if data, readErr := os.ReadFile(markerPath); readErr != nil || string(data) != "not valid json" {
		t.Fatalf("legacy marker changed = %q, %v", data, readErr)
	}
}

func TestResourceMailboxEmptyWorkspaceHasNoStores(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	mailbox, err := loadResourceMailbox(root)
	if err != nil || mailbox.Version != resourceMailboxVersion || mailbox.NextSequence != 0 || len(mailbox.Messages) != 0 {
		t.Fatalf("empty mailbox = %#v, %v", mailbox, err)
	}
	resourceIDs, err := listResourceMailboxResourceIDs(root)
	if err != nil || len(resourceIDs) != 0 {
		t.Fatalf("empty resource stores = %#v, %v", resourceIDs, err)
	}
}

func TestResultSubscriptionDefaultsAndSystemMessages(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	sender := &agentHubMessageSender{ID: "project1.task2", Name: "Sender"}

	defaulted, err := acceptMailboxMessage(root, "workspace", resourceMessageRequest{
		Text: "default subscription", Mode: resourceMessageModeEnqueue, Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !defaulted.SubscribeResult || defaulted.ResultSubscriptionStatus != "" || !defaulted.subscribeResultPresent {
		t.Fatalf("omitted subscribeResult = %#v", defaulted)
	}
	bindMailboxResultSubscription(&defaulted, "turn-1")
	if defaulted.ResultSubscriptionStatus != resourceResultSubscriptionPending {
		t.Fatalf("delivered default subscription = %#v", defaulted)
	}

	steered, err := acceptMailboxMessage(root, "workspace", resourceMessageRequest{
		Text: "steer without result subscription", Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	bindMailboxResultSubscription(&steered, "turn-1")
	if !steered.SubscribeResult || steered.ResultSubscriptionStatus != resourceResultSubscriptionNone {
		t.Fatalf("delivered steer subscription = %#v", steered)
	}

	downgraded := steered
	downgraded.ActualMode = resourceMessageModeEnqueue
	bindMailboxResultSubscription(&downgraded, "turn-2")
	if downgraded.ResultSubscriptionStatus != resourceResultSubscriptionPending {
		t.Fatalf("steer downgraded to enqueue subscription = %#v", downgraded)
	}

	interrupted := steered
	interrupted.ActualMode = resourceMessageModeInterrupt
	bindMailboxResultSubscription(&interrupted, "turn-3")
	if interrupted.ResultSubscriptionStatus != resourceResultSubscriptionPending {
		t.Fatalf("interrupt opener subscription = %#v", interrupted)
	}

	disabled := false
	explicitlyDisabled, err := acceptMailboxMessage(root, "workspace", resourceMessageRequest{
		Text: "disabled subscription", Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1", SubscribeResult: &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if explicitlyDisabled.SubscribeResult || explicitlyDisabled.ResultSubscriptionStatus != resourceResultSubscriptionDisabled {
		t.Fatalf("explicit false subscription = %#v", explicitlyDisabled)
	}
	bindMailboxResultSubscription(&explicitlyDisabled, "turn-2")
	if explicitlyDisabled.ResultSubscriptionStatus != resourceResultSubscriptionDisabled {
		t.Fatalf("bound explicit false subscription = %#v", explicitlyDisabled)
	}

	trueValue := true
	system, err := acceptMailboxMessage(root, "workspace", resourceMessageRequest{
		Text: "system message", Role: "system", SubscribeResult: &trueValue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if system.SubscribeResult || system.ResultSubscriptionStatus != resourceResultSubscriptionDisabled {
		t.Fatalf("system subscription = %#v", system)
	}

	var omitted resourceMailboxMessage
	if err := json.Unmarshal([]byte(`{"id":"old","role":"agent","status":"delivered"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if !omitted.SubscribeResult || omitted.ResultSubscriptionStatus != "" {
		t.Fatalf("old completed message subscription = %#v", omitted)
	}
	normalizeStoredMailboxMessage(&omitted)
	if omitted.SubscribeResult || omitted.ResultSubscriptionStatus != resourceResultSubscriptionNone {
		t.Fatalf("stored old completed message subscription = %#v", omitted)
	}
	var explicitFalse resourceMailboxMessage
	if err := json.Unmarshal([]byte(`{"id":"new","role":"agent","status":"queued","subscribeResult":false}`), &explicitFalse); err != nil {
		t.Fatal(err)
	}
	if explicitFalse.SubscribeResult || !explicitFalse.subscribeResultPresent {
		t.Fatalf("explicit false JSON subscription = %#v", explicitFalse)
	}

	legacySteer := resourceMailboxMessage{
		Status: resourceMessageDelivered, ActualMode: resourceMessageModeSteer, SubscribeResult: true,
		ResultSubscriptionStatus: resourceResultSubscriptionPending, subscribeResultPresent: true,
	}
	normalizeStoredMailboxMessage(&legacySteer)
	if !legacySteer.SubscribeResult || legacySteer.ResultSubscriptionStatus != resourceResultSubscriptionNone || legacySteer.ResultOperationID != "" {
		t.Fatalf("stored steer subscription = %#v", legacySteer)
	}
}

func TestProviderMessageContextSurvivesReceiptAndClone(t *testing.T) {
	original := resourceMailboxMessage{
		ID: "msg-context", Role: "agent", Sender: &agentHubMessageSender{ID: "project1.task2"},
		ProviderContext: &providerMessageContext{
			Language: "zh-CN", TurnID: "turn-1", OpenerRole: "user",
			OpenerSender: &agentHubMessageSender{Name: "disksing"}, OpenerResponse: providerResponseProgressFinal,
		},
	}
	cloned := cloneMailboxMessage(original)
	cloned.ProviderContext.OpenerSender.Name = "changed"
	if original.ProviderContext.OpenerSender.Name != "disksing" {
		t.Fatal("mailbox clone shared provider context sender")
	}
	roundTrip := mailboxMessageFromReceipt(receiptFromMailboxMessage(original))
	if !reflect.DeepEqual(roundTrip.ProviderContext, original.ProviderContext) {
		t.Fatalf("receipt provider context = %#v, want %#v", roundTrip.ProviderContext, original.ProviderContext)
	}
	roundTrip.ProviderContext.OpenerSender.Name = "changed again"
	if original.ProviderContext.OpenerSender.Name != "disksing" {
		t.Fatal("receipt round trip shared provider context sender")
	}
}

func TestResourceMailboxReceiptRetentionReturnsStableExpiredError(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	previousCount, previousWindow := resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow
	previousExpiredCount, previousExpiredWindow := resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow
	defer func() {
		resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = previousCount, previousWindow
		resourceMailboxExpiredRetentionCount, resourceMailboxExpiredRetentionWindow = previousExpiredCount, previousExpiredWindow
	}()
	resourceMailboxReceiptRetentionCount = 2
	resourceMailboxReceiptRetentionWindow = 0
	resourceMailboxExpiredRetentionCount = 8
	resourceMailboxExpiredRetentionWindow = 24 * time.Hour
	now := time.Now()
	_, err := mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		for index := 0; index < 3; index++ {
			stamp := now.Add(time.Duration(index-3) * time.Minute).Format(time.RFC3339Nano)
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: fmt.Sprintf("msg-retention-%d", index), Sequence: mailbox.NextSequence, ResourceID: "workspace",
				Text: fmt.Sprintf("body-%d", index), Role: "user", RequestedMode: resourceMessageModeEnqueue,
				ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered, AcceptedAt: stamp,
				UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadResourceMailboxStoreForRead(root, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Receipts.Receipts) != 2 || len(store.Receipts.Expired) != 1 {
		t.Fatalf("receipt retention counts = receipts=%d expired=%d", len(store.Receipts.Receipts), len(store.Receipts.Expired))
	}
	latest, found, err := mailboxMessageByID(root, "msg-retention-2")
	if err != nil || !found || !latest.receipt || latest.Text != "" {
		t.Fatalf("retained receipt = %#v, found=%v err=%v", latest, found, err)
	}
	_, found, err = mailboxMessageByID(root, "msg-retention-0")
	var apiErr *resourceAPIError
	if found || !errors.As(err, &apiErr) || apiErr.Code != "message_receipt_expired" || resourceErrorStatus(err) != http.StatusGone {
		t.Fatalf("expired receipt lookup = found=%v err=%v", found, err)
	}
	manager := newNotificationTestManager(t, "http://127.0.0.1:1", []serveWorkspace{{ID: "workspace", Path: root}})
	recorder := httptest.NewRecorder()
	manager.handleResourceMessage(recorder, httptest.NewRequest(http.MethodGet, "/messages/msg-retention-0", nil), "workspace", "msg-retention-0")
	if recorder.Code != http.StatusGone || !strings.Contains(recorder.Body.String(), `"code":"message_receipt_expired"`) {
		t.Fatalf("expired receipt HTTP response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestResourceMailboxHotStoreIsBoundedIndependentlyOfReceiptHistory(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	previousCount, previousWindow := resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow
	defer func() {
		resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = previousCount, previousWindow
	}()
	resourceMailboxReceiptRetentionCount = 32
	resourceMailboxReceiptRetentionWindow = 0
	const completed = 64
	_, err := mutateResourceMailboxForResource(root, "workspace", func(mailbox *resourceMailbox) error {
		for index := 0; index < completed; index++ {
			stamp := "2026-08-13T00:00:00Z"
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: "msg-scale-" + fmt.Sprint(index), Sequence: mailbox.NextSequence, ResourceID: "workspace",
				Text: "completed body that must leave hot storage", Role: "user", RequestedMode: resourceMessageModeEnqueue,
				ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered, AcceptedAt: stamp,
				UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
			})
		}
		mailbox.NextSequence++
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: "msg-scale-pending", Sequence: mailbox.NextSequence, ResourceID: "workspace", Text: "pending body",
			Role: "user", RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue,
			Status: resourceMessageQueued, AcceptedAt: time.Now().Format(time.RFC3339Nano), UpdatedAt: time.Now().Format(time.RFC3339Nano),
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	hot, err := loadHotResourceMailbox(root, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	store, err := loadResourceMailboxStoreForRead(root, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(hot.Messages) != 1 || hot.Messages[0].ID != "msg-scale-pending" || len(store.Receipts.Receipts) != 32 {
		t.Fatalf("hot/receipt scale bounds = hot=%d %#v receipts=%d", len(hot.Messages), hot.Messages, len(store.Receipts.Receipts))
	}
}

func TestResourceMailboxModesAndPriority(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	sender := &agentHubMessageSender{ID: "project1.task2", Name: "project1.task2"}

	first := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "first", resourceMessageModeSteer, sender)
	if first.Status != resourceMessageDelivered || first.RequestedMode != resourceMessageModeSteer ||
		first.ActualMode != resourceMessageModeEnqueue || first.DowngradeReason != resourceMessageReasonNoActiveTurn {
		t.Fatalf("first message did not open a normal Turn: %#v", first)
	}
	record, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatalf("current generation missing: found=%v err=%v", found, err)
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	if session.LaunchEnvironment["PUA_WORKSPACE_ROOT"] != workspace.Path ||
		session.LaunchEnvironment["PUA_WORKSPACE_INSTANCE_ID"] != runtimeConfig.InstanceID ||
		session.LaunchEnvironment["PUA_RESOURCE_ID"] != "project1.task1" {
		fake.mu.Unlock()
		t.Fatalf("resource generation provenance environment = %#v", session.LaunchEnvironment)
	}
	session.InputCapabilities.Steer = true
	session.CurrentTurnID = "turn-first"
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	enqueued := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "queued", resourceMessageModeEnqueue, sender)
	if enqueued.Status != resourceMessageQueued || enqueued.ActualMode != resourceMessageModeEnqueue {
		t.Fatalf("enqueue entered the active Turn: %#v", enqueued)
	}
	steered := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "urgent steer", resourceMessageModeSteer, sender)
	if steered.Status != resourceMessageDelivered || steered.ActualMode != resourceMessageModeSteer {
		t.Fatalf("steer did not bypass the queued enqueue: %#v", steered)
	}
	promoted, err := manager.promoteWaitingMessage(context.Background(), workspace, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ID != enqueued.ID || promoted.Status != resourceMessageDelivered || promoted.ActualMode != resourceMessageModeSteer ||
		promoted.RequestedMode != resourceMessageModeEnqueue || promoted.PromotedAt == "" {
		t.Fatalf("waiting message was not promoted in place: %#v", promoted)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageSteers) != 3 || fake.messageSteers[0] || !fake.messageSteers[1] || !fake.messageSteers[2] {
		t.Fatalf("AgentHub delivery order/modes mismatch: %#v", fake.messageSteers)
	}
	if fake.messageSenders[0] == nil || fake.messageSenders[0].ID != "project1.task2" || fake.messageRoles[0] != "agent" {
		t.Fatalf("agent provenance was not preserved: roles=%#v senders=%#v", fake.messageRoles, fake.messageSenders)
	}
}

func TestResourceMailboxProviderEnvelopeUsesTurnOpenerAndFreezesLanguage(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	opener, err := manager.acceptResourceMessage(context.Background(), workspace, "project1.task1", resourceMessageRequest{
		Text: "start here", Mode: resourceMessageModeSteer, Role: "user", Sender: &agentHubMessageSender{Name: "disksing"},
	})
	if err != nil || opener.Status != resourceMessageDelivered || opener.ActualMode != resourceMessageModeEnqueue {
		t.Fatalf("user opener = %#v, err=%v", opener, err)
	}
	fake.mu.Lock()
	openerWire := fake.messageInputs[opener.ID]
	fake.mu.Unlock()
	if openerWire.Text != "Message from user \"disksing\" [sender receives: progress + final reply]:\nstart here" {
		t.Fatalf("opener provider text = %q", openerWire.Text)
	}

	record, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatalf("current generation missing: found=%v err=%v", found, err)
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := puaWorkspace.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "turn-opener"
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.failNextMessage = true
	fake.mu.Unlock()

	inserted, err := manager.acceptResourceMessage(context.Background(), workspace, "project1.task1", resourceMessageRequest{
		Text: "check this", Mode: resourceMessageModeSteer, Role: "agent",
		Sender: &agentHubMessageSender{ID: "project1.task347", Name: "project1.task347"}, SenderWorkspaceInstanceID: runtimeConfig.InstanceID,
	})
	if err != nil || inserted.Status != resourceMessageDelivering || inserted.ProviderContext == nil ||
		inserted.ProviderContext.Language != "en" || inserted.ProviderContext.TurnID != "turn-opener" ||
		inserted.ProviderContext.OpenerRole != "user" || inserted.ProviderContext.OpenerSender == nil ||
		inserted.ProviderContext.OpenerSender.Name != "disksing" || inserted.ProviderContext.OpenerResponse != providerResponseProgressFinal {
		t.Fatalf("inserted message context = %#v, err=%v", inserted, err)
	}
	wantInserted := "Inserted message from agent \"project1.task347\" (Reply via `pua message send --to=project1.task347 '<reply>'`. Current conversation: user \"disksing\" [sender receives: progress + final reply]):\ncheck this"
	fake.mu.Lock()
	insertedWire := fake.messageInputs[inserted.ID]
	fake.mu.Unlock()
	if insertedWire.Text != wantInserted {
		t.Fatalf("inserted provider text = %q, want %q", insertedWire.Text, wantInserted)
	}

	if err := puaWorkspace.Migrate("zh-CN"); err != nil {
		t.Fatal(err)
	}
	if err := manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task1")
	}); err != nil {
		t.Fatal(err)
	}
	recovered, found, err := mailboxMessageByID(workspace.Path, inserted.ID)
	if err != nil || !found || recovered.Status != resourceMessageDelivered || recovered.ProviderContext == nil || recovered.ProviderContext.Language != "en" {
		t.Fatalf("recovered inserted message = %#v, found=%v err=%v", recovered, found, err)
	}
	fake.mu.Lock()
	retriedWire := fake.messageInputs[inserted.ID]
	fake.mu.Unlock()
	if retriedWire.Text != wantInserted {
		t.Fatalf("language change rewrote retry text = %q, want %q", retriedWire.Text, wantInserted)
	}
}

func TestResourceMailboxSteerDowngradeAndInterrupt(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	sender := &agentHubMessageSender{ID: "project1.task2"}

	_ = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "first", resourceMessageModeSteer, sender)
	record, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatal("generation missing")
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.CurrentTurnID = "turn-current"
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	downgraded := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "wait", resourceMessageModeSteer, sender)
	if downgraded.Status != resourceMessageQueued || downgraded.ActualMode != resourceMessageModeEnqueue ||
		downgraded.DowngradeReason != resourceMessageReasonSteerUnsupported {
		t.Fatalf("unsupported steer did not durably downgrade: %#v", downgraded)
	}
	interrupted := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "replace current turn", resourceMessageModeInterrupt, sender)
	if interrupted.Status != resourceMessageDelivered || interrupted.RequestedMode != resourceMessageModeInterrupt ||
		interrupted.ActualMode != resourceMessageModeInterrupt || interrupted.InterruptTurnID != "turn-current" {
		t.Fatalf("interrupt did not wait for termination and open a new Turn: %#v", interrupted)
	}
	fake.mu.Lock()
	if len(fake.actions) == 0 || fake.actions[0] != "interrupt" {
		fake.mu.Unlock()
		t.Fatalf("interrupt action missing: %#v", fake.actions)
	}
	fake.mu.Unlock()
	fake.mu.Lock()
	session = fake.sessions[record.AgentHubSessionID]
	session.State = "ready"
	session.CurrentTurnID = ""
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	err = manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task1")
	})
	if err != nil {
		t.Fatal(err)
	}
	downgraded, found, err = mailboxMessageByID(workspace.Path, downgraded.ID)
	if err != nil || !found || downgraded.Status != resourceMessageDelivered ||
		downgraded.ActualMode != resourceMessageModeEnqueue || downgraded.DowngradeReason != resourceMessageReasonSteerUnsupported {
		t.Fatalf("unsupported steer decision drifted during recovery: found=%v err=%v message=%#v", found, err, downgraded)
	}
}

func TestResourceMailboxInterruptRetiresReplacingGeneration(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	_ = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "first", resourceMessageModeSteer, nil)
	oldGeneration, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatal("generation missing")
	}
	runtime := manager.runtimeByID(oldGeneration.ID)
	if runtime == nil {
		t.Fatal("runtime missing")
	}
	if _, err := runtime.mutateGeneration(func(record *generationRecord) { record.ReplacementPending = true }); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	session := fake.sessions[oldGeneration.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "turn-before-replacement"
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "interrupt and replace", resourceMessageModeInterrupt, nil)
	if message.Status != resourceMessageQueued || message.InterruptAt == "" {
		t.Fatalf("interrupt did not stop at the replacement boundary: %#v", message)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		message, found, err = mailboxMessageByID(workspace.Path, message.ID)
		current, currentFound, currentErr := currentResourceGeneration(workspace.Path, "project1.task1")
		if err == nil && found && currentErr == nil && currentFound && current.Generation > oldGeneration.Generation && message.Status == resourceMessageDelivered {
			// The generation and mailbox files become observable before the
			// retirement goroutine publishes its final notice. Join that bounded
			// critical section so TempDir cleanup cannot race the last disk write.
			if err := manager.withResourceController(context.Background(), workspace, "project1.task1", func() error { return nil }); err != nil {
				t.Fatalf("join resource controller: %v", err)
			}
			if message.GenerationID == oldGeneration.GenerationID {
				t.Fatalf("interrupt message was delivered to the retired generation: %#v", message)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if len(fake.actions) == 0 || fake.actions[0] != "interrupt" {
				t.Fatalf("old Turn was not interrupted first: %#v", fake.actions)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("replacement did not receive the interrupt message: found=%v err=%v message=%#v", found, err, message)
}

func TestResourceMailboxArchiveTerminatesPendingMessages(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	_ = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "first", resourceMessageModeSteer, nil)
	pending := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "later", resourceMessageModeEnqueue, nil)
	unknown := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "outcome unknown", resourceMessageModeEnqueue, nil)
	unknown, err := updateMailboxMessage(workspace.Path, unknown.ID, func(message *resourceMailboxMessage) {
		message.Status = resourceMessageDelivering
		message.AttemptCount = 1
	})
	if err != nil {
		t.Fatal(err)
	}
	puaWorkspace, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := puaWorkspace.ArchiveResource("project1.task1"); err != nil {
		t.Fatal(err)
	}
	err = manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task1")
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, found, err := mailboxMessageByID(workspace.Path, pending.ID)
	if err != nil || !found || pending.Status != resourceMessageUndeliverable || pending.DowngradeReason != resourceMessageReasonResourceArchived || pending.LastErrorCode != "resource_archived" {
		t.Fatalf("archived mailbox item mismatch: found=%v err=%v message=%#v", found, err, pending)
	}
	unknown, found, err = mailboxMessageByID(workspace.Path, unknown.ID)
	if err != nil || !found || unknown.Status != resourceMessageDeliveryUnknown || unknown.LastErrorCode != "resource_archived" {
		t.Fatalf("archived unknown-outcome item mismatch: found=%v err=%v message=%#v", found, err, unknown)
	}
	_, err = acceptTestResourceMessageWithError(manager, workspace, "project1.task1")
	var apiErr *resourceAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "resource_archived" {
		t.Fatalf("archived resource accepted a new message: %v", err)
	}
}

func TestResourceMailboxArchiveRaceEitherRejectsOrTerminatesAcceptedMessage(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	configData, _ := json.Marshal(agentHubServeConfig{
		Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace},
		AgentHubEndpoint: hub.URL, AgentHubInstanceID: "pua-runtime-test",
	})
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var sent resourceMailboxMessage
	var sendErr error
	var archiveCode int
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		sent, sendErr = manager.acceptResourceMessage(context.Background(), workspace, "project1.task1", resourceMessageRequest{Text: "race", Mode: resourceMessageModeEnqueue})
	}()
	go func() {
		defer wait.Done()
		<-start
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/archive", strings.NewReader(`{"resourceId":"project1.task1"}`))
		manager.server.archiveResource(recorder, request, workspace.ID)
		archiveCode = recorder.Code
	}()
	close(start)
	wait.Wait()
	if archiveCode != http.StatusOK {
		t.Fatalf("archive race did not converge: status=%d", archiveCode)
	}
	if sendErr != nil {
		var apiErr *resourceAPIError
		if !errors.As(sendErr, &apiErr) || apiErr.Code != "resource_archived" {
			t.Fatalf("race rejected send with the wrong error: %v", sendErr)
		}
		return
	}
	terminal, found, err := mailboxMessageByID(workspace.Path, sent.ID)
	if err != nil || !found || terminal.Status != resourceMessageUndeliverable || terminal.LastErrorCode != "resource_archived" {
		t.Fatalf("accepted race message lacked an archive terminal: found=%v err=%v message=%#v", found, err, terminal)
	}
}

func TestResourceMailboxPersistsBindingAndTemporaryDeliveryErrors(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	manager, workspace, configPath := newRuntimeTestManager(t, hub.URL)
	configData, _ := json.Marshal(agentHubServeConfig{
		Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace},
		AgentHubEndpoint: hub.URL, AgentHubInstanceID: "pua-runtime-test",
	})
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	message := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "wait for binding", resourceMessageModeSteer, nil)
	if message.Status != resourceMessageQueued || message.LastErrorCode != "binding_unavailable" {
		t.Fatalf("binding failure was not queryable: %#v", message)
	}

	// Restore a valid binding but make AgentHub unreachable. The same accepted
	// message remains queued and reports a distinct retryable delivery class.
	hub.Close()
	configData, _ = json.Marshal(agentHubServeConfig{
		Version: agentHubConfigVersion, Workspaces: []serveWorkspace{workspace},
		AgentHubEndpoint: hub.URL, AgentHubInstanceID: "pua-runtime-test",
		AgentProfiles: []agentHubProfileRoute{{Key: "default", AgentName: "fake-agent"}},
	})
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	err := manager.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		return manager.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task1")
	})
	if err == nil {
		t.Fatal("unreachable AgentHub unexpectedly reconciled")
	}
	message, found, loadErr := mailboxMessageByID(workspace.Path, message.ID)
	if loadErr != nil || !found || message.Status != resourceMessageQueued || message.LastErrorCode != "temporarily_undeliverable" {
		t.Fatalf("temporary delivery failure was not retained: found=%v err=%v message=%#v", found, loadErr, message)
	}
}

func TestResourceMailboxSeparatesTargetsAndRejectsPersistenceFailure(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	projectMessage := acceptTestResourceMessage(t, manager, workspace, "project1", "project", resourceMessageModeSteer, nil)
	taskMessage := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "task", resourceMessageModeSteer, nil)
	mailbox, err := loadResourceMailbox(workspace.Path)
	if err != nil {
		t.Fatal(err)
	}
	projectCounts, _, _ := mailboxCounts(mailbox, "project1")
	taskCounts, _, _ := mailboxCounts(mailbox, "project1.task1")
	if projectMessage.ID == taskMessage.ID || projectCounts.Delivered != 1 || taskCounts.Delivered != 1 {
		t.Fatalf("resource mailbox targets were not independent: project=%#v task=%#v", projectCounts, taskCounts)
	}

	brokenWorkspace := t.TempDir()
	if _, err := app.Initialize(brokenWorkspace, "en"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resourceMailboxResourcesRoot(brokenWorkspace), 0o700); err != nil {
		t.Fatal(err)
	}
	directory, _, _, err := resourceMailboxDirectory(brokenWorkspace, "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("blocks directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acceptMailboxMessage(brokenWorkspace, "workspace", resourceMessageRequest{Text: "must not accept"}); err == nil {
		t.Fatal("mailbox persistence failure was reported as accepted")
	}
}

func TestResourceServerAPIStatusSendAndMessageQuery(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	sendRequest := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/resources/project1.task1/messages", strings.NewReader(`{"text":"coordinate","mode":"steer","role":"agent","sender":{"id":"project1.task2","name":"Task two"}}`))
	sendRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(sendRecorder, sendRequest)
	if sendRecorder.Code != http.StatusAccepted {
		t.Fatalf("resource send failed: %d %s", sendRecorder.Code, sendRecorder.Body.String())
	}
	var sent resourceMessageResponse
	if err := json.Unmarshal(sendRecorder.Body.Bytes(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.MessageID == "" || sent.ResourceID != "project1.task1" || sent.RequestedMode != resourceMessageModeSteer ||
		sent.ActualMode != resourceMessageModeSteer || sent.Status != "waiting" || !strings.Contains(sent.Reference, sent.MessageID) {
		t.Fatalf("send response mismatch: %#v", sent)
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+workspace.ID+"/resources/project1.task1/status", nil)
	statusRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(statusRecorder, statusRequest)
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("resource status failed: %d %s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status resourceStatusResponse
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Exists || !status.AcceptsMessages || status.Archived || status.SessionState != "working" || status.Generation == nil || status.Session == nil || status.Messages.Delivered != 1 {
		t.Fatalf("resource status mismatch: %#v", status)
	}

	messageRequest := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+workspace.ID+"/messages/"+sent.MessageID, nil)
	messageRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(messageRecorder, messageRequest)
	if messageRecorder.Code != http.StatusOK {
		t.Fatalf("message query failed: %d %s", messageRecorder.Code, messageRecorder.Body.String())
	}
	var queried resourceMessageResponse
	if err := json.Unmarshal(messageRecorder.Body.Bytes(), &queried); err != nil {
		t.Fatal(err)
	}
	if queried.MessageID != sent.MessageID || queried.Status != resourceMessageDelivered {
		t.Fatalf("message query mismatch: %#v", queried)
	}
}

func TestUserMessageSendMarksResourceRead(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	now := "2026-08-13T00:00:00Z"
	record := generationRecord{
		ID: "gen-auto-read", WorkspaceID: workspace.ID, ResourceID: "project1.task1",
		Generation: 1, GenerationID: "gen-auto-read", AgentHubSessionID: "session-auto-read",
		Status: "idle", TurnNumber: 2, Title: "Auto read", Cwd: workspace.Path, CreatedAt: now, UpdatedAt: now, CompletionAt: now,
	}
	if err := rewriteTestGenerationRecords(workspace.Path, []generationRecord{record}); err != nil {
		t.Fatal(err)
	}
	tree, err := manager.server.treeAt(context.Background(), workspace.Path, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Projects) != 1 || len(tree.Projects[0].Children) != 1 || tree.Projects[0].Children[0].UnreadCount != 2 {
		t.Fatalf("task should start with two unread Turns: %#v", tree.Projects)
	}

	// Agent-to-agent messages leave the user's read cursor untouched.
	agentRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(agentRecorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/resources/project1.task1/messages", strings.NewReader(`{"text":"coordinate","role":"agent","sender":{"id":"project1.task2","name":"Task two"}}`)))
	if agentRecorder.Code != http.StatusAccepted {
		t.Fatalf("agent send failed: %d %s", agentRecorder.Code, agentRecorder.Body.String())
	}
	state, err := manager.server.resourceUserStateForResource(workspace.Path, "project1.task1", app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if state.ReadTurnNumber != nil {
		t.Fatalf("agent message moved the read cursor: %#v", state)
	}

	// A user message marks the resource read through the latest completed Turn.
	userRecorder := httptest.NewRecorder()
	userRequest := httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/resources/project1.task1/messages", strings.NewReader(`{"text":"hello","role":"user","sender":{"name":"User"}}`))
	userRequest.Header.Set(workspaceUserHeader, app.LegacyDefaultUserName)
	manager.server.handleWorkspace(userRecorder, userRequest)
	if userRecorder.Code != http.StatusAccepted {
		t.Fatalf("user send failed: %d %s", userRecorder.Code, userRecorder.Body.String())
	}
	state, err = manager.server.resourceUserStateForResource(workspace.Path, "project1.task1", app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if state.ReadTurnNumber == nil || *state.ReadTurnNumber != 2 {
		t.Fatalf("user message did not mark the resource read: %#v", state)
	}
	tree, err = manager.server.treeAt(context.Background(), workspace.Path, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if task := tree.Projects[0].Children[0]; task.UnreadCount != 0 || len(tree.Activity.Unread) != 0 {
		t.Fatalf("resource stayed unread after user send: task=%#v unread=%#v", task, tree.Activity.Unread)
	}

	// The Turn the sends triggered becomes the next unread one once done.
	manager.waitBackground()
	current, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatalf("triggered generation missing: found=%v err=%v", found, err)
	}
	current.TurnNumber = 3
	current.Status = "idle"
	current.CurrentTurnID = ""
	current.CompletionAt = "2026-08-13T00:10:00Z"
	current.UpdatedAt = "2026-08-13T00:10:00Z"
	if err := saveGenerationRecord(workspace.Path, current); err != nil {
		t.Fatal(err)
	}
	tree, err = manager.server.treeAt(context.Background(), workspace.Path, app.LegacyDefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if task := tree.Projects[0].Children[0]; task.UnreadCount != 1 {
		t.Fatalf("next completed Turn did not become unread: %#v", task)
	}
}

func TestResourceServerAPIListsAndSteersWaitingMessageInPlace(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	_ = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "start", resourceMessageModeSteer, nil)
	record, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatalf("generation missing: found=%v err=%v", found, err)
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "turn-active"
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	waiting := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "move this now", resourceMessageModeEnqueue, nil)
	statusRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/workspaces/"+workspace.ID+"/resources/project1.task1/status", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status failed: %d %s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var status resourceStatusResponse
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.SessionState != "working" || !status.CanSteerWaiting || status.Messages.Waiting != 1 || len(status.WaitingMessages) != 1 ||
		status.WaitingMessages[0].MessageID != waiting.ID || status.WaitingMessages[0].Text != "move this now" || status.WaitingMessages[0].Status != "waiting" ||
		!status.WaitingMessages[0].CanPromote {
		t.Fatalf("waiting projection mismatch: %#v", status)
	}

	steerRecorder := httptest.NewRecorder()
	manager.server.handleWorkspace(steerRecorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/messages/"+waiting.ID+"/steer", nil))
	if steerRecorder.Code != http.StatusOK {
		t.Fatalf("steer failed: %d %s", steerRecorder.Code, steerRecorder.Body.String())
	}
	var promoted resourceMessageResponse
	if err := json.Unmarshal(steerRecorder.Body.Bytes(), &promoted); err != nil {
		t.Fatal(err)
	}
	if promoted.MessageID != waiting.ID || promoted.RequestedMode != resourceMessageModeEnqueue || promoted.ActualMode != resourceMessageModeSteer ||
		promoted.Status != resourceMessageDelivered || promoted.PromotedAt == "" {
		t.Fatalf("promoted response mismatch: %#v", promoted)
	}
}

func TestResourceServerAPIPromotionPolicySurvivesFixedBaseMessages(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	fake.enforceMessageIDs = true
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	_ = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "start", resourceMessageModeSteer, nil)
	record, found, err := currentResourceGeneration(workspace.Path, "project1.task1")
	if err != nil || !found {
		t.Fatalf("generation missing: found=%v err=%v", found, err)
	}
	fake.mu.Lock()
	session := fake.sessions[record.AgentHubSessionID]
	session.State = "running"
	session.CurrentTurnID = "turn-active"
	session.InputCapabilities.Steer = true
	fake.sessions[session.ID] = session
	fake.mu.Unlock()

	legacyOrdinary := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "legacy ordinary waiting", resourceMessageModeEnqueue, nil)
	legacyOrdinary, err = updateMailboxMessage(workspace.Path, legacyOrdinary.ID, func(message *resourceMailboxMessage) {
		// The fixed base persisted ordinary enqueues as mode-frozen while they
		// waited behind an active Turn. The new field is deliberately omitted.
		message.ModeFrozen = true
		message.NonPromotable = false
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinary := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "new ordinary waiting", resourceMessageModeEnqueue, nil)
	frozen, err := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
		ID: "msg-frozen-occurrence", ResourceID: "project1.task1", Text: "run the one-time occurrence",
		RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
		Type: resourceMessageTypeScheduleOccurrence, SenderWorkspaceInstanceID: "workspace-instance",
		Causation: &resourceMessageCausation{
			Type: resourceMessageTypeScheduleOccurrence, SourceWorkspaceInstanceID: "workspace-instance",
			SourceResourceID: app.SchedulerResourceID, ScheduleID: "schedule-once", ScheduleRevision: 1,
			OccurrenceID: "occurrence-once", ScheduledFor: "2026-08-01T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	generated := []resourceMailboxMessage{frozen}
	for _, fixture := range []struct {
		id, messageType string
	}{
		{id: "msg-fixed-base-migration", messageType: resourceMessageTypeScheduleMigration},
		{id: "msg-fixed-base-generated", messageType: resourceMessageTypeTurnStallRecovery},
	} {
		accepted, acceptErr := acceptGeneratedMailboxMessage(workspace.Path, resourceMailboxMessage{
			ID: fixture.id, ResourceID: "project1.task1", Text: "fixed-base generated message",
			RequestedMode: resourceMessageModeEnqueue, ActualMode: resourceMessageModeEnqueue, ModeFrozen: true,
			Type: fixture.messageType, SenderWorkspaceInstanceID: "workspace-instance",
			Causation: &resourceMessageCausation{
				Type: fixture.messageType, SourceWorkspaceInstanceID: "workspace-instance", SourceResourceID: app.SchedulerResourceID,
			},
		})
		if acceptErr != nil {
			t.Fatal(acceptErr)
		}
		// Simulate the exact fixed-base JSON omission and prove the loader's
		// origin classifier restores the generated-message policy.
		if _, updateErr := updateMailboxMessage(workspace.Path, accepted.ID, func(message *resourceMailboxMessage) {
			message.NonPromotable = false
		}); updateErr != nil {
			t.Fatal(updateErr)
		}
		loaded, found, loadErr := mailboxMessageByID(workspace.Path, accepted.ID)
		if loadErr != nil || !found || !loaded.NonPromotable {
			t.Fatalf("fixed-base generated message did not normalize: found=%v err=%v message=%#v", found, loadErr, loaded)
		}
		generated = append(generated, loaded)
	}
	if legacyOrdinary.Status != resourceMessageQueued || !legacyOrdinary.ModeFrozen || legacyOrdinary.NonPromotable ||
		ordinary.Status != resourceMessageQueued || ordinary.ModeFrozen || ordinary.NonPromotable || frozen.Status != resourceMessageQueued ||
		!frozen.ModeFrozen || !frozen.NonPromotable || frozen.ActualMode != resourceMessageModeEnqueue || frozen.Sequence <= ordinary.Sequence {
		t.Fatalf("waiting setup mismatch: legacy=%#v ordinary=%#v frozen=%#v", legacyOrdinary, ordinary, frozen)
	}

	manager.waitBackground()
	restarted := newAgentManager(manager.server)
	manager.server.agents = restarted
	beforeMailbox, err := loadHotResourceMailbox(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	statusRecorder := httptest.NewRecorder()
	restarted.server.handleWorkspace(statusRecorder, httptest.NewRequest(http.MethodGet,
		"/api/workspaces/"+workspace.ID+"/resources/project1.task1/status", nil))
	var status resourceStatusResponse
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	canPromote := make(map[string]bool, len(status.WaitingMessages))
	for _, waiting := range status.WaitingMessages {
		canPromote[waiting.MessageID] = waiting.CanPromote
	}
	if statusRecorder.Code != http.StatusOK || !status.CanSteerWaiting || !canPromote[legacyOrdinary.ID] || !canPromote[ordinary.ID] {
		t.Fatalf("ordinary promotion projection mismatch: code=%d status=%#v", statusRecorder.Code, status)
	}
	fake.mu.Lock()
	beforeInputs := len(fake.messageIDs)
	beforeSession := fake.sessions[record.AgentHubSessionID]
	beforeSessionReads := fake.getSessionCalls
	fake.mu.Unlock()
	for _, message := range generated {
		if canPromote[message.ID] {
			t.Fatalf("generated message %s was projected as promotable: %#v", message.ID, status)
		}
		rejected := httptest.NewRecorder()
		restarted.server.handleWorkspace(rejected, httptest.NewRequest(http.MethodPost,
			"/api/workspaces/"+workspace.ID+"/messages/"+message.ID+"/steer", nil))
		var rejection map[string]any
		if err := json.Unmarshal(rejected.Body.Bytes(), &rejection); err != nil {
			t.Fatal(err)
		}
		if rejected.Code != http.StatusConflict || rejection["code"] != "message_not_promotable" {
			t.Fatalf("generated promotion response = %d %#v", rejected.Code, rejection)
		}
	}
	afterMailbox, err := loadHotResourceMailbox(workspace.Path, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterMailbox, beforeMailbox) {
		t.Fatalf("rejected promotion mutated mailbox:\nbefore=%#v\nafter=%#v", beforeMailbox, afterMailbox)
	}
	fake.mu.Lock()
	afterInputs := len(fake.messageIDs)
	afterSession := fake.sessions[record.AgentHubSessionID]
	afterSessionReads := fake.getSessionCalls
	fake.mu.Unlock()
	if afterInputs != beforeInputs || afterSessionReads != beforeSessionReads || !reflect.DeepEqual(afterSession, beforeSession) {
		t.Fatalf("rejected promotion contacted the active Turn: inputs=%d/%d reads=%d/%d session=%#v/%#v",
			afterInputs, beforeInputs, afterSessionReads, beforeSessionReads, afterSession, beforeSession)
	}

	for _, message := range []resourceMailboxMessage{legacyOrdinary, ordinary} {
		promoted := httptest.NewRecorder()
		restarted.server.handleWorkspace(promoted, httptest.NewRequest(http.MethodPost,
			"/api/workspaces/"+workspace.ID+"/messages/"+message.ID+"/steer", nil))
		if promoted.Code != http.StatusOK {
			t.Fatalf("ordinary promotion failed: %d %s", promoted.Code, promoted.Body.String())
		}
		var ordinaryResponse resourceMessageResponse
		if err := json.Unmarshal(promoted.Body.Bytes(), &ordinaryResponse); err != nil {
			t.Fatal(err)
		}
		if ordinaryResponse.MessageID != message.ID || ordinaryResponse.ActualMode != resourceMessageModeSteer ||
			ordinaryResponse.Status != resourceMessageDelivered || ordinaryResponse.PromotedAt == "" || ordinaryResponse.CanPromote {
			t.Fatalf("ordinary promotion response mismatch: %#v", ordinaryResponse)
		}
	}
	stillFrozen, found, err := mailboxMessageByID(workspace.Path, frozen.ID)
	if err != nil || !found || stillFrozen.Status != resourceMessageQueued || !stillFrozen.ModeFrozen ||
		stillFrozen.ActualMode != resourceMessageModeEnqueue || stillFrozen.Sequence != frozen.Sequence || stillFrozen.PromotedAt != "" {
		t.Fatalf("ordinary promotion changed frozen occurrence: found=%v err=%v message=%#v", found, err, stillFrozen)
	}

	fake.mu.Lock()
	session = fake.sessions[record.AgentHubSessionID]
	session.State = "ready"
	session.CurrentTurnID = ""
	fake.sessions[session.ID] = session
	fake.mu.Unlock()
	if err := restarted.withResourceController(context.Background(), workspace, "project1.task1", func() error {
		return restarted.reconcileResourceMailboxLocked(context.Background(), workspace, "project1.task1")
	}); err != nil {
		t.Fatal(err)
	}
	delivered, found, err := mailboxMessageByID(workspace.Path, frozen.ID)
	if err != nil || !found || delivered.Status != resourceMessageDelivered || !delivered.ModeFrozen ||
		delivered.ActualMode != resourceMessageModeEnqueue || delivered.Sequence != frozen.Sequence || delivered.PromotedAt != "" {
		t.Fatalf("ready-boundary occurrence delivery mismatch: found=%v err=%v message=%#v", found, err, delivered)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.messageIDs) != beforeInputs+3 || fake.messageIDs[len(fake.messageIDs)-1] != frozen.ID ||
		fake.messageSteers[len(fake.messageSteers)-1] {
		t.Fatalf("delivery modes after promotions = ids=%#v steers=%#v", fake.messageIDs, fake.messageSteers)
	}
}

func TestResourceServerAPISteerUnavailableLeavesWaitingMessageUnchanged(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)

	_ = acceptTestResourceMessage(t, manager, workspace, "project1.task1", "start", resourceMessageModeSteer, nil)
	waiting := acceptTestResourceMessage(t, manager, workspace, "project1.task1", "keep waiting", resourceMessageModeEnqueue, nil)
	recorder := httptest.NewRecorder()
	manager.server.handleWorkspace(recorder, httptest.NewRequest(http.MethodPost, "/api/workspaces/"+workspace.ID+"/messages/"+waiting.ID+"/steer", nil))
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusConflict || response["code"] != "steer_unavailable" {
		t.Fatalf("unexpected unavailable response: status=%d body=%#v", recorder.Code, response)
	}
	unchanged, found, err := mailboxMessageByID(workspace.Path, waiting.ID)
	if err != nil || !found || unchanged.Status != resourceMessageQueued || unchanged.ActualMode != resourceMessageModeEnqueue || unchanged.PromotedAt != "" {
		t.Fatalf("failed steer mutated the waiting item: found=%v err=%v message=%#v", found, err, unchanged)
	}
}

func TestResourceServerAPIStructuredErrors(t *testing.T) {
	fake := newRuntimeFakeAgentHub()
	hub := httptest.NewServer(fake)
	defer hub.Close()
	manager, workspace, _ := newRuntimeTestManager(t, hub.URL)
	tests := []struct {
		name, method, path, body, code string
		status                         int
	}{
		{name: "invalid request", method: http.MethodPost, path: "/api/workspaces/" + workspace.ID + "/resources/project1.task1/messages", body: `{"text":"hello","mode":"later"}`, code: "invalid_request", status: http.StatusBadRequest},
		{name: "missing resource", method: http.MethodPost, path: "/api/workspaces/" + workspace.ID + "/resources/project9.task9/messages", body: `{"text":"hello"}`, code: "resource_not_found", status: http.StatusNotFound},
		{name: "missing message", method: http.MethodGet, path: "/api/workspaces/" + workspace.ID + "/messages/msg-missing", code: "message_not_found", status: http.StatusNotFound},
		{name: "steer missing message", method: http.MethodPost, path: "/api/workspaces/" + workspace.ID + "/messages/msg-missing/steer", code: "message_not_found", status: http.StatusNotFound},
		{name: "workspace not owned", method: http.MethodGet, path: "/api/workspaces/not-owned/resources/workspace/status", code: "workspace_not_owned", status: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			recorder := httptest.NewRecorder()
			manager.server.handleWorkspace(recorder, request)
			var response map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.status || response["code"] != test.code {
				t.Fatalf("structured error mismatch: status=%d response=%#v", recorder.Code, response)
			}
		})
	}
}

func acceptTestResourceMessageWithError(manager *agentManager, workspace serveWorkspace, resourceID string) (resourceMailboxMessage, error) {
	return manager.acceptResourceMessage(context.Background(), workspace, resourceID, resourceMessageRequest{Text: "no", Mode: resourceMessageModeSteer})
}
