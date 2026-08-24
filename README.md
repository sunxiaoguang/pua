# PUA

PUA is a local, filesystem-first workspace manager for people and AI coding agents. It combines a deterministic CLI with a responsive web UI for organizing projects and tasks, running interactive agent sessions, and reviewing the resulting files and Git changes.

**PUA — Projects, Users & Agents.** Agents pick up assignments and push until accomplished.

The workspace is the source of truth. Contracts are Markdown, structured state is JSON, resource History is the canonical conversation projection, generated output is stored as artifacts, and code changes live in task-owned Git worktrees. The web UI is a control plane over those files rather than a separate project database.

## Highlights

- **Transparent local state.** Projects, tasks, resource History, artifacts, templates, and Wiki pages remain inspectable workspace data that can be backed up or repaired without the web UI.
- **Purpose-built agent context.** Durable scope and acceptance criteria are paired with bounded resource History so a new agent can resume without reconstructing the task from an obsolete manual timeline.
- **Isolated code changes.** Repositories under `repos/` are shared source caches; each coding task records its own branch and worktree under `task.../worktree/`.
- **Explicit file ownership.** Generated agent instructions allow writes only in the starting resource and its task worktrees, while keeping other Workspace resources read-only.
- **Interactive agents through AgentHub.** The PUA web UI uses AgentHub as its only execution and conversation surface, including streaming chat, resumable history, file uploads, approvals, and mid-turn user intervention.
- **A workspace-oriented UI.** Switch between workspaces, browse projects and tasks, inspect Markdown and artifacts, preview Wiki pages, review worktree diffs, monitor resource runtime state, and use the details/chat layout on desktop or mobile. The layout adapts to the window width: three columns (sidebar, details, chat) on wide screens, two columns with a tabbed details/chat pane below 1440px, and a single-column mobile layout below 980px. The Appearance tab in System Settings lets users override the responsive choice manually — auto, three columns, tabbed two columns, or a split view that collapses the sidebar into a drawer with details and chat side by side — and scale the text of the sidebar, details, and chat columns independently; both preferences are stored in the browser.
- **Native structured scheduling.** Every Workspace has a PUA-managed Scheduler resource that compiles natural-language requests once, while the Server executes durable one-time, interval, and timezone-aware cron rules through ordinary resource mailboxes.

## Design

PUA separates concerns deliberately:

```text
pua CLI ───── internal/app ─┐
                              ├── AgentWorkspace files (source of truth)
pua serve ─── shared HTTP listener
  ├── PUA Web/API ── internal/app ── AgentWorkspace files
  └── /agenthub ── AgentHub application ── provider processes and sessions

agenthub ── the same AgentHub application at /agenthub

shared checkout in repos/ ── git worktree ── task-owned branch in worktree/
```

- The **CLI** owns flag parsing and stable user-facing output; deterministic workspace mutations and typed views live in the reusable `internal/app` API.
- **`pua serve`** renders workspace state in the web UI and embeds AgentHub on the same listener by default, while still talking to it through the public HTTP contract.
- **AgentHub** owns provider discovery, provider process lifecycle, provider-native configuration, and durable agent sessions.
- **Agents** may read other Workspace resources for context, but write only files owned by their starting resource and its task worktrees. Host files outside the Workspace follow user scope and host permissions.

Resource-level Session Locks are not part of PUA. Multiple sessions can run against the same resource; generated instructions provide the non-recursive coordination boundary.

## Requirements

- Go 1.26 or newer
- Git
- Node.js 22 or newer for frontend builds only

## Build

Clone the repository and build both binaries with branch and commit metadata embedded:

```bash
git clone https://github.com/disksing/pua.git
cd pua
scripts/build
```

This creates:

```text
bin/pua
bin/agenthub
```

`pua` provides the workspace CLI, PUA Web service, and embedded AgentHub.
`agenthub` provides the same AgentHub application as a standalone CLI/service.
Both binaries embed their Web UI and have no Node runtime dependency. Pass
another output directory to `scripts/build` if needed.

### Local releases

The macOS release workflow builds `pua` and `agenthub` for macOS and Linux on
both arm64 and amd64. macOS binaries are signed with a Developer ID Application
identity, submitted together to Apple's notary service, checked against the
resulting online notarization tickets, and then packaged separately. Linux
binaries are unsigned. All ZIP archives and notarization diagnostics are
written to a new output directory, along with `SHA256SUMS`.

Before the first release, install a valid Developer ID Application identity and
save notarization credentials in the login Keychain:

```bash
xcrun notarytool store-credentials pua-notary \
  --apple-id "you@example.com" \
  --team-id "YOUR_TEAM_ID" \
  --keychain "$HOME/Library/Keychains/login.keychain-db"
```

`notarytool` prompts for an app-specific password and validates it before
saving. Never put the password, certificate private key, or exported `.p12`
file in the repository.

Run the release from a clean macOS checkout and provide an explicit version:

```bash
scripts/release-local 0.1.0
```

By default the command creates `dist/0.1.0/` containing:

```text
pua-0.1.0-darwin-arm64.zip
pua-0.1.0-darwin-amd64.zip
pua-0.1.0-linux-arm64.zip
pua-0.1.0-linux-amd64.zip
SHA256SUMS
notarization-submit.json
notarization-log.json
```

Pass a second argument to choose another output directory. The command refuses
to overwrite an existing path or release a dirty worktree. If more than one
Developer ID Application identity is installed, select one explicitly with
`CODESIGN_IDENTITY`. Set `NOTARYTOOL_KEYCHAIN_PROFILE` when the credentials use
a profile name other than `pua-notary`, or `NOTARYTOOL_KEYCHAIN` when they are
stored in a keychain other than the user's default keychain.

Apple creates online notarization tickets for the standalone Mach-O binaries,
but neither those binaries nor their ZIP containers support ticket stapling.
A newly downloaded macOS archive therefore needs network access when macOS
first looks up its notarization tickets; the programs themselves do not
otherwise require a network connection. `spctl` only accepts app-like code and
reports a standalone CLI as "the code is valid but does not seem to be an app";
the release workflow instead checks other notarized code with
`codesign --check-notarization`.

## Quick Start

Create a workspace and its first project:

```bash
mkdir AgentWorkspace
cd AgentWorkspace

pua init --language=en
pua project create --slug pua-dev "Develop PUA"
pua repo add pua https://github.com/disksing/pua.git
pua task create --project=project1 --slug first-change \
  --detail "Implement and verify the first change." \
  "First change"
```

Open the web UI for that workspace:

```bash
pua serve --workspace "$PWD"
```

Then visit [http://127.0.0.1:4936](http://127.0.0.1:4936). The embedded
AgentHub Web UI is at [http://127.0.0.1:4936/agenthub/](http://127.0.0.1:4936/agenthub/),
its API base is `http://127.0.0.1:4936/agenthub/v1`, and Agent Profiles remain
configurable in Settings.

To run the two services separately, start the standalone binary and select
external mode explicitly:

```bash
agenthub serve --addr=127.0.0.1:4646
pua serve --workspace "$PWD" \
  --agenthub-mode=external \
  --agenthub-endpoint=http://127.0.0.1:4646/agenthub
```

Standalone and embedded AgentHub use the same canonical `/agenthub` base path;
only the host and port change. The standalone root URL redirects to
`/agenthub/`, and no root-level `/v1` compatibility route is provided.

### Upgrade from separate PUA and AgentHub services

Before switching to embedded mode, stop the old PUA service and standalone
AgentHub so the new process can acquire the existing AgentHub daemon lock.
Then start only `pua serve`; it reads the existing `~/.agenthub` config,
Sessions, Archive, and logs in place without copying or rewriting Session
facts. To roll back to a split deployment, stop embedded PUA, start the
`agenthub` binary from the same release, and start PUA in external mode with
its canonical `http://host:port/agenthub` endpoint. Never run embedded and
standalone AgentHub against the same data directory simultaneously.

PUA and embedded AgentHub have no built-in authentication and share the same
network exposure. The default loopback address is appropriate for local use;
do not expose it to an untrusted network.

## PUA Web UI

The main UI is split into navigation, resource details, and agent chat:

- **Navigation:** switch workspaces, open the fixed Scheduler entry, expand the project/task tree, and monitor each resource's current runtime state.
- **Details:** render Scheduler context, schedules, `project.md`, `task.md`, and resource History; browse templates and artifacts; preview the workspace Wiki; inspect repository/worktree metadata; and render tracked plus untracked Git diffs.
- **Chat:** select a Workspace, Scheduler, Project, or Task and send a message directly; PUA lazily creates or reuses that work subject's current generation. The resource timeline continues across generation boundaries, shows explicit history gaps, and pages older Turns without exposing Session lifecycle controls. At an idle Turn boundary, the composer can explicitly end the current generation; PUA safely stops and archives its AgentHub Session, retires the generation, and waits for the next message to create a successor. A new generation recovers from the brief, bounded recent resource history, task-worktree Git state, and artifacts rather than a second permanent progress file. Waiting mailbox messages appear above the composer and can be inserted into the active Turn without changing message ID when steer is supported.
- **Settings:** add or remove workspaces, choose one of the bundled workspace icons, edit the user-owned portion of workspace `AGENTS.md`, manage Workspace-local users and preferences, inspect the read-only AgentHub catalog, map Profiles to catalog agents, and choose the one-time Profile defaults for newly created Workspaces, Projects, and Tasks. Each browser selects an existing user independently for each Workspace; creating a Workspace requires an initial user name, which is prefilled from the Server account when that name is valid.

The desktop panes are resizable. On smaller screens, navigation becomes a drawer and details/chat become switchable views.

### AgentHub execution

PUA does not import provider adapters, spawn provider CLIs, probe provider health, or keep a direct-runner fallback. Agent and provider definitions in the AgentHub catalog are read-only in PUA. Every Workspace, Scheduler, Project, and Task stores an explicit binding to either a PUA Profile or an AgentHub Agent. The chat composer options bar presents Profiles and direct Agents in one selector and saves a new binding as soon as it is picked: Profile entries show their current Agent, while Agent entries list every Profile that targets them. Profile bindings resolve to an `agentName`; a missing custom Profile preserves that explicit binding and falls back through the resource-type default and then global `default`, exposing the unresolved binding and actual fallback on the generation. Saving Profile routes does not scan resources or pre-mark generations for replacement. The mailbox resolves the current binding immediately before each new Turn: an unchanged Agent reuses the generation, while a changed Agent retires it and creates the successor lazily. A steer into an active Turn keeps using its current Agent. The Scheduler defaults to Profile `fast` and then the global `default` fallback if `fast` is unavailable.

PUA owns the complete Provider prompt and sends AgentHub a schema-v2 message containing that prompt as `text` plus a `pua.resource-message.v1` opaque payload. The payload retains the original text, role, sender, Workspace instance, type, and causation for PUA history and recovery; AgentHub forwards only the top-level text without interpreting PUA metadata. The timeline decodes the payload and shows the original message with its sender. Browser user selection is scoped to the stable Workspace instance, and requests for personal state require a selected registered user. Historical messages without sender provenance may still display the legacy `User` label.

The Provider-facing envelope follows the target Workspace language (`en` or `zh-CN`) and states what the Turn opener receives: a user can observe progress and the final reply, a subscribed Agent receives only the final reply, and an unsubscribed Agent or system sender receives neither. An active-Turn insertion names the actual Turn opener and its response channel; when the insertion came from a different address, the envelope also includes the exact `pua message send --to=...` command for a separate reply. An insertion from the opener itself stays compact. PUA freezes this presentation context before the first delivery attempt, so a Workspace language change or Turn transition cannot rewrite the same stable message during recovery. The opaque payload remains `pua.resource-message.v1`, and PUA still recognizes the previous English envelope when recovering an input accepted before upgrade.

PUA persists each resource generation under `<workspace>/.pua/runtime/resources/<resource-key>/`. The resource key is derived from the stable Workspace instance ID and normalized resource ID, so it is unambiguous and independent of the Workspace path. A versioned `generation-store.json` marker and staging directory make migration from the old runtime generation index repeatable; legacy files remain as rollback evidence, while records without a generation ID are isolated as cold history. The same resource directory contains the atomic mailbox `hot.json`, `receipts.json`, `outbox.json`, `scheduler.json`, and `commit.json`: hot state retains complete messages while delivery, recovery, notification, or Scheduler turn-boundary work is unresolved; terminal messages become minimal receipts without body text.

A ready current generation is automatically slept after 30 minutes of continuous idle when there is no active Turn or approval, pending mailbox delivery, or lifecycle convergence. PUA persists the ready boundary and Stop-confirms the exact AgentHub Session, then retains that same generation and Session as an addressable idle-suspended resource. A later user, agent, system, or Scheduler message resumes that exact Session on demand and only delivers after the Session is ready; a stopped current Session observed after a PUA or AgentHub restart follows the same path. Temporary Resume failures use durable exponential backoff from five seconds to five minutes, so polling and Server restarts cannot create a restart storm. With no pending message, a stopped Session stays stopped and no provider work starts. Only a binding/profile change, explicit generation end, resource archive, archived/missing Session, explicit provider/native resume failure, or other replacement intent archives and retires the generation. Polling and Server restarts do not reset the deadline, and the resource history remains continuous across the retained generation.

Resource messages use `steer` (default), `enqueue`, or `interrupt`. A supported active Turn receives steer immediately; an unsupported steer is durably downgraded to enqueue. Enqueue waits for a ready boundary. Interrupt first persists its mailbox item, records the exact active Turn, interrupts only that Turn, waits for terminal state, and then opens a new Turn. Already-delivering messages resolve first; otherwise interrupt outranks steer, and steer outranks queued enqueue, with acceptance order preserved inside each class. A live steer or interrupt may therefore bypass older enqueue work. Generation replacement never moves mailbox ownership: delivered steer remains with the old Turn, enqueue waits for the new generation, and interrupt terminates the old Turn before replacement delivery. The periodic reconciler recovers all modes after Server or AgentHub failures. Archived resources reject new messages: items not yet sent become `undeliverable`, while an already-attempted item whose AgentHub outcome cannot be confirmed becomes `delivery_unknown`; both remain queryable by message ID.

Public work-subject state is `idle`, `working`, `attention_required`, `unavailable`, or `archived`. Waiting is a message state and count, never a Task state: the persisted internal `queued` value is exposed as `waiting`. Promoting a waiting message to the current Turn records `promotedAt` and changes its actual mode to steer on the same durable mailbox item.

The Web sidebar has a server-owned Activity panel below the resource tree with independent tabs: Running, Unread, Problems, and Inbox. Every tab title always includes its item count, including zero. Running includes resources with an active Turn; Unread contains resources whose latest completed resource-wide Turn ordinal is newer than the current user's read cursor; Problems contains Tasks whose persisted workflow state is blocked or error. A resource may appear in multiple tabs. Unread Project/Task rows show their own non-aggregated unread count badge. Opening a resource marks only the latest completed Turn observed by that client as read. An active Turn does not count as unread and cannot be marked read; if it finishes while the resource stays selected, the completed Turn becomes unread until the row is clicked again. Read cursors are per-user state in `<workspace>/.pua/users/<name>/ui-state.json`; archiving a resource removes that state for every user. Version 1 Follow/Dismiss state and the pre-rename `gui-state.json` are migrated on startup, preserving tracked read cursors while baselining previously untracked resources at their current latest completed Turn; the legacy Follow flags and the later Favorite state have been dropped.

### Scheduler

`pua init` creates the special `scheduler/` resource with portable schema v2 definitions. Each schedule has a revision, a persisted activation revision that advances only when its trigger or target changes, lifecycle state, original condition, optional guard, target, and exactly one native trigger: an offset-bearing one-time timestamp, a fixed interval plus anchor, or a six-field cron expression plus an explicit IANA timezone. The special `scheduler` management/compiler resource cannot itself be an execution target. Repeating rules are limited to one occurrence per 60 seconds. Before `pua migrate` replaces a valid v1 definition, it durably preserves the exact original bytes as `scheduler/scheduler.v1.json`.

A file-only rollback is supported only before any v2 Server has accepted or dispatched native Scheduler mailbox work, for example immediately after an offline migration and before the first v2 `pua serve`. In that case, stop the current PUA process, restore `scheduler/scheduler.v1.json` as `scheduler/scheduler.json`, then run the v1 binary's `pua migrate` from the Workspace before starting its `pua serve`; the v1 migration regenerates the PUA-managed Scheduler guidance in `scheduler/AGENTS.md` while preserving content outside the managed block. Once native execution has begun, restoring only `scheduler.json` is unsupported: durable `schedule_occurrence` or `schedule_migration` messages may remain in resource mailboxes, and a dispatched message may already own an external AgentHub Turn. Restore a complete pre-v2 Workspace backup instead, keep the v2 Server stopped, and do not start v1 until every v2-started AgentHub Turn is terminal. An alternative rollback requires an explicitly supported drain or cancellation procedure that covers both queued mailbox records and active external Turns; this release does not provide one. After restoring a pre-v2 backup, run the v1 binary's `pua migrate` before `pua serve` so the managed guidance matches the restored runtime.

Migration to v2 preserves every v1 action/condition/target as an inert `needs_compilation` definition until the Scheduler Agent compiles it without guessing ambiguous timing or timezone details; a historical self-target remains inert until the user chooses another open resource.

Normal execution does not wake the Scheduler Agent. The Server persists a per-schedule cursor and immutable prepared occurrence in the Scheduler runtime checkpoint, sets one dynamic timer to the earliest Workspace deadline, and delivers due work through the existing target resource mailbox. Restart and sleep catch-up coalesces missed repeating occurrences into one message; one-time work is delivered once. Repeating work skips an occurrence when the target already has an active Turn or hot mailbox, while one-time work remains queued. Occurrence causation includes stable schedule/revision/occurrence IDs and coalescing bounds, so targets can make external side effects idempotent.

The Scheduler Agent is the natural-language compilation and management entry point. An edit request must compile to exactly one complete replacement trigger. If its timing or IANA timezone is ambiguous, the Agent asks for clarification instead of guessing; compilation, validation, or revision compare-and-swap failure leaves the existing schedule unchanged. The Web form sends the Scheduler Agent an ordinary user message; direct pause, resume, and remove operations remain structural. Update non-trigger fields are optional, but the current revision and replacement trigger are required. A guard is checked by the target Agent at occurrence time and is never interpreted by the Scheduler. The CLI requires the owning Server:

```text
pua scheduler list [--json] [--server=<url>]
pua scheduler show --id=<schedule> [--server=<url>]
pua scheduler add --description=<text> --condition=<text> --target=<resource> --at=<rfc3339>
pua scheduler add --description=<text> --condition=<text> --target=<resource> --every=5m --anchor=<rfc3339>
pua scheduler add --description=<text> --condition=<text> --target=<resource> --cron='0 0 9 * * *' --timezone=Asia/Shanghai
pua scheduler update --id=<schedule> --revision=<n> [--description=<text>] [--condition=<text>] [--guard=<text>] [--target=<resource>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>)
pua scheduler pause|resume|remove --id=<schedule>
```

Commands address Workspace, Project, and Task directly without exposing a separate Agent subject or requiring run/Session IDs:

```bash
pua task status --project=project1 --task=task2
pua task history --project=project1 --task=task2
pua history turn show --ref=<opaque-turn-reference>
pua history event show --ref=<opaque-event-reference>
pua message send --to=project1.task2 "Please review the current API design."
pua message send --to=project1.task2 --mode=enqueue "Handle this in a new Turn."
pua message send --to=project1.task2 --mode=interrupt "Stop the current approach and investigate this instead."
pua message show --id=msg-run-0123456789abcdef
```

Message text is sent verbatim and may use Markdown. For multi-line text use real newlines (for example `$'line1\nline2'` quoting or a heredoc), or pass `-` to read the message from standard input: `pua message send --to=project1.task2 - <<'EOF'`. The CLI warns when the text looks like it carries a literal `\n` escape sequence instead of a real newline.

These commands infer the sending resource from the current directory, attach `role=agent` and its stable resource ID as provenance, and contact the owning `pua serve` address discovered from the selected control directory's `serve.lock`. `--server=<url>` is an explicit override. History lists and Turn/Event details default to formatted text for direct reading; pass `--json` for the complete structured response. `pua message show` returns a body only while the message is hot; a retained cold receipt is explicitly marked as a receipt and remains queryable by its status and provenance, while an aged-out ID returns `message_receipt_expired`/HTTP 410 and an ID beyond the expired-index window returns `message_not_found`.

Useful overrides:

```text
PUA_SERVE_CONFIG    serve configuration file
```

When neither variable is set, PUA stores the serve configuration at
`~/.pua/serve.json`.

Each running serve instance exclusively locks its configuration file, and every managed Workspace is additionally owned by exactly one `pua serve` process through an OS advisory `serve.lock` in its selected control directory. A second instance cannot write a Workspace owned by another instance: startup fails before runtime recovery begins.

## Task Worktrees

PUA discovers worktree metadata from Git but leaves Git operations explicit. A typical coding task looks like this:

```bash
repo="$PWD/repos/pua"
task="$PWD/project1-pua-dev/task1-first-change"

git -C "$repo" worktree add \
  -b task1-first-change \
  "$task/worktree/pua" \
  master
```

Use an absolute destination with `git worktree add`, especially when combining it with `git -C`; otherwise Git may resolve the destination relative to the shared checkout. PUA derives the task's repositories, branches, and diffs directly from the worktrees under the task's `worktree/` directory; nothing needs to be recorded in task.json.

## Interactive Agent Sessions

Interactive agents are launched through the PUA web UI and AgentHub. A resource-managed conversation has one current generation, addressed by the resource ID and generation ID. AgentHub Session IDs are retained only as provider-correlation facts in durable generation records and history; they are not PUA resource addresses or lifecycle controls. Archiving a resource reconciles its generation with AgentHub and preserves the durable history.

## Task Templates

Project-local content templates live in `templates/*.md`. Schema V2 declares a dynamic input form and deterministic title/Markdown rendering; it never chooses whether or how the task runs:

When creating a task, prefer an existing suitable project template whenever one is available. When creating a task from a template, preserve all existing template rules by default: do not delete, weaken, bypass, or accidentally override them. Override a particular rule only when the user explicitly asks for that override.

```markdown
---
schema-version: 2
title: Request or bug
description: Capture a concrete change.
task-title: "{{ summary }}"
fields:
  - name: summary
    type: text
    label: Summary
    required: true
  - name: behavior
    type: textarea
    label: Expected behavior
    required: true
  - name: priority
    type: select
    label: Priority
    options: [low, medium, high]
  - name: reproduced
    type: boolean
    label: Reproduced
    default: false
---
# {{ summary }}

{{ behavior }}

Priority: {{ priority }}
Reproduced: {{ reproduced }}
```

Field types are `text`, `textarea`, `select`, and `boolean`. Placeholders may only reference declared fields and are replaced once, so template-like text inside a field value stays literal. Unknown properties, fields, placeholders, type mismatches, and missing required values produce stable structured errors. The normalized template bytes have a SHA-256 digest; previewed creates can submit that digest to detect template changes.

Use `pua template list/show/validate/render/create` to inspect and manage templates. `pua template show <name>` defaults to human-readable metadata, every field requirement, diagnostics, and the complete Markdown body. Use `--raw` for the original template file, `--json` for structured template data, or `--schema` for schema metadata and diagnostics; these output modes are mutually exclusive. `pua task create --template=<name> --field name=value` creates from one, while `--dry-run` previews without filesystem side effects. `--fields` accepts a YAML or JSON object and repeated `--field` values override the file. Execution settings are not part of V2 content templates.

Templates without `schema-version` are invalid. Created tasks store only the template name, schema version, and digest in `task.json`; later template edits do not modify them.

## Workspace Layout

```text
AgentWorkspace/
  AGENTS.md                   global human and agent instructions
  workspace.json              workspace configuration
  .pua/runtime/generation-store.json  generation store schema/migration marker
  .pua/runtime/resources/<resource-key>/  current/retired generations plus mailbox bundle
    current.json                    mutable current generation record
    generations/                    immutable retired generation manifests
    hot.json                         unresolved complete mailbox messages only
    receipts.json                    bounded terminal receipts and expired-ID index
    outbox.json                      recoverable notification operations
    scheduler.json                   native schedule cursors and prepared occurrences
    commit.json                      mailbox multi-document recovery marker
  .pua/runtime/resources/.message-locations/       rebuildable message lookup index
  wiki/
    index.md                  long-lived workspace knowledge
  scheduler/
    scheduler.json            portable native schedule definitions
    scheduler.md              Scheduler Agent durable judgment context
    AGENTS.md                 generated Scheduler resource instructions
  repos/
    pua/                    shared normal checkout
  project1-pua-dev/
    AGENTS.md                 generated project launch card
    project.json              structured project metadata
    project.md                durable project contract
    templates/                reusable task templates
    artifacts/                project outputs
    task1-first-change/
      AGENTS.md               generated task launch card
      task.json               task and repository metadata
      task.md                 durable task contract
      artifacts/              reports, screenshots, uploads, patches
      worktree/               task-owned Git worktrees
    archive/                  archived tasks
  archive/                    archived projects
```

Open/archive state is represented by directory location. Human-readable directory suffixes do not change resource ids: `project1-pua-dev/task1-first-change/` is still `project1.task1`.

The Workspace tree contains only open Projects and Tasks. Archived resources remain addressable through resource lookup, archived listings, history, and Project detail views, but do not appear in the default navigation tree.

Archive is a reversible, non-destructive directory move. Archiving a Project moves its complete subtree, including open Tasks, in one top-level rename; `--all`, resource lookup, history, and the Web API continue to find the archived resources. PUA performs only read-only best-effort Git checks before the move. Dirty or unmerged worktrees, missing target branches, unverifiable Git state, open child Tasks, and post-move worktree repair failures are returned as structured warnings; PUA never resets, cleans, stashes, deletes, or commits source code for archive. Runtime Session stop/archive and pending mailbox convergence continue asynchronously after the directory move, and a warning does not roll back a completed move.

### File Roles

| File | Role |
| --- | --- |
| `project.md`, `task.md` | Durable contracts: background, scope, acceptance criteria, stable constraints, decisions, and contract-changing questions. |
| Resource History | Bounded generation/Turn conversation history, exposed by the History API and CLI. |
| `project.json`, `task.json` | Versioned structured facts PUA understands. Arbitrary notes belong in Markdown. |
| `AGENTS.md` | Workspace operating rules plus generated project/task launch cards. PUA rewrites only its marked managed block. |
| `wiki/` | Long-lived knowledge shared across projects and tasks, with `wiki/index.md` as the entry point. |
| `artifacts/` | Generated reports, screenshots, patches, uploaded files, and other outputs. |

## CLI Reference

Run `pua help` for the top-level command list, and `pua help <command>` (or `<command> --help`) for subcommand details. The current command surface is:

```text
pua --version
pua init [--language=<language>]
pua migrate [--language=<language>]

pua repo add [--bare] <name> <url>
pua repo list

pua project create [--slug <slug>] <description>
pua project list [--all]
pua project show [--project=<project>]
pua project archive [--project=<project>]
pua project binding set [--project=<project>] (--profile=<name>|--agent=<name>) [--server=<url>]
pua template list|show|validate|render|create|migrate ...

pua scheduler list|show ... [--server=<url>]
pua scheduler add ... (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]
pua scheduler update --id=<schedule> --revision=<n> [--description=<text>] [--condition=<text>] [--guard=<text>] [--target=<resource>] (--at=<rfc3339>|--every=<duration> --anchor=<rfc3339>|--cron=<six-fields> --timezone=<iana>) [--server=<url>]
pua scheduler pause|resume|remove --id=<schedule> [--server=<url>]

pua agent list [--server=<url>] [--json]
pua user list [--json]

pua workspace status [--server=<url>]
pua workspace history [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
pua workspace tree --json
pua workspace resource --id=<resource> --json
pua workspace binding set (--profile=<name>|--agent=<name>) [--server=<url>]
pua project status [--project=<project>] [--server=<url>]
pua project history [--project=<project>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
pua task status [--project=<project>] [--task=<task>] [--server=<url>]
pua task history [--project=<project>] [--task=<task>] [--cursor=<cursor>] [--limit=<n>] [--server=<url>] [--json]
pua history turn show --ref=<reference> [--server=<url>] [--json]
pua history event show --ref=<reference> [--server=<url>] [--json]
pua message send --to=<resource> [--mode=steer|enqueue|interrupt] [--server=<url>] <message>
pua message show --id=<message-id> [--server=<url>]
pua task create [<title>] [--project=<project>] [--slug <slug>]
                  [--detail <detail>|--task-markdown <markdown>|--template=<name>]
                  [--field <name>=<value>...] [--fields <file>]
                  [--profile=<name>|--agent=<name>] [--dry-run] [--json]
pua task list [--project=<project>] [--all]
pua task show [--project=<project>] [--task=<task>]
pua task archive [--project=<project>] [--task=<task>]
pua task binding set [--project=<project>] [--task=<task>] (--profile=<name>|--agent=<name>) [--server=<url>]

pua serve [--addr=<address>] [--workspace=<path>]
          [--agenthub-mode=embedded|external]
          [--agenthub-endpoint=<url>] [--version]
```

`pua agent list` queries the owning `pua serve` process for the configured PUA Agent Profiles and the read-only AgentHub agent catalog. The default output lists profiles (key, agent, description) followed by agents (name, provider, availability); pass `--json` for the complete structured result including profiles, providers, and probes.

`pua user list` lists the users registered in the current Workspace together with their free-text preferences. User profiles, preferences, UI/read state, and Inbox data live in the Workspace control directory under `.pua/users/<name>/`. Agents can address a registered user with `pua message send --to=<name>`. Running `pua init`, `pua migrate`, or `pua user list` never creates a user implicitly; an empty Workspace receives its first user through the Web identity flow, while Web-created Workspaces require that initial name during creation.

`pua init` and `pua migrate` accept `--language=en` or `--language=zh-CN`.
The selected language is stored in `workspace.json` and controls generated Markdown
templates and PUA-managed `AGENTS.md` prompts. Existing workspaces without a
language setting default to English. Use `pua migrate --language=zh-CN` (or
`--language=en`) to switch languages.

`workspace.json` also carries an optional `name`: a display name editable from
the web Settings workspace tab (mirroring the Project and Task titles kept in
their resource metadata). Workspaces without a name fall back to the directory
base name.

Workspace, Project, and Task creation is local and uses the shared `internal/app` application boundary. Creation no longer persists resource creator metadata; Agent sender provenance remains on messages and is validated from the injected generation environment. Creation sends no initial message and creates no generation; call `pua message send` separately, which lazily creates the first generation. A message that actually opens a Turn subscribes to that Turn's result by default; a message delivered as steer into an existing Turn does not. A steer request downgraded to enqueue becomes an opener and does subscribe. Pass `--subscribe-result=false` to disable the result for an opening message. If a create command commits but its output is lost, query the resource before deciding whether to issue a new create operation.

Subscribed terminal Turn results and terminal cross-resource delivery failures return through the source resource's recoverable outbox as structured system messages with stable `type`, `causation`, and receipt metadata. Only the Agent message that opened a Turn receives its final result; steer inputs delivered into that Turn require an explicit PUA reply when needed. The generated body is retained only until the target mailbox accepts it; after that, source and target retain bounded summaries while AgentHub canonical history remains the content source. Generated messages force `subscribeResult=false` and never recursively generate another notification. Use `pua message show` for delivery diagnostics and `pua history turn show` for Turn references.

`pua migrate` upgrades supported resource metadata, generation/mailbox stores,
legacy task history, Scheduler resources, and PUA-managed `AGENTS.md` blocks.
Repeated migration is safe.

```markdown
<!-- managed by pua cli -->
...
<!-- end of pua cli prompt -->
```

## Development

Run the full test suite and build all binaries:

```bash
cd web && npm ci && npm run check && npm test && npm run test:e2e && cd ..
go test -race ./...
go vet ./...
scripts/build
```

`scripts/build` validates the single Svelte/TypeScript project, builds its
separate PUA and AgentHub entries, embeds their generated assets, and produces
`pua` plus `agenthub`. Node is required only for
development and builds; neither shipped binary has a Node runtime dependency.
Focused and full Go tests compile directly from a clean checkout without a
frontend build. The release script enables an internal `embed_frontend` build
tag that still fails if either generated frontend entrypoint is missing.
For frontend development against an isolated Workspace, run
`scripts/frontend-dev /path/to/isolated/AgentWorkspace` and open the Vite URL.
See [web/README.md](web/README.md) for the PUA frontend ownership, lifecycle,
and performance contracts.

Useful focused commands:

```bash
go test ./internal/pua
go test ./internal/serve/...
go run ./cli/cmd/pua help
go run ./cli/cmd/pua serve --workspace /path/to/AgentWorkspace
```

When testing a second serve instance, isolate all mutable state. Each Workspace can only be managed by one `pua serve` process at a time, so a test instance must point at its own isolated Workspace; pointing it at a real Workspace now fails fast with a lock-conflict error instead of corrupting shared state, but tests must still use isolated Workspaces to avoid real business writes:

```bash
PUA_SERVE_CONFIG=/tmp/pua-serve-test/serve.json \
AGENTHUB_HOME=/tmp/pua-agenthub-test \
  go run ./cli/cmd/pua serve \
  --addr 127.0.0.1:4999 \
  --workspace /tmp/pua-workspace-test
```

## Companion Tools

- [PUA serve AgentHub guide](internal/serve/README.md): the execution boundary, current settings, environment variables, and isolated testing.

## License

[BSD 3-Clause License](LICENSE)
