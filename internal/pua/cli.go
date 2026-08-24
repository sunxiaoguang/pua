package pua

import (
	"errors"
	"fmt"
	"strings"

	"github.com/disksing/pua/internal/app"
	"github.com/disksing/pua/internal/buildinfo"
	"github.com/disksing/pua/internal/serve"
)

const (
	projectCreateUsage = "usage: pua project create [--slug <slug>] <description>"
	taskCreateUsage    = "usage: pua task create [<title>] [--project=<project>] [--slug <slug>] [--detail <detail>|--task-markdown <markdown>|--template=<name> [--field <name>=<value>...] [--fields <file>]] [--title <title>] [--profile=<name>|--agent=<name>] [--dry-run] [--json]"
	taskListUsage      = "usage: pua task list [--project=<project>] [--all]"
	taskShowUsage      = "usage: pua task show [--project=<project>] [--task=<task>]"
	taskArchiveUsage   = "usage: pua task archive [--project=<project>] [--task=<task>]"
)

type createResourceOptions struct {
	Slug        string
	Description string
}

type taskListOptions struct {
	ProjectID       string
	IncludeArchived bool
}

func Run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "--version":
		if len(args) != 1 {
			return errors.New("usage: pua --version")
		}
		fmt.Print(buildinfo.Text("pua"))
		return nil
	case "init":
		return runInit(args[1:])
	case "repo":
		return runRepo(args[1:])
	case "project":
		return runProject(args[1:])
	case "task":
		return runTask(args[1:])
	case "scheduler":
		return runScheduler(args[1:])
	case "template":
		return runTemplate(args[1:])
	case "agent":
		return runAgent(args[1:])
	case "user":
		return runUser(args[1:])
	case "message":
		return runMessage(args[1:])
	case "history":
		return runHistory(args[1:])
	case "workspace":
		return runWorkspace(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "serve":
		return serve.Main(args[1:])
	case "help", "-h", "--help":
		return runHelp(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runWorkspace(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printWorkspaceHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("workspace requires a subcommand")
	}
	switch args[0] {
	case "status":
		return runWorkspaceStatus(args[1:])
	case "history":
		return runWorkspaceHistory(args[1:])
	case "tree":
		if len(args) != 2 || args[1] != "--json" {
			return errors.New("usage: pua workspace tree --json")
		}
		return workspaceTreeJSON()
	case "resource":
		id, err := parseWorkspaceResourceArgs(args[1:])
		if err != nil {
			return err
		}
		return workspaceResourceJSON(id)
	case "binding":
		return runWorkspaceBinding(args[1:])
	default:
		return fmt.Errorf("unknown workspace subcommand %q", args[0])
	}
}

func runRepo(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printRepoHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("repo requires a subcommand")
	}
	switch args[0] {
	case "add":
		return repoAdd(args[1:])
	case "list":
		if len(args) != 1 {
			return errors.New("usage: pua repo list")
		}
		return repoList()
	default:
		return fmt.Errorf("unknown repo subcommand %q", args[0])
	}
}

func runProject(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printProjectHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("project requires a subcommand")
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New(projectCreateUsage)
		}
		options, err := parseProjectCreateArgs(args[1:])
		if err != nil {
			return err
		}
		return applicationProjectCreate(options.Description, options.Slug)
	case "list":
		options, err := parseProjectListArgs(args[1:])
		if err != nil {
			return err
		}
		return applicationProjectList(options.IncludeArchived)
	case "show":
		projectID, err := resolveProjectArg(args[1:], "show")
		if err != nil {
			return err
		}
		return applicationShowResource(projectID)
	case "status":
		return runProjectStatus(args[1:])
	case "history":
		return runProjectHistory(args[1:])
	case "binding":
		return runProjectBinding(args[1:])
	case "archive":
		projectID, err := resolveProjectArg(args[1:], "archive")
		if err != nil {
			return err
		}
		return applicationArchiveResource(projectID)
	case "repo":
		return errors.New("projects do not manage repositories or worktrees; place Git worktrees under a Task's worktree/ directory")
	default:
		return fmt.Errorf("unknown project subcommand %q", args[0])
	}
}

func runTask(args []string) error {
	if len(args) > 0 && isHelpCommand(args[0]) {
		printTaskHelp()
		return nil
	}
	if len(args) == 0 {
		return errors.New("task requires a subcommand")
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New(taskCreateUsage)
		}
		options, err := parseTaskCreateArgs(args[1:])
		if err != nil {
			return err
		}
		parentID := options.ParentID
		if parentID == "" {
			var ok bool
			parentID, ok, err = inferCurrentProjectID()
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("could not infer current project; use pua task create --project=<project> <title>")
			}
		}
		workspace, err := openApplicationWorkspace()
		if err != nil {
			return err
		}
		var fields map[string]any
		if options.TemplateName != "" {
			fields, err = templateFieldValues(workspace, parentID, options.TemplateName, options.FieldsFile, options.Fields)
			if err != nil {
				return err
			}
		}
		input := appCreateTaskInput(parentID, options.Title, options.Detail, options.TaskMarkdown, options.TaskMarkdownSet, options.Slug)
		input.AgentBinding = options.Binding
		input.TemplateName, input.TemplateFields = options.TemplateName, fields
		if options.DryRun {
			preview, err := workspace.PreviewTask(input)
			if err != nil {
				return err
			}
			return printJSON(preview)
		}
		return applicationTaskCreate(input)
	case "list":
		options, err := resolveTaskListArgs(args[1:])
		if err != nil {
			return err
		}
		return applicationTaskList(options)
	case "show":
		taskID, err := resolveTaskArg(args[1:], "show")
		if err != nil {
			return err
		}
		return applicationShowResource(taskID)
	case "status":
		return runTaskStatus(args[1:])
	case "state":
		return runTaskState(args[1:])
	case "archive":
		taskID, err := resolveTaskArg(args[1:], "archive")
		if err != nil {
			return err
		}
		return applicationArchiveResource(taskID)
	case "history":
		return runTaskHistory(args[1:])
	case "binding":
		return runTaskBinding(args[1:])
	default:
		return fmt.Errorf("unknown task subcommand %q", args[0])
	}
}

func runMigrate(args []string) error {
	return runWorkspaceMigrate(args)
}

func printUsage() {
	fmt.Print(`pua manages a local AgentWorkspace.

How PUA works:
  All workspace data lives on the filesystem as project/task directories,
  JSON/Markdown files, resource History, artifacts, and task worktrees. Agents may inspect
  other resources, but write only the Workspace files owned by their starting
  resource and its task worktrees. The web service is provided by pua serve.

Usage:
  pua --version
  pua init [--language=<language>]
  pua migrate [--language=<language>]
  pua doctor [--json] [--server=<url>]
  pua repo <command>
  pua project <command>
  pua task <command>
  pua scheduler <command>
  pua template <command>
  pua agent <command>
  pua user <command>
  pua workspace <command>
  pua message <command>
  pua history <command>
  pua serve [--addr=<address>] [--workspace=<path>] [--version]
  pua help [<command>]

Commands:
  pua --version
    Print the build-time branch and sha.

  pua init [--language=<language>]
    Initialize the current directory as a new AgentWorkspace. Fails when run
    from inside an existing workspace. Supported languages: en, zh-CN.

  pua migrate [--language=<language>]
    Refresh pua-managed AGENTS.md blocks and migrate legacy task/resource
    history before removing obsolete files. Pass --language to switch between
    en and zh-CN.

  pua doctor [--json] [--server=<url>]
    Inspect open Workspace data and Agent bindings without changing them.

  pua repo <command>
    Manage repositories known to the workspace. Subcommands: add, list.

  pua project <command>
    Manage projects. Subcommands: create, list, show, archive, status, history,
    binding.

  pua task <command>
    Manage tasks. Subcommands: create, list, show, archive, status, history,
    binding, repo.

  pua scheduler <command>
    Manage native structured schedules through the owning pua serve process.
    Subcommands: list, show, add, update, pause, resume, remove. Add and update
    require --at, --every with --anchor, or --cron with --timezone. Use the Web
    UI or Scheduler Agent to compile natural-language requests.

  pua template <command>
    Manage project-local content templates. Subcommands: list, show, validate,
    render, create.

  pua agent <command>
    Query the AgentHub agent catalog through the owning pua serve process.
    Subcommands: list.

  pua user <command>
    Manage Workspace-local users. Subcommands: list.

  pua workspace <command>
    Query workspace state and settings. Subcommands: status, history, tree,
    resource, binding.

  pua message <command>
    Send and inspect mailbox messages. Subcommands: send, show.

  pua history <command>
    Read resource conversation history. Subcommands: turn show, event show.

  pua serve [--addr=<address>] [--workspace=<path>] [--version]
    Start the PUA web service: Workspace API, AgentHub session orchestration
    and recovery, and the static web UI.

  pua help [<command>]
    Show help for pua or one of its subcommands.

Use "pua help <command>" to see the subcommands of <command>.
`)
}

func runHelp(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}
	if len(args) != 1 {
		return errors.New("usage: pua help [<command>]")
	}
	switch args[0] {
	case "init":
		printInitHelp()
	case "migrate":
		printMigrateHelp()
	case "repo":
		printRepoHelp()
	case "project":
		printProjectHelp()
	case "task":
		printTaskHelp()
	case "scheduler":
		printSchedulerHelp()
	case "template":
		printTemplateHelp()
	case "agent":
		printAgentHelp()
	case "user":
		printUserHelp()
	case "workspace":
		printWorkspaceHelp()
	case "message":
		printMessageHelp()
	case "history":
		printHistoryHelp()
	case "serve":
		serve.PrintHelp()
	default:
		return fmt.Errorf("unknown help topic %q", args[0])
	}
	return nil
}

func isHelpCommand(arg string) bool {
	return arg == "help" || arg == "-h" || arg == "--help"
}

func printInitHelp() {
	fmt.Print(`Usage:
  pua init [--language=<language>]

Commands:
  pua init [--language=<language>]
    Initialize the current directory as a new AgentWorkspace. Fails when run
    from inside an existing workspace. Supported languages: en, zh-CN.
`)
}

func printMigrateHelp() {
	fmt.Print(`Usage:
  pua migrate [--language=<language>]

Commands:
  pua migrate [--language=<language>]
    Refresh pua-managed AGENTS.md blocks and migrate legacy task/resource
    history before removing obsolete files. Pass --language to switch between
    en and zh-CN.
`)
}

func printRepoHelp() {
	fmt.Print(`Usage:
  pua repo add [--bare] <name> <url>
  pua repo list

Commands:
  pua repo add [--bare] <name> <url>
    Clone <url> into repos/<name> as a normal checkout by default. <name> may
    include path segments, for example disksing/pua. Use --bare to clone into
    repos/<name>.git as a bare repository.

  pua repo list
    List repositories known to the workspace.
`)
}

func printProjectHelp() {
	fmt.Print(`Usage:
  pua project create [--slug <slug>] <description>
  pua project list [--all]
  pua project show [--project=<project>]
  pua project archive [--project=<project>]
  pua project binding set [--project=<project>] (--profile=<name>|--agent=<name>) [--server=<url>]
  pua project status [--project=<project>] [--server=<url>]
  pua project history [--project=<project>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]

Commands:
  pua project create [--slug <slug>] <description>
    Create the next top-level project directory, including project.json,
    project.md, artifacts/, templates/, and project-local AGENTS.md. Conversation
    history is created lazily through resource History.
    Use --slug <slug> to append a human-readable suffix to the directory name.
    Creation is local and does not create a generation or send a message.

  pua project list [--all]
    List open projects. Use --all to include archived projects.

  pua project show [--project=<project>]
    Print a project's project.json as formatted JSON. <project> may be a full
    id such as project22 or just a number such as 22. When omitted, PUA uses
    the project containing the current working directory.

  pua project archive [--project=<project>]
    Move a project into workspace archive/. <project> may be a full id such as
    project22 or just a number such as 22. When omitted, PUA uses the project
    containing the current working directory.

  pua project binding set [--project=<project>] (--profile=<name>|--agent=<name>) [--server=<url>]
    Set the Project's explicit Profile or direct Agent binding through the
    owning pua serve process. <project> follows the same rules as pua project
    show. The server validates configured Profile names and converges a
    running generation when the resolved Agent changes.

  pua project status [--project=<project>] [--server=<url>]
    Query the owning pua serve process for the project's public session
    state and generation status, message counts, waiting messages, steer
    capability, and recent delivery error. <project> follows the same rules as
    pua project show.

  pua project history [--project=<project>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
    Read one bounded newest-first page of the project's long-lived conversation
    through the owning pua serve process. Results are grouped into ordered
    generation segments. Explicit gap segments identify missing, unavailable, or
    damaged AgentHub history without hiding older generations. The default
    output is formatted text; use --json for the complete structured response.
`)
}

func printTaskHelp() {
	fmt.Print(`Usage:
  pua task create [<title>] [--project=<project>] [--slug <slug>] [--detail <detail>|--task-markdown <markdown>|--template=<name> [--field <name>=<value>...] [--fields <file>]] [--title <title>] [--profile=<name>|--agent=<name>] [--dry-run] [--json]
  pua task list [--project=<project>] [--all]
  pua task show [--project=<project>] [--task=<task>]
  pua task archive [--project=<project>] [--task=<task>]
  pua task binding set [--project=<project>] [--task=<task>] (--profile=<name>|--agent=<name>) [--server=<url>]
  pua task status [--project=<project>] [--task=<task>] [--server=<url>]
  pua task state [--project=<project>] [--task=<task>] [--server=<url>]
  pua task state set <waiting|blocked|paused|completed> [--note <text>] [--project=<project>] [--task=<task>] [--server=<url>]
  pua task history [--project=<project>] [--task=<task>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]

Commands:
  pua task create [<title>] [--project=<project>] [--slug <slug>] [--detail <detail>|--task-markdown <markdown>|--template=<name>] [--field <name>=<value>...] [--fields <file>] [--title <title>] [--profile=<name>|--agent=<name>] [--dry-run]
    Create the next task under the project in a short taskN/ or taskN-<slug>/
    directory, including task.json, task.md, artifacts/, worktree/,
    and task-local AGENTS.md. Conversation history is created lazily through
    resource History. <title> is written to task.json and shown by task list.
    --detail initializes the Background section in the default task.md scaffold.
    --task-markdown writes the complete task.md file and is mutually exclusive
    with --detail. <project> may be a full id such as project22 or just a number
    such as 22. When omitted, PUA uses the project containing the current
    working directory. Send the first message separately with pua message
    send; that delivery creates a generation lazily. If CLI output is ambiguous,
    query before attempting another create.

  pua task list [--project=<project>] [--all]
    List open tasks in a project. Use --all to include archived tasks.
    <project> may be a full id such as project22 or just a number such as 22.
    When omitted, PUA uses the project containing the current working
    directory.

  pua task show [--project=<project>] [--task=<task>]
    Print a task's task.json as formatted JSON. <task> may be a short id such
    as task4, or just a number such as 4. PUA combines it with --project when
    provided, otherwise the current directory's project. When <task> is omitted,
    PUA uses the task containing the current working directory.

  pua task archive [--project=<project>] [--task=<task>]
    Move an open task into its project archive. <task> follows the same rules
    as pua task show.

  pua task binding set [--project=<project>] [--task=<task>] (--profile=<name>|--agent=<name>) [--server=<url>]
    Set a Task's explicit Profile or direct Agent binding through the owning
    pua serve process. Task selection follows pua task show. The server
    validates configured Profile names and converges a running generation
    when the resolved Agent changes.

  pua task status [--project=<project>] [--task=<task>] [--server=<url>]
    Query the owning pua serve process for the task's public session state
    and generation status, message counts, waiting messages, steer capability,
    and recent delivery error. Task selection follows pua task show.

  pua task state [--project=<project>] [--task=<task>] [--server=<url>]
    Read the Task workflow state. Use pua task state set to finish an Agent
    Turn with waiting, blocked, paused, or completed; waiting, blocked, and
    paused require --note.

  pua task history [--project=<project>] [--task=<task>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
    Read one bounded newest-first page of the task's long-lived conversation
    through the owning pua serve process. Results are grouped into ordered
    generation segments. Explicit gap segments identify missing, unavailable, or
    damaged AgentHub history without hiding older generations. The default
    output is formatted text; use --json for the complete structured response.
`)
}

func printSchedulerHelp() {
	fmt.Print(`Usage:
  pua scheduler list [--json] [--server=<url>]
  pua scheduler show --id=<schedule> [--server=<url>]
  pua scheduler add --description=<text> --condition=<text> --target=<resource> [--guard=<text>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]
  pua scheduler update --id=<schedule> --revision=<n> [--description=<text>] [--condition=<text>] [--guard=<text>] [--target=<resource>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]
  pua scheduler pause --id=<schedule> [--server=<url>]
  pua scheduler resume --id=<schedule> [--server=<url>]
  pua scheduler remove --id=<schedule> [--server=<url>]

Commands:
  pua scheduler list [--json] [--server=<url>]
    List schedules with runtime state, next run, and last outcome.

  pua scheduler show --id=<schedule> [--server=<url>]
    Print one portable definition and its runtime projection as JSON.

  pua scheduler add ...
    Create a native one-time, fixed-interval, or six-field cron schedule.
    Cron timezones must be explicit IANA names; repeating rules run no more
    frequently than once per 60 seconds.

  pua scheduler update ...
    Compare-and-swap an existing definition using its current revision and one
    complete replacement trigger.

  pua scheduler pause ...
    Pause a definition without replaying occurrences missed while paused.

  pua scheduler resume ...
    Resume a definition from its next future occurrence.

  pua scheduler remove ...
    Remove a definition deterministically.

All reads and mutations go through the owning pua serve process. If owner
discovery fails, start the Server or pass --server explicitly.
CLI add and update accept complete structured triggers only. Use the Web UI or
Scheduler Agent to compile natural-language requests before changing a
definition.
`)
}

func printTemplateHelp() {
	fmt.Print(`Usage:
  pua template list [--project=<project>] [--json]
  pua template show [--project=<project>] [--json|--raw|--schema] <name>
  pua template validate [--project=<project>] [<name>|--all] [--json]
  pua template render [--project=<project>] [--field <name>=<value>...] [--fields <file>] [--title <title>] [--json] <name>
  pua template create [--project=<project>] [--title=<title>] <name>

Commands:
  pua template list|show|validate|render|create ...
    Manage project-local schema V2 content templates. Templates declare typed
    fields and deterministic title/Markdown rendering. list and validate
    include invalid templates. show defaults to metadata, field requirements,
    diagnostics, and the complete Markdown body; use --raw for the original
    file, --json for structured template data, or --schema for schema metadata
    and diagnostics. render and task create --dry-run have no side effects.
`)
}

func printWorkspaceHelp() {
	fmt.Print(`Usage:
  pua workspace status [--server=<url>]
  pua workspace history [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
  pua workspace tree --json
  pua workspace resource --id=<resource> --json
  pua workspace binding set (--profile=<name>|--agent=<name>) [--server=<url>]

Commands:
  pua workspace status [--server=<url>]
    Query the owning pua serve process for the Workspace's public session
    state and generation status, message counts, waiting messages, steer
    capability, and recent delivery error.

  pua workspace history [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
    Read one bounded newest-first page of the Workspace's long-lived
    conversation through the owning pua serve process. Results are grouped
    into ordered generation segments. Explicit gap segments identify missing,
    unavailable, or damaged AgentHub history without hiding older generations.
    The default output is formatted text; use --json for the complete structured
    response.

  pua workspace tree --json
    Print a lightweight JSON tree of open projects, open tasks, and resource
    runtime state for web and tool integrations.

  pua workspace resource --id=<resource> --json
    Print detail JSON for one project or task, including common Markdown files,
    artifacts, worktrees, and task repository metadata discovered from Git.

  pua workspace binding set (--profile=<name>|--agent=<name>) [--server=<url>]
    Set the Workspace's explicit Profile or direct Agent binding through the
    owning pua serve process. The server validates configured Profile names
    and converges a running generation when the resolved Agent changes.
`)
}

func printMessageHelp() {
	fmt.Print(`Usage:
  pua message send --to=<resource|user> [--mode=steer|enqueue|interrupt] [--subscribe-result=false] [--server=<url>] <message>
  pua message show --id=<message-id> [--server=<url>]

Commands:
  pua message send --to=<resource|user> [--mode=steer|enqueue|interrupt] [--subscribe-result=false] [--server=<url>] <message>
    When --to is a stable resource id (workspace, scheduler, projectN, or
    projectN.taskM), persist a message in the target resource mailbox through
    the owning pua serve process. steer is the default.
    A message that actually opens a Turn subscribes to its result by default;
    a message delivered as steer into an
    existing Turn does not. Pass --subscribe-result=false to disable the
    opener result. The current directory's stable work-subject id and
    Workspace instance id are sent as role=agent provenance. A valid injected
    PUA resource environment takes precedence over cwd; provenance is not
    authentication or instruction priority.

    The message text is sent verbatim. For multi-line text use real newlines
    (for example $'line1\nline2'), or pass - to read the message from
    standard input, for example:
      pua message send --to=project1.task2 - <<'EOF'

    When --to is a registered user name, durably append the message to that
    user's inbox for display in the Web GUI. The user reads it in the Inbox
    panel and may reply; the reply arrives as an ordinary role=user resource
    mailbox message addressed to the sending resource. --mode and
    --subscribe-result apply only to resource targets.

  pua message show --id=<message-id> [--server=<url>]
    Query the current delivery record for a stable mailbox message id. Status
    and message commands discover the owner from the Workspace control
    directory (.pua/serve.lock);
    --server explicitly overrides its diagnostic address.
`)
}

func printHistoryHelp() {
	fmt.Print(`Usage:
  pua history turn show --ref=<reference> [--server=<url>] [--json]
  pua history event show --ref=<reference> [--server=<url>] [--json]

Commands:
  pua history turn|event show --ref=<reference> [--server=<url>] [--json]
    Expand a stable opaque reference returned by a resource history page or Turn
    detail. Turn details contain complete compact messages and Event ranges;
    Event details read one canonical AgentHub Event on demand. Neither command
    requires or accepts a run or AgentHub Session id. The default output is
    formatted text; use --json for the complete structured response.
`)
}

func parseProjectCreateArgs(args []string) (createResourceOptions, error) {
	options := createResourceOptions{}
	var description []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--slug=") {
			value := strings.TrimPrefix(arg, "--slug=")
			if value == "" {
				return createResourceOptions{}, errors.New("slug cannot be empty")
			}
			options.Slug = value
			continue
		}
		if arg == "--slug" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return createResourceOptions{}, errors.New(projectCreateUsage)
			}
			options.Slug = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--slug") {
			return createResourceOptions{}, errors.New(projectCreateUsage)
		}
		if strings.HasPrefix(arg, "--") {
			return createResourceOptions{}, errors.New(projectCreateUsage)
		}
		description = append(description, arg)
	}
	if len(description) == 0 {
		return createResourceOptions{}, errors.New(projectCreateUsage)
	}
	options.Description = strings.Join(description, " ")
	return options, nil
}

type taskCreateOptions struct {
	ParentID        string
	Title           string
	Detail          string
	DetailSet       bool
	TaskMarkdown    string
	TaskMarkdownSet bool
	Slug            string
	TemplateName    string
	FieldsFile      string
	Fields          []string
	DryRun          bool
	TitleSet        bool
	JSON            bool
	Binding         app.AgentBinding
	BindingSet      bool
}

func parseTaskCreateArgs(args []string) (taskCreateOptions, error) {
	if len(args) == 0 {
		return taskCreateOptions{}, errors.New(taskCreateUsage)
	}
	var options taskCreateOptions
	var title []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		handled, err := parseAgentBindingOption(args, &i, &options.Binding, &options.BindingSet, taskCreateUsage)
		if err != nil {
			return taskCreateOptions{}, err
		}
		if handled {
			continue
		}
		if strings.HasPrefix(arg, "--project=") {
			value := strings.TrimPrefix(arg, "--project=")
			if value == "" {
				return taskCreateOptions{}, errors.New("project cannot be empty")
			}
			if options.ParentID != "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			projectID, err := normalizeProjectArg(value)
			if err != nil {
				return taskCreateOptions{}, err
			}
			options.ParentID = projectID
			continue
		}
		if arg == "--project" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			if options.ParentID != "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			projectID, err := normalizeProjectArg(args[i+1])
			if err != nil {
				return taskCreateOptions{}, err
			}
			options.ParentID = projectID
			i++
			continue
		}
		if strings.HasPrefix(arg, "--slug=") {
			value := strings.TrimPrefix(arg, "--slug=")
			if value == "" {
				return taskCreateOptions{}, errors.New("slug cannot be empty")
			}
			options.Slug = value
			continue
		}
		if arg == "--slug" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.Slug = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--detail=") {
			options.Detail = strings.TrimPrefix(arg, "--detail=")
			options.DetailSet = true
			continue
		}
		if arg == "--detail" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.Detail = args[i+1]
			options.DetailSet = true
			i++
			continue
		}
		if strings.HasPrefix(arg, "--task-markdown=") {
			options.TaskMarkdown = strings.TrimPrefix(arg, "--task-markdown=")
			options.TaskMarkdownSet = true
			continue
		}
		if strings.HasPrefix(arg, "--template=") {
			if options.TemplateName != "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.TemplateName = strings.TrimSpace(strings.TrimPrefix(arg, "--template="))
			if options.TemplateName == "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			continue
		}
		if arg == "--template" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") || options.TemplateName != "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.TemplateName = strings.TrimSpace(args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "--fields=") {
			if options.FieldsFile != "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.FieldsFile = strings.TrimPrefix(arg, "--fields=")
			if options.FieldsFile == "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			continue
		}
		if arg == "--fields" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") || options.FieldsFile != "" {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.FieldsFile = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, "--field=") {
			options.Fields = append(options.Fields, strings.TrimPrefix(arg, "--field="))
			continue
		}
		if arg == "--field" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.Fields = append(options.Fields, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "--title=") {
			if options.TitleSet {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.Title, options.TitleSet = strings.TrimPrefix(arg, "--title="), true
			continue
		}
		if arg == "--title" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") || options.TitleSet {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.Title, options.TitleSet = args[i+1], true
			i++
			continue
		}
		if arg == "--dry-run" {
			if options.DryRun {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.DryRun = true
			continue
		}
		if arg == "--json" {
			if options.JSON {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.JSON = true
			continue
		}
		if arg == "--task-markdown" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return taskCreateOptions{}, errors.New(taskCreateUsage)
			}
			options.TaskMarkdown = args[i+1]
			options.TaskMarkdownSet = true
			i++
			continue
		}
		if strings.HasPrefix(arg, "--") {
			return taskCreateOptions{}, errors.New(taskCreateUsage)
		}
		title = append(title, arg)
	}
	if len(title) == 0 && options.TemplateName == "" {
		return taskCreateOptions{}, errors.New(taskCreateUsage)
	}
	if len(title) > 0 && options.TitleSet {
		return taskCreateOptions{}, errors.New("positional title and --title are mutually exclusive")
	}
	if !options.TitleSet {
		options.Title = strings.Join(title, " ")
	}
	contentSources := boolCount(options.DetailSet, options.TaskMarkdownSet, options.TemplateName != "")
	if contentSources > 1 {
		return taskCreateOptions{}, errors.New("--template, --detail, and --task-markdown are mutually exclusive")
	}
	if (options.FieldsFile != "" || len(options.Fields) > 0) && options.TemplateName == "" {
		return taskCreateOptions{}, errors.New("--field and --fields require --template")
	}
	if options.DryRun && options.TemplateName == "" {
		return taskCreateOptions{}, errors.New("--dry-run currently requires --template")
	}
	return options, nil
}

func parseProjectListArgs(args []string) (taskListOptions, error) {
	var options taskListOptions
	for _, arg := range args {
		switch arg {
		case "--all":
			if options.IncludeArchived {
				return taskListOptions{}, errors.New("usage: pua project list [--all]")
			}
			options.IncludeArchived = true
		default:
			return taskListOptions{}, errors.New("usage: pua project list [--all]")
		}
	}
	return options, nil
}

func resolveProjectArg(args []string, command string) (string, error) {
	projectID, err := parseProjectArg(args, command)
	if err != nil {
		return "", err
	}
	if projectID != "" {
		return projectID, nil
	}
	inferred, ok, err := inferCurrentProjectID()
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("could not infer current project; use pua project %s --project=<project>", command)
	}
	return inferred, nil
}

func parseProjectArg(args []string, command string) (string, error) {
	usage := fmt.Sprintf("usage: pua project %s [--project=<project>]", command)
	var project string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--project=") {
			value := strings.TrimPrefix(arg, "--project=")
			if value == "" {
				return "", errors.New("project cannot be empty")
			}
			if project != "" {
				return "", errors.New(usage)
			}
			project = value
			continue
		}
		if arg == "--project" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", errors.New(usage)
			}
			if project != "" {
				return "", errors.New(usage)
			}
			project = args[i+1]
			i++
			continue
		}
		return "", errors.New(usage)
	}
	return normalizeProjectArg(project)
}

func normalizeProjectArg(project string) (string, error) {
	return app.NormalizeProjectID(project)
}

func resolveTaskListArgs(args []string) (taskListOptions, error) {
	projectID, includeArchived, err := parseTaskListArgs(args)
	if err != nil {
		return taskListOptions{}, err
	}
	if projectID != "" {
		return taskListOptions{ProjectID: projectID, IncludeArchived: includeArchived}, nil
	}
	inferred, ok, err := inferCurrentProjectID()
	if err != nil {
		return taskListOptions{}, err
	}
	if !ok {
		return taskListOptions{}, errors.New("could not infer current project; use pua task list --project=<project>")
	}
	return taskListOptions{ProjectID: inferred, IncludeArchived: includeArchived}, nil
}

func resolveTaskArg(args []string, command string) (string, error) {
	projectID, task, err := parseTaskArg(args, command)
	if err != nil {
		return "", err
	}
	if task == "" {
		inferred, ok, err := inferCurrentTaskID()
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("could not infer current task; use pua task %s --task=<task>", command)
		}
		return inferred, nil
	}
	return normalizeTaskArg(projectID, task)
}

func parseTaskArg(args []string, command string) (string, string, error) {
	usage := taskShowUsage
	if command == "archive" {
		usage = taskArchiveUsage
	} else if command == "status" {
		usage = taskStatusUsage
	}
	var projectID string
	var task string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--project="):
			value := strings.TrimPrefix(arg, "--project=")
			if value == "" {
				return "", "", errors.New("project cannot be empty")
			}
			if projectID != "" {
				return "", "", errors.New(usage)
			}
			normalized, err := normalizeProjectArg(value)
			if err != nil {
				return "", "", err
			}
			projectID = normalized
		case arg == "--project":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", "", errors.New(usage)
			}
			if projectID != "" {
				return "", "", errors.New(usage)
			}
			normalized, err := normalizeProjectArg(args[i+1])
			if err != nil {
				return "", "", err
			}
			projectID = normalized
			i++
		case strings.HasPrefix(arg, "--task="):
			value := strings.TrimPrefix(arg, "--task=")
			if value == "" {
				return "", "", errors.New("task cannot be empty")
			}
			if task != "" {
				return "", "", errors.New(usage)
			}
			task = value
		case arg == "--task":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", "", errors.New(usage)
			}
			if task != "" {
				return "", "", errors.New(usage)
			}
			task = args[i+1]
			i++
		default:
			return "", "", errors.New(usage)
		}
	}
	return projectID, strings.TrimSpace(task), nil
}

func normalizeTaskArg(projectID, task string) (string, error) {
	normalizedTask, err := app.NormalizeTaskName(task)
	if err != nil {
		return "", err
	}
	if projectID == "" {
		inferred, ok, err := inferCurrentProjectID()
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New("could not infer current project; use --project=<project>")
		}
		projectID = inferred
	}
	return app.NormalizeTaskID(projectID, normalizedTask)
}

func parseTaskListArgs(args []string) (string, bool, error) {
	var projectID string
	includeArchived := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--all":
			if includeArchived {
				return "", false, errors.New(taskListUsage)
			}
			includeArchived = true
		case strings.HasPrefix(arg, "--project="):
			value := strings.TrimPrefix(arg, "--project=")
			if value == "" {
				return "", false, errors.New("project cannot be empty")
			}
			if projectID != "" {
				return "", false, errors.New(taskListUsage)
			}
			normalized, err := normalizeProjectArg(value)
			if err != nil {
				return "", false, err
			}
			projectID = normalized
		case arg == "--project":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", false, errors.New(taskListUsage)
			}
			if projectID != "" {
				return "", false, errors.New(taskListUsage)
			}
			normalized, err := normalizeProjectArg(args[i+1])
			if err != nil {
				return "", false, err
			}
			projectID = normalized
			i++
		default:
			return "", false, errors.New(taskListUsage)
		}
	}
	return projectID, includeArchived, nil
}
