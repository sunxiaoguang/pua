package pua

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/schedulerapi"
)

const (
	schedulerAddUsage    = "usage: pua scheduler add --description=<text> --condition=<text> --target=<resource> [--guard=<text>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]"
	schedulerShowUsage   = "usage: pua scheduler show --id=<schedule> [--server=<url>]"
	schedulerUpdateUsage = "usage: pua scheduler update --id=<schedule> --revision=<n> [--description=<text>] [--condition=<text>] [--guard=<text>] [--target=<resource>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]"
	schedulerRemoveUsage = "usage: pua scheduler remove --id=<schedule> [--server=<url>]"
)

type schedulerChangePayload struct {
	Operation        app.ScheduleChangeOperation `json:"operation"`
	ID               string                      `json:"id,omitempty"`
	ExpectedRevision schedulerapi.Revision       `json:"expectedRevision,omitempty"`
	Description      *string                     `json:"description,omitempty"`
	Condition        *string                     `json:"condition,omitempty"`
	Guard            *string                     `json:"guard,omitempty"`
	Target           *string                     `json:"target,omitempty"`
	Trigger          *app.ScheduleTrigger        `json:"trigger,omitempty"`
}

type schedulerCommandKind uint8

const (
	schedulerCommandList schedulerCommandKind = iota
	schedulerCommandShow
	schedulerCommandChange
)

type schedulerCommand struct {
	kind       schedulerCommandKind
	serverURL  string
	jsonOutput bool
	id         string
	change     schedulerChangePayload
}

func runScheduler(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printSchedulerHelp()
		return nil
	}
	command, err := parseSchedulerCommand(args)
	if err != nil {
		return err
	}
	client, _, err := newResourceServerClient(command.serverURL)
	if err != nil {
		return err
	}
	basePath := fmt.Sprintf("/api/workspaces/%s/scheduler", url.PathEscape(client.workspaceID))

	switch command.kind {
	case schedulerCommandList:
		snapshot, err := schedulerSnapshot(client, basePath)
		if err != nil {
			return err
		}
		if command.jsonOutput {
			return printJSON(snapshot)
		}
		for _, schedule := range snapshot.Schedules {
			nextRun, lastOutcome := schedule.NextRunAt, schedule.LastOutcome
			if nextRun == "" {
				nextRun = "-"
			}
			if lastOutcome == "" {
				lastOutcome = "-"
			}
			fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", schedule.ID, schedule.Revision, schedule.EffectiveState, scheduleTriggerSummary(schedule.Trigger), nextRun, lastOutcome, schedule.Description, schedule.Target)
		}
		return nil
	case schedulerCommandShow:
		snapshot, err := schedulerSnapshot(client, basePath)
		if err != nil {
			return err
		}
		for _, schedule := range snapshot.Schedules {
			if schedule.ID == command.id {
				return printJSON(schedule)
			}
		}
		return fmt.Errorf("schedule not found: %s", command.id)
	case schedulerCommandChange:
		return schedulerChangeRequest(client, basePath, command.change)
	default:
		return errors.New("invalid parsed scheduler command")
	}
}

func parseSchedulerCommand(args []string) (schedulerCommand, error) {
	if len(args) == 0 {
		return schedulerCommand{}, errors.New("scheduler requires a subcommand")
	}
	subcommand := args[0]
	switch subcommand {
	case "list", "show", "add", "update", "pause", "resume", "remove":
	default:
		return schedulerCommand{}, fmt.Errorf("unknown scheduler subcommand %q", subcommand)
	}

	usage := "usage: pua scheduler " + subcommand + " [--server=<url>]"
	if specific, ok := map[string]string{
		"add": schedulerAddUsage, "update": schedulerUpdateUsage,
		"show": schedulerShowUsage, "remove": schedulerRemoveUsage,
	}[subcommand]; ok {
		usage = specific
	}
	remaining, serverURL, err := splitServerArg(args[1:], usage)
	if err != nil {
		return schedulerCommand{}, err
	}
	command := schedulerCommand{serverURL: serverURL}
	switch subcommand {
	case "list":
		command.kind = schedulerCommandList
		for _, arg := range remaining {
			if arg != "--json" || command.jsonOutput {
				return schedulerCommand{}, errors.New("usage: pua scheduler list [--json] [--server=<url>]")
			}
			command.jsonOutput = true
		}
	case "show":
		values, parseErr := parseSchedulerOptions(remaining, map[string]bool{"id": true})
		if parseErr != nil || values["id"] == "" {
			return schedulerCommand{}, errors.New(schedulerShowUsage)
		}
		command.kind, command.id = schedulerCommandShow, values["id"]
	case "add":
		command.kind = schedulerCommandChange
		command.change, err = parseSchedulerAddPayload(remaining)
	case "update":
		command.kind = schedulerCommandChange
		command.change, err = parseSchedulerUpdatePayload(remaining)
	default:
		command.kind = schedulerCommandChange
		command.change, err = parseSchedulerStatePayload(subcommand, remaining)
	}
	if err != nil {
		return schedulerCommand{}, err
	}
	if command.serverURL != "" {
		command.serverURL, err = normalizePUAServerURL(command.serverURL)
		if err != nil {
			return schedulerCommand{}, err
		}
	}
	return command, nil
}

func parseSchedulerAddPayload(args []string) (schedulerChangePayload, error) {
	values, err := parseSchedulerOptions(args, schedulerMutationOptions(false))
	if err != nil || values["description"] == "" || values["condition"] == "" || values["target"] == "" {
		return schedulerChangePayload{}, errors.New(schedulerAddUsage)
	}
	trigger, present, err := schedulerTriggerFromOptions(values)
	if err != nil || !present {
		if err != nil {
			return schedulerChangePayload{}, err
		}
		return schedulerChangePayload{}, errors.New(schedulerAddUsage)
	}
	if err := app.ValidateScheduleTarget(values["target"]); err != nil {
		return schedulerChangePayload{}, err
	}
	description, condition, target := values["description"], values["condition"], values["target"]
	payload := schedulerChangePayload{Operation: app.ScheduleChangeCreate, Description: &description, Condition: &condition, Target: &target, Trigger: trigger}
	if guard, ok := values["guard"]; ok {
		payload.Guard = &guard
	}
	return payload, nil
}

func parseSchedulerStatePayload(subcommand string, args []string) (schedulerChangePayload, error) {
	values, err := parseSchedulerOptions(args, map[string]bool{"id": true})
	if err != nil || values["id"] == "" {
		return schedulerChangePayload{}, errors.New("usage: pua scheduler " + subcommand + " --id=<schedule> [--server=<url>]")
	}
	operation, err := app.ParseScheduleChangeOperation(subcommand)
	if err != nil || operation == app.ScheduleChangeCreate || operation == app.ScheduleChangeUpdate {
		return schedulerChangePayload{}, fmt.Errorf("unknown scheduler subcommand %q", subcommand)
	}
	return schedulerChangePayload{Operation: operation, ID: values["id"]}, nil
}

func parseSchedulerUpdatePayload(args []string) (schedulerChangePayload, error) {
	values, err := parseSchedulerOptions(args, schedulerMutationOptions(true))
	if err != nil || values["id"] == "" || values["revision"] == "" {
		return schedulerChangePayload{}, errors.New(schedulerUpdateUsage)
	}
	revision, err := schedulerapi.ParseRevision(values["revision"])
	if err != nil {
		return schedulerChangePayload{}, errors.New(schedulerUpdateUsage)
	}
	trigger, triggerPresent, err := schedulerTriggerFromOptions(values)
	if err != nil {
		return schedulerChangePayload{}, err
	}
	if !triggerPresent {
		return schedulerChangePayload{}, errors.New(schedulerUpdateUsage)
	}
	if target, ok := values["target"]; ok {
		if err := app.ValidateScheduleTarget(target); err != nil {
			return schedulerChangePayload{}, err
		}
	}
	payload := schedulerChangePayload{Operation: app.ScheduleChangeUpdate, ID: values["id"], ExpectedRevision: revision, Trigger: trigger}
	for name, target := range map[string]**string{"description": &payload.Description, "condition": &payload.Condition, "guard": &payload.Guard, "target": &payload.Target} {
		if value, ok := values[name]; ok {
			copy := value
			*target = &copy
		}
	}
	return payload, nil
}

func schedulerSnapshot(client *resourceServerClient, path string) (schedulerapi.Snapshot, error) {
	var snapshot schedulerapi.Snapshot
	err := client.request(context.Background(), http.MethodGet, path, nil, &snapshot)
	return snapshot, err
}

func schedulerChangeRequest(client *resourceServerClient, path string, payload schedulerChangePayload) error {
	var schedule schedulerapi.Schedule
	if err := client.request(context.Background(), http.MethodPost, path+"/changes", payload, &schedule); err != nil {
		return err
	}
	return printJSON(schedule)
}

func schedulerMutationOptions(update bool) map[string]bool {
	result := map[string]bool{
		"description": true, "condition": true, "guard": true, "target": true,
		"at": true, "every": true, "anchor": true, "cron": true, "timezone": true,
	}
	if update {
		result["id"], result["revision"] = true, true
	}
	return result
}

func schedulerTriggerFromOptions(values map[string]string) (*app.ScheduleTrigger, bool, error) {
	at, hasAt := values["at"]
	everyValue, hasEvery := values["every"]
	anchor, hasAnchor := values["anchor"]
	expression, hasCron := values["cron"]
	timeZone, hasTimeZone := values["timezone"]
	forms := 0
	if hasAt {
		forms++
	}
	if hasEvery || hasAnchor {
		forms++
	}
	if hasCron || hasTimeZone {
		forms++
	}
	if forms == 0 {
		return nil, false, nil
	}
	if forms != 1 {
		return nil, true, errors.New("exactly one trigger form is required")
	}
	var trigger app.ScheduleTrigger
	switch {
	case hasAt:
		trigger = app.ScheduleTrigger{Type: app.ScheduleTriggerAt, At: at}
	case hasEvery || hasAnchor:
		if !hasEvery || !hasAnchor {
			return nil, true, errors.New("--every and --anchor are required together")
		}
		duration, err := time.ParseDuration(everyValue)
		if err != nil || duration%time.Second != 0 {
			return nil, true, errors.New("--every must be a whole-second duration such as 5m")
		}
		trigger = app.ScheduleTrigger{Type: app.ScheduleTriggerInterval, EverySeconds: int64(duration / time.Second), AnchorAt: anchor}
	case hasCron || hasTimeZone:
		if !hasCron || !hasTimeZone {
			return nil, true, errors.New("--cron and --timezone are required together")
		}
		trigger = app.ScheduleTrigger{Type: app.ScheduleTriggerCron, Cron: expression, TimeZone: timeZone}
	}
	if err := app.ValidateScheduleTrigger(trigger); err != nil {
		return nil, true, err
	}
	return &trigger, true, nil
}

func scheduleTriggerSummary(trigger *app.ScheduleTrigger) string {
	if trigger == nil {
		return "needs compilation"
	}
	switch trigger.Type {
	case app.ScheduleTriggerAt:
		return "at " + trigger.At
	case app.ScheduleTriggerInterval:
		return fmt.Sprintf("every %ds from %s", trigger.EverySeconds, trigger.AnchorAt)
	case app.ScheduleTriggerCron:
		return trigger.Cron + " [" + trigger.TimeZone + "]"
	default:
		return trigger.Type
	}
}

func parseSchedulerOptions(args []string, allowed map[string]bool) (map[string]string, error) {
	values := make(map[string]string)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			return nil, fmt.Errorf("unexpected argument %q", arg)
		}
		nameValue := strings.SplitN(strings.TrimPrefix(arg, "--"), "=", 2)
		name := nameValue[0]
		if !allowed[name] {
			return nil, fmt.Errorf("unknown option --%s", name)
		}
		if _, exists := values[name]; exists {
			return nil, fmt.Errorf("duplicate option --%s", name)
		}
		value := ""
		explicitEmpty := len(nameValue) == 2 && nameValue[1] == ""
		if len(nameValue) == 2 {
			value = nameValue[1]
		} else if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			index++
			value = args[index]
		}
		value = strings.TrimSpace(value)
		if value == "" && !(name == "guard" && explicitEmpty) {
			return nil, fmt.Errorf("option --%s requires a value", name)
		}
		values[name] = value
	}
	return values, nil
}
