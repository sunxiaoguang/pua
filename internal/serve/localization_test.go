package serve

import (
	"strings"
	"testing"

	"github.com/disksing/pua/internal/localize"
)

func TestAutomaticMessagesRenderInRequestedLanguage(t *testing.T) {
	turn := agentHubTurn{TurnID: "turn-1", Status: "completed", Closed: true}
	delivery := resourceMailboxMessage{
		ID: "message-1", ResourceID: "project1.task1", Status: resourceMessageUndeliverable,
		LastErrorCode: "resource_not_found",
	}
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "scheduler", text: localize.MustRender("zh-CN", "scheduler-migration.md", map[string]any{"MessageID": "migration-1", "Count": 1, "ScheduleIDs": "schedule-1"}), want: "不得猜测 IANA 时区"},
		{name: "task continuation", text: taskStateContinuationText("en"), want: "The Task state is still in_progress"},
		{name: "task exhausted", text: taskStateContinuationExhaustedNote("en"), want: "3 automatic continuation attempts"},
		{name: "task waiting schedule", text: taskWaitingScheduleText("en", "project1.task1"), want: "Scheduler has no schedule targeting project1.task1"},
		{name: "task waiting schedule exhausted", text: taskWaitingScheduleExhaustedNote("en"), want: "3 automatic reminders"},
		{name: "turn result", text: turnResultMessage("zh-CN", "project1.task1", "generation-1", turn, "", nil, false), want: "Turn 结果"},
		{name: "delivery", text: terminalDeliveryMessage("zh-CN", delivery), want: "投递通知"},
		{name: "stall recovery English", text: stallWatchdogRecoveryText("en"), want: "Automatic recovery detected"},
		{name: "stall recovery Chinese", text: stallWatchdogRecoveryText("zh-CN"), want: "自动恢复检测到"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(test.text, test.want) {
				t.Fatalf("localized text missing %q:\n%s", test.want, test.text)
			}
		})
	}
}
