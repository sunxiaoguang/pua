# Unified PUA and AgentHub web applications

This directory owns the unified Svelte 5 and TypeScript project for both PUA and AgentHub: shared UI primitives, frontend tests, build tooling, and two production entries. `src/entry.ts` mounts PUA; `src/agenthub/entry.ts` mounts AgentHub. The default Vite build emits deterministic PUA assets into `static/assets`, while `vite.agenthub.config.ts` emits AgentHub assets into `../agenthub/frontend/dist/client` before the existing embed and Sites packaging steps copy them into place. The released binaries have no Node runtime dependency. Generated assets are never committed: run `npm --prefix web run build` (or `scripts/build`) after checkout.

The directory layers are intentional:

- `src/` contains editable application source; `src/agenthub/` owns the AgentHub-specific audit UI and reuses shared components from `src/components/`.
- `tests/` contains Vitest fixtures/component tests and Playwright flows.
- `static/` contains the HTML shell, immutable vendor/icon assets, and the (git-ignored, generated) `assets/pua-app.{js,css}` files served at runtime.
- `assets.go` exposes the complete static tree to the Go server through `go:embed`.

## Architecture and ownership

- `app-controller.ts` is the application composition boundary. It owns startup/shutdown, the canonical Workspace/Resource selection, auto-refresh ordering, and cross-domain invalidation. Domain projection, persistence, dialogs, and mutations live behind explicit controller dependencies.
- `controllers/` contains independently testable domain boundaries. Controllers receive browser/server access and cross-domain effects through explicit dependencies; none imports `app-controller.ts` or owns a second application-wide store.
- `models/` separates the common, create, settings, chat, detail, shell, and Workspace contracts. `components/models.ts` is a compatibility barrel only; production modules import their owning domain directly, and `noImplicitAny` is enabled.
- `components/` owns the entire interactive UI. Components receive typed models and callbacks through `ModelChannel`, and keep form, focus, selection, expansion, and pending-action state locally when that state belongs to the view.
- `entry.ts` creates the typed channels and mounts one `PUAApp` root. `agenthub/entry.ts` mounts the independent `AgentHubApp` root. Each binary still serves its own HTML shell and embedded asset tree.
- `api/client.ts` owns scoped request cancellation and stale-response rejection. Detail previews, Diff requests, uploads, and chat history are keyed by Workspace, Resource, generation, path, or mode identity as appropriate.
- `app.css` is the global style entry and imports only `styles/tokens.css`, browser defaults from `styles/base.css`, deliberately shared UI primitives, and the `.markdown-rendered` rich-content boundary. It must not contain component selectors.
- Every visual Svelte component owns an adjacent `ComponentName.css` module imported by that component. Selectors are bounded by the component's `data-component-owner`; composed roots declare the attribute at the owning boundary. `:where(...)` keeps the boundary from increasing the original selector specificity.
- A rule belongs in `styles/primitives.css` only when multiple independent component roots intentionally share the same class contract. Component-specific responsive rules, forced-colors behavior, reduced-motion behavior, state selectors, and keyframes stay with their component.
- Dynamic status classes remain inside their owning component boundary. Marked/DOMPurify output is styled only below `.markdown-rendered`, and Diff2Html vendor classes are styled only below the `DiffModal` owner and `.diff-viewer`; unbounded vendor selectors are not allowed.

When adding or changing styles, prefer the nearest component module. Promote a rule to a shared layer only after identifying multiple current consumers; do not add a component selector to `app.css` or move an unbounded global selector into another file. `tests/unit/css-ownership.test.ts` enforces the entry imports, component imports, ownership markers, and rich-content wrapper contract.

### Create dialog composition

The Create dialog keeps one identity-scoped `CreateDraft` while delegating each stable view responsibility to a directly testable component. The split reduced `CreateDialog.svelte` from 263 to 84 lines and its root-owned CSS from 456 to 93 lines.

```text
CreateDialog (channel subscription, modal lifecycle, draft identity, focus trap, submit orchestration)
├── ProjectCreateForm (Project description and slug)
└── TaskCreateForm (Task coordination and preview debounce)
    ├── TemplatePicker (blank/template selection)
    ├── TemplateFieldGroup × required/optional (schema field rendering)
    └── TaskPreview (blank/rendered preview, edit protection, reset and refresh)
```

Children edit the shared identity-scoped draft through typed props and callbacks; the parent does not mirror their form state. `TaskCreateForm` owns template defaults, switch confirmation, generated-title overrides, and preview scheduling. `TaskPreview` advances unedited Markdown when a newer preview arrives but preserves an actual local edit. HTTP cancellation, stale-response rejection, request payload conversion, and pending-submit deduplication remain in `controllers/create-dialog-controller.ts`.

Application state is never rendered by assembling HTML strings or mutating component-owned DOM. `DiffModal.svelte` is the single explicit rich-HTML boundary: Diff2Html converts a backend diff string to its vendor-defined presentation inside a dedicated viewer element. Markdown passes through Marked and DOMPurify before Svelte inserts the sanitized output.

### Settings view boundaries

`SettingsModal.svelte` is the coordination layer for modal visibility, keyboard/overlay close, active navigation, the shared AgentHub/Profile draft, dirty refresh protection, and the single cross-panel pending lease. It delegates rendering and domain actions to `SettingsNavigation.svelte` plus `WorkspaceSettingsPanel.svelte`, `UserSettingsPanel.svelte`, `AppearanceSettingsPanel.svelte`, `AgentHubSettingsPanel.svelte`, `ProfilesSettingsPanel.svelte`, and `NotificationSettingsPanel.svelte`. The parent dropped from 258 to 75 lines; its CSS dropped from 711 to 68 lines and now owns only the overlay, modal grid, close button, viewport, and responsive shell.

Each panel receives typed `SettingsModel` callbacks and the smallest relevant shared state. Panels do not import controllers, issue API requests, query another panel's DOM, or recreate AgentHub/Profile catalog derivation. Workspace owns add/remove/icon UI and pending errors; User owns browser-local name submission; Appearance owns the browser-local layout preference and per-column text scaling; AgentHub owns connection/catalog display and shared save; Profiles owns route validation, system/custom rules, unavailable-agent fallback, and shared save; Notifications owns permission/error/sound toggles. `SettingsPanel.css` contains the visual contract intentionally shared by all six panel roots, while each adjacent panel stylesheet owns its domain selectors and `SettingsNavigation.css` owns desktop/mobile tabs. Direct panel tests live in `tests/unit/settings-panels.test.ts`; parent refresh, dirty, pending, focus, identity, and close coordination is covered by `tests/unit/settings-modal.test.ts`.

### Controller boundaries

| Controller | Single responsibility | Creation and disposal |
| --- | --- | --- |
| `notification-controller.ts` plus `notification-{store,projection,delivery}.ts` | Orchestration; versioned persistence; completion projection; browser/sound delivery | Created for each application start because orchestration owns a `ResourceScope` and `BroadcastChannel`; delivery owns and disposes the optional `AudioContext` |
| `agent-draft-store.ts` | Versioned Workspace/Resource draft keys, generation metadata, local persistence, and bounded orphan eviction | Stateless adapter created once; browser storage is resolved lazily |
| `agent-draft-controller.ts` | Resource-scoped draft restore/persist/prune coordination | Created once over the application draft runtime |
| `resource-detail-controller.ts` | Resource detail fetch identity and stale-result rejection | Created once over the canonical detail records; requests are accepted only for the captured Workspace, Resource, and generation |
| `scheduler-mutation-controller.ts` | Scheduler target validation, HTTP adaptation, and keyed identity-scoped mutation leases | Created once; selection changes and application stop abort all pending Scheduler operations, while application composition releases successful leases after Tree/detail refresh, publication, and toast orchestration |
| `chat-state.ts` and `agent-operation-controller.ts` | Resource history/stream and Turn mutations with keyed pending state; stale operation leases cannot clear newer state | Created once; pending leases are reset during selection changes and application stop |
| `create-dialog-controller.ts` | Create Project/Task draft conversion, template preview cancellation, submission, and dialog identity | Created once; pending preview is aborted on close and application stop |
| `settings-controller.ts` and `user-settings-controller.ts` | Settings loading/mutation plus browser-local User identity persistence | Settings state is application-scoped; the User controller and its storage listener are recreated with each application lifecycle |
| `route-controller.ts` and `pane-layout-controller.ts` | Typed URL projection and persisted desktop/mobile layout state plus per-column text scales | Created once; browser state is applied during startup and callbacks publish immutable snapshots |
| `shell-projection.ts` | Pure ordering, lock, status, and Project/Task/resource-runtime presentation | Created once with Tree/resource lookup dependencies and a replaceable clock |

Dependencies point from `app-controller.ts` into these controllers, and from controllers only into typed component models or small runtime utilities. Cross-domain work such as switching Workspace, reconciling Tree + resource runtime state, and publishing several view roots remains in `app-controller.ts`; storage formats, request pagination, mutations, and pending-operation state remain inside their domain owner.

The shell has one canonical Workspace and Resource selection. That selection drives tree highlight, title, resource runtime state, unread state, and History API projection. Project, Task, History, and timeline rows use stable keys so unrelated refreshes retain their DOM identity. A drag transaction suppresses refresh until persistence succeeds or rolls back.

### App shell component boundaries

`AppShell.svelte` is the lifecycle and composition boundary, not the owner of sidebar widgets. It subscribes to the `AppShellModel`, projects History state, maintains viewport/body classes, and composes the desktop/mobile panes. Its direct component graph is:

| Component | Owned responsibility and local state | Style owner |
| --- | --- | --- |
| `MobileToolbar` | Mobile navigation, Details/Chat tabs, and sidebar backdrop | `mobile-toolbar` |
| `WorkspaceSwitcher` | Active Workspace presentation, menu dismissal, switch deduplication, pending state, and switch errors | `workspace-switcher` |
| `SchedulerNav` | Fixed Scheduler entry between the Workspace switcher and Project tree, with selection | `scheduler-nav` |
| `ProjectTree` | Keyed Project/Task rows, expansion/selection dispatch, same-kind drag ordering, and Tree empty/error states | `project-tree` |
| `AttentionList` | Server-computed focused resources, Project/Task star toggles, active-Turn retention without dismiss controls, and idle-row dismiss actions | `attention-list` |
| `StatusPresentation` | Shared status/lock icon markup and animation used by resource tree rows | `status-presentation` |
| `PaneResizeHandle` | Pointer preview/commit lifecycle and cleanup for the desktop resize handles | `pane-resize-handle` |

Callbacks and immutable typed props are the only parent/child coordination mechanism. Workspace menus, drag targets, drop previews, switch pending state, and pointer cleanup stay in their nearest owner. `AppShell` intentionally retains ModelChannel subscription, viewport keyboard correction, body-class projection, History push/replace/popstate, and the brand/workspace pane mount points because those span multiple children or application roots.

The Scheduler detail view reuses the normal resource binding and long-running chat surfaces. `SchedulerPanel` sends natural-language create/edit requests to the Scheduler resource as ordinary user/enqueue messages, while pause, resume, and remove stay direct structural operations. The list renders the Server projection: trigger, guard, effective state, next run, last occurrence outcome, errors, and migration status. There is no wake-interval setting; native timing is owned by the Server's dynamic deadline timer.

The pre-split baseline was 383 lines in `AppShell.svelte` and 1,270 lines in `AppShell.css`. After the split the root is 126 Svelte lines and 269 CSS lines; the six responsibility components plus shared status presentation total 498 Svelte lines and 1,104 CSS lines. The modest total increase is the explicit typed boundaries and duplicated private section-title/drag-handle rules; no selectors were promoted to a global stylesheet.

Form state is keyed by explicit identity, not refresh frequency. Republishing a model with the same identity preserves edits, focus, selection, scroll, uploads, and pending sends. Changing identity resets local state and invalidates work from the previous context. Document identity includes Workspace, Resource, path, display mode, and `contentHash`; AGENTS.md saves carry the baseline hash and preserve the local draft on HTTP 409.

### Event timeline boundary

`EventTimeline.svelte` is the only owner of the active resource/generation chat identity, `ChatSessionController`, projector identity, history pagination, selection deferral, and scroll anchoring/auto-fill. It delegates event markup to typed renderers: `TimelineMessage`, `ActivityGroup`/`ToolItem`, `ApprovalCard`, `LifecycleNotice`, the shared `TimelineNotice`, and `UnknownEvent`. Each uninterrupted thinking/tool run uses one `ActivityGroup`: the live tail opens automatically and folds when it settles, while expanding compact history loads its bounded Event range. Approval drafts and pending actions remain local to their keyed approval card; sanitized assistant and agent-to-agent Markdown remains inside `.markdown-rendered`.

`chat-state.ts` owns HTTP/SSE context generations, accepted resource/generation identities, the 80 ms stream publication window, notice reconciliation, and cleanup of requests, streams, and flush timers. It consumes the side-effect-free `timeline-events.ts` module for canonical merge, batched insertion, append healing, and cumulative ACP tool-update compaction. Rendering components never open network streams outside this controller.

The extraction reduced the stateful roots while retaining the complete behavior in focused modules:

| Source boundary | Before | After |
| --- | ---: | ---: |
| `EventTimeline.svelte` state plus all event markup | 311 lines | 203 lines, plus focused typed renderers |
| `chat-state.ts` state machine plus event algorithms | 514 lines | 393 lines, plus 123 lines in `timeline-events.ts` |
| One timeline stylesheet | 497 lines | 47 root lines plus 363 lines next to the nine renderers |

## Lifecycle contract

- One Svelte application root owns every component subtree and therefore one teardown path.
- `pagehide` stops the controller and unmounts the application root; `pageshow` mounts and starts a fresh application instance.
- `ResourceScope` owns controller DOM listeners, intervals, animation frames, and custom cleanup. Controller shutdown also closes EventSource, `BroadcastChannel`, and `AudioContext` instances, clears delayed renders and operation leases, aborts pending preview work, and flushes the active draft.
- Components release subscriptions, viewport timers, request controllers, streams, and upload XHRs from their Svelte teardown paths.
- Late HTTP responses, stream events, and send acknowledgements are rejected after their identity or generation changes.

## Performance gates

`tests/fixtures/performance.ts` provides deterministic stress fixtures. These tests mount large stress fixtures and assert wall-clock budgets plus bounded DOM growth, so they are deliberately opt-in: they are not part of the default `npm test` run and are excluded from CI. Run them on demand with `npm run test:perf` when performance-sensitive components such as the Project/Task tree, History timeline, Markdown rendering, or event canonicalization change. The budgets are deliberately generous to catch order-of-magnitude regressions and unbounded DOM growth:

| Scenario | Fixture | Budget |
| --- | ---: | ---: |
| Project/Task tree | 720 rows | 5,000 ms and fewer than 15,000 elements |
| Resource History | 750 Turn summaries | 4,000 ms and fewer than 10,000 elements |
| Markdown document | 3,000 sections | 1,000 ms |
| Generation event canonicalization | 10,000 events with an overlapping delta | 1,000 ms |
| Continuous resource updates | 1,000 deltas applied after 10,000 events | 1,500 ms |
| Cumulative ACP tool updates | 30,000 frames for one call | 1,000 ms and a two-event compacted timeline |

These gates complement component stability tests and Playwright flows; they are regression alarms rather than user-facing latency targets.

## Commands

```sh
npm ci
npm run check
npm test
npm run build
npm run test:perf
npm run test:e2e
```

`npm run test:perf` runs the opt-in performance and bounded-DOM gates described above; the regular `npm test` suite excludes them.

`npm run dev` proxies `/api` to PUA on `127.0.0.1:4936`. Run development and browser tests only against an isolated Workspace.
