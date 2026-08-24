package serve

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/disksing/pua/internal/app"
)

func TestHotMailboxIndexSkipsColdResourceStores(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}

	const coldCount = 96
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for index := 0; index < coldCount; index++ {
		resourceID := fmt.Sprintf("cold-%03d", index)
		_, err := mutateResourceMailboxForResource(root, resourceID, func(mailbox *resourceMailbox) error {
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: fmt.Sprintf("cold-message-%03d", index), Sequence: mailbox.NextSequence, ResourceID: resourceID,
				Text: "completed", Role: "user", RequestedMode: resourceMessageModeEnqueue,
				ActualMode: resourceMessageModeEnqueue, Status: resourceMessageDelivered,
				AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	activeID := "project1.task-hot"
	if _, err := mutateResourceMailboxForResource(root, activeID, func(mailbox *resourceMailbox) error {
		mailbox.NextSequence++
		mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
			ID: "active-message", Sequence: mailbox.NextSequence, ResourceID: activeID,
			Text: "retry me", Role: "user", RequestedMode: resourceMessageModeEnqueue,
			ActualMode: resourceMessageModeEnqueue, Status: resourceMessageQueued,
			AcceptedAt: stamp, UpdatedAt: stamp,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := rebuildResourceMailboxHotIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != activeID {
		t.Fatalf("rebuilt hot resources = %#v, want [%q]", ids, activeID)
	}

	// Once the ready marker exists, a malformed cold hot.json must not be read
	// by the periodic hot-only path. This also makes the scale property
	// observable without depending on OS-specific file-read instrumentation.
	coldDirectory, _, _, err := resourceMailboxDirectory(root, "cold-000")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resourceMailboxHotPath(coldDirectory), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	hot, err := loadAllHotResourceMailboxes(root)
	if err != nil {
		t.Fatalf("hot-only load read a cold store: %v", err)
	}
	if len(hot) != 1 || len(hot[0].Messages) != 1 || hot[0].Messages[0].ID != "active-message" {
		t.Fatalf("hot-only result = %#v", hot)
	}
}

func TestHotMailboxIndexRebuildsAfterMarkerLoss(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	resourceID := "workspace"
	if _, err := acceptMailboxMessage(root, resourceID, resourceMessageRequest{Text: "pending", Mode: resourceMessageModeEnqueue}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildResourceMailboxHotIndex(root); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(resourceMailboxHotIndexRoot(root)); err != nil {
		t.Fatal(err)
	}
	ids, err := listHotResourceMailboxResourceIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != resourceID {
		t.Fatalf("recovered hot resources = %#v, want [%q]", ids, resourceID)
	}
	if _, err := os.Stat(resourceMailboxHotIndexReadyPath(root)); err != nil {
		t.Fatalf("hot index was not rebuilt: %v", err)
	}
}

func TestHotMailboxMarkerLeavesActiveSetAfterRetryAndExitsAfterCompletion(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	message, err := acceptMailboxMessage(root, "workspace", resourceMessageRequest{Text: "retry", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := listHotResourceMailboxResourceIDs(root)
	if err != nil || len(ids) != 1 || ids[0] != "workspace" {
		t.Fatalf("active marker after acceptance = %#v, err=%v", ids, err)
	}
	completedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := updateMailboxMessage(root, message.ID, func(current *resourceMailboxMessage) {
		current.Status = resourceMessageDelivered
		current.DeliveredAt = completedAt
		current.TerminalAt = completedAt
	}); err != nil {
		t.Fatal(err)
	}
	ids, err = listHotResourceMailboxResourceIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("completed mailbox remained active: %#v", ids)
	}
}

func TestHotMailboxMarkerDropsSettledAgentMessages(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	sender := &agentHubMessageSender{ID: "project1.task2", Name: "Sender"}
	tests := []struct {
		name            string
		mode            string
		subscribeResult bool
		subscription    string
		wantHot         bool
	}{
		{name: "disabled enqueue", mode: resourceMessageModeEnqueue, subscription: resourceResultSubscriptionDisabled},
		{name: "disabled steer", mode: resourceMessageModeSteer, subscription: resourceResultSubscriptionDisabled},
		{name: "settled steer", mode: resourceMessageModeSteer, subscribeResult: true, subscription: resourceResultSubscriptionNone},
		{name: "unbound result subscription", mode: resourceMessageModeEnqueue, subscribeResult: true, wantHot: true},
	}
	for index, test := range tests {
		resourceID := fmt.Sprintf("project1.task%d", index+1)
		_, err := mutateResourceMailboxForResource(root, resourceID, func(mailbox *resourceMailbox) error {
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: fmt.Sprintf("message-%d", index), Sequence: mailbox.NextSequence, ResourceID: resourceID,
				Text: "completed", Role: "agent", Sender: sender, SenderWorkspaceInstanceID: "instance-1",
				SubscribeResult: test.subscribeResult, ResultSubscriptionStatus: test.subscription,
				RequestedMode: test.mode, ActualMode: test.mode, Status: resourceMessageDelivered,
				AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
				GenerationID: "generation-1",
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := listHotResourceMailboxResourceIDs(root)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, id := range ids {
			if id == resourceID {
				found = true
				break
			}
		}
		if found != test.wantHot {
			t.Fatalf("%s hot marker = %v, want %v; active=%#v", test.name, found, test.wantHot, ids)
		}
	}
}

func TestHotMailboxCompactsDeliveredLegacySchedulerTicks(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := mutateResourceMailboxForResource(root, app.SchedulerResourceID, func(mailbox *resourceMailbox) error {
		for index := 1; index <= 3; index++ {
			mailbox.NextSequence++
			mailbox.Messages = append(mailbox.Messages, resourceMailboxMessage{
				ID: fmt.Sprintf("tick-%d", index), Sequence: mailbox.NextSequence, ResourceID: app.SchedulerResourceID,
				Type: resourceMessageTypeSchedulerTick, Status: resourceMessageDelivered,
				AcceptedAt: stamp, UpdatedAt: stamp, DeliveredAt: stamp, TerminalAt: stamp,
				GenerationID: fmt.Sprintf("generation-%d", index), TurnID: fmt.Sprintf("turn-%d", index),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	hot, err := loadHotResourceMailbox(root, app.SchedulerResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hot.Messages) != 0 {
		t.Fatalf("delivered legacy Scheduler ticks remained hot: %#v", hot.Messages)
	}
	for _, id := range []string{"tick-1", "tick-2", "tick-3"} {
		message, found, err := mailboxMessageByID(root, id)
		if err != nil || !found || !message.receipt {
			t.Fatalf("historical tick %s was not retained as a receipt: found=%v err=%v message=%#v", id, found, err, message)
		}
	}
	ids, err := listHotResourceMailboxResourceIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("terminal scheduler mailbox remained active: %#v", ids)
	}
}

func TestUnresolvedLegacySchedulerTickSurvivesReceiptCompaction(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	previousCount, previousWindow := resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow
	resourceMailboxReceiptRetentionCount = 1
	resourceMailboxReceiptRetentionWindow = time.Hour
	t.Cleanup(func() {
		resourceMailboxReceiptRetentionCount, resourceMailboxReceiptRetentionWindow = previousCount, previousWindow
	})
	old := time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	_, err := mutateResourceMailboxForResource(root, app.SchedulerResourceID, func(mailbox *resourceMailbox) error {
		mailbox.Messages = append(mailbox.Messages,
			resourceMailboxMessage{
				ID: "tick-unresolved", Sequence: 1, ResourceID: app.SchedulerResourceID,
				Type: resourceMessageTypeSchedulerTick, Status: resourceMessageDelivered,
				AcceptedAt: old, UpdatedAt: old, DeliveredAt: old, TerminalAt: old,
				GenerationID: "generation-unresolved", AgentHubSessionID: "session-unresolved", TurnID: "turn-unresolved",
			},
			resourceMailboxMessage{
				ID: "tick-terminal", Sequence: 2, ResourceID: app.SchedulerResourceID,
				Type: resourceMessageTypeSchedulerTick, Status: resourceMessageDelivered,
				AcceptedAt: old, UpdatedAt: old, DeliveredAt: old, TerminalAt: old, TurnTerminalAt: old,
				GenerationID: "generation-terminal", AgentHubSessionID: "session-terminal", TurnID: "turn-terminal",
			},
		)
		mailbox.NextSequence = 2
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	unresolved, found, err := mailboxMessageByID(root, "tick-unresolved")
	if err != nil || !found || !unresolved.receipt || unresolved.TurnTerminalAt != "" {
		t.Fatalf("unresolved tick receipt = %#v, found=%v err=%v", unresolved, found, err)
	}
	if _, found, err := mailboxMessageByID(root, "tick-terminal"); err == nil || found || !strings.Contains(err.Error(), "receipt expired") {
		t.Fatalf("terminal tick retention = found=%v err=%v", found, err)
	}
}

func TestHotMailboxLoadCompactsLegacySettledMarker(t *testing.T) {
	root := t.TempDir()
	if _, err := app.Initialize(root, "en"); err != nil {
		t.Fatal(err)
	}
	message, err := acceptMailboxMessage(root, "project1.task1", resourceMessageRequest{Text: "pending", Mode: resourceMessageModeEnqueue})
	if err != nil {
		t.Fatal(err)
	}
	directory, _, _, err := resourceMailboxDirectory(root, "project1.task1")
	if err != nil {
		t.Fatal(err)
	}
	var document resourceMailboxHotDocument
	found, err := readResourceMailboxJSON(resourceMailboxHotPath(directory), &document)
	if err != nil || !found || len(document.Messages) != 1 {
		t.Fatalf("load staged hot document: found=%v err=%v document=%#v", found, err, document)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	document.Messages[0].Status = resourceMessageDelivered
	document.Messages[0].SubscribeResult = false
	document.Messages[0].ResultSubscriptionStatus = resourceResultSubscriptionDisabled
	document.Messages[0].DeliveredAt = stamp
	document.Messages[0].TerminalAt = stamp
	document.Messages[0].UpdatedAt = stamp
	if err := writeResourceMailboxJSON(resourceMailboxHotPath(directory), document); err != nil {
		t.Fatal(err)
	}

	hot, err := loadAllHotResourceMailboxes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hot) != 0 {
		t.Fatalf("legacy settled mailbox remained hot: %#v", hot)
	}
	ids, err := listHotResourceMailboxResourceIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("legacy settled marker remained active: %#v", ids)
	}
	settled, found, err := mailboxMessageByID(root, message.ID)
	if err != nil || !found || settled.Status != resourceMessageDelivered {
		t.Fatalf("legacy settled message was not retained as a receipt: found=%v err=%v message=%#v", found, err, settled)
	}
}
