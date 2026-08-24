// Regression tests for the lucide icon rendering rework: Icon.svelte resolves
// lucide icon data and renders Svelte-owned SVG, so icons can change name and
// toggle visibility without leaving orphaned SVGs in the DOM (previously the
// global createIcons replaceChild detached Svelte's marker nodes and icons
// accumulated until a page refresh).
import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import AppShell from "../../src/components/AppShell.svelte";
import ChatComposer from "../../src/components/ChatComposer.svelte";
import Icon from "../../src/components/Icon.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import type { ComposerModel, WaitingMessage } from "../../src/models/chat";
import type { AppShellModel, ShellResourceItem, ShellStatusPresentation } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
  document.body.className = "";
  delete (window as unknown as { lucide?: unknown }).lucide;
});

// installFakeLucide exposes the icon data tables Icon.svelte consumes, using a
// small subset of the real lucide icon nodes.
function installFakeLucide(): void {
  (window as unknown as { lucide: unknown }).lucide = {
    icons: {
      CornerUpLeft: ["svg", {}, [["polyline", { points: "9 14 4 9 9 4" }], ["path", { d: "M20 20v-7a4 4 0 0 0-4-4H4" }]]],
      LoaderCircle: ["svg", {}, [["path", { d: "M21 12a9 9 0 1 1-6.219-8.56" }]]],
      ChevronRight: ["svg", {}, [["path", { d: "m9 18 6-6-6-6" }]]],
      Clock3: ["svg", {}, [["circle", { cx: "12", cy: "12", r: "10" }], ["polyline", { points: "12 6 12 12 16.5 12" }]]],
    },
  };
}

const emptyStatus: ShellStatusPresentation = {
  hasTaskState: false, className: "", layoutClassName: "", slotClassName: "", statuses: [],
};

function resource(id: string, title: string, type: "project" | "task" = "project"): ShellResourceItem {
  return {
    id, type, title, ref: "#1", active: false, expanded: type === "project",
    ariaLabel: title, statusLabel: "", status: emptyStatus, summary: null, children: [], unreadCount: 0,
  };
}

function schedulerItem(): ShellResourceItem {
  return {
    id: "scheduler", type: "scheduler", title: "Scheduler", ref: "", active: false, expanded: false,
    ariaLabel: "Scheduler", statusLabel: "Workspace Scheduler", status: emptyStatus, summary: null, children: [], unreadCount: 0,
  };
}

function model(overrides: Partial<AppShellModel> = {}): AppShellModel {
  return {
    identity: "workspace-a", loading: false, error: "", version: "v0.1.0", activeWorkspaceId: "workspace-a", workspaceName: "Workspace A",
    userGate: { mode: "", users: [], suggestedUserName: "", missingUserName: "" },
    workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg" }],
    projects: [], treeEditing: false, activity: { running: [], unread: [], problems: [] }, inbox: [],
    doctor: { checking: false, complete: true, summary: { errors: 0, warnings: 0 }, workspaces: [] },
    paneSizes: { sidebarWidth: 280, chatWidth: 420, chatHeight: 320, sidebarAttentionHeight: 210 },
    mobile: { sidebarOpen: false },
    layout: { preference: "auto", effective: "three" },
    route: { path: "", revision: 0, replace: true },
    resolveResourceTitle: () => null,
    onSwitchWorkspace: vi.fn(async () => undefined), onResolveWorkspaceUser: vi.fn(async () => undefined), onAddWorkspace: vi.fn(), onCreateProject: vi.fn(), onOpenSettings: vi.fn(), onRefreshDoctor: vi.fn(async () => undefined),
    onToggleProject: vi.fn(async () => undefined), onSelectResource: vi.fn(async () => undefined), onReorder: vi.fn(async () => undefined),
    onDragState: vi.fn(), onToggleTreeEditing: vi.fn(), onCreateFolder: vi.fn(async () => ""), onRenameFolder: vi.fn(async () => undefined),
    onDeleteFolder: vi.fn(async () => undefined), onToggleFolder: vi.fn(async () => undefined), onOpenInboxMessage: vi.fn(async () => undefined), onReplyInboxMessage: vi.fn(async () => undefined), onDeleteInboxMessage: vi.fn(async () => undefined), onPanePreview: vi.fn(), onPaneCommit: vi.fn(), onPaneViewport: vi.fn(), onMobileSidebar: vi.fn(),
    onToast: vi.fn(),
    onHistoryNavigation: vi.fn(async () => undefined),
    ...overrides,
  };
}

function composerModel(overrides: Partial<ComposerModel> = {}): ComposerModel {
  return {
    identity: "", workspaceId: "workspace-a", resourceId: "project1.task1", draft: "", draftKey: "", draftResetVersion: 0,
    unavailableReason: "", sending: false, canEndTurn: false, endingTurn: false, canEndGeneration: false, endingGeneration: false,
    stopNotice: "", waitingMessages: [], canSteerWaiting: true, steeringMessageId: "",
    agentBinding: { kind: "profile", name: "default" }, agentProfiles: [], agents: [], bindingSaving: false,
    onDraft: vi.fn(), onSend: vi.fn(async () => ({ accepted: true, clear: true })), onOpenUpload: vi.fn(),
    onEndTurn: vi.fn(), onEndGeneration: vi.fn(), onDismissStopNotice: vi.fn(), onSteerWaiting: vi.fn(async () => undefined),
    onSaveAgentBinding: vi.fn(async () => undefined),
    ...overrides,
  };
}

const waitingMessage: WaitingMessage = { messageId: "m1", text: "hello", status: "waiting", acceptedAt: "", requestedMode: "steer", actualMode: "steer", canPromote: true };

async function mountShell(channel: ReturnType<typeof createModelChannel<AppShellModel>>): Promise<void> {
  const target = document.createElement("div");
  document.body.appendChild(target);
  const app = mount(AppShell, { target, props: { channel } });
  cleanups.push(async () => { await unmount(app); });
  await tick();
}

describe("Icon component", () => {
  it("renders a Svelte-owned svg carrying the data-lucide marker and icon paths", async () => {
    installFakeLucide();
    const target = document.createElement("div");
    document.body.appendChild(target);
    const app = mount(Icon, { target, props: { name: "clock-3", className: "scheduler-nav-icon" } });
    cleanups.push(async () => { await unmount(app); });
    await tick();
    const svg = target.querySelector('svg[data-lucide="clock-3"]');
    expect(svg).not.toBeNull();
    expect(svg!.getAttribute("class")).toContain("lucide-clock-3");
    expect(svg!.getAttribute("class")).toContain("scheduler-nav-icon");
    const circle = svg!.querySelector("circle");
    expect(circle).not.toBeNull();
    expect(circle!.namespaceURI).toBe("http://www.w3.org/2000/svg");
    expect(target.querySelector("i")).toBeNull();
  });
});

describe("icon duplication regressions", () => {
  it("keeps a single composer steer icon that actually swaps names while steering toggles", async () => {
    installFakeLucide();
    const channel = createModelChannel(composerModel({ waitingMessages: [waitingMessage] }));
    const target = document.createElement("div");
    document.body.appendChild(target);
    const app = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(async () => { await unmount(app); });
    await tick();
    for (let round = 0; round < 4; round++) {
      channel.publish(composerModel({ waitingMessages: [waitingMessage], steeringMessageId: "m1" }));
      await tick();
      channel.publish(composerModel({ waitingMessages: [waitingMessage], steeringMessageId: "" }));
      await tick();
    }
    const button = document.querySelector(".chat-message-steer");
    expect(button).not.toBeNull();
    const icons = button!.querySelectorAll("[data-lucide]");
    expect(icons.length).toBe(1);
    expect(icons[0].getAttribute("data-lucide")).toBe("corner-up-left");
    channel.publish(composerModel({ waitingMessages: [waitingMessage], steeringMessageId: "m1" }));
    await tick();
    const busy = button!.querySelectorAll("[data-lucide]");
    expect(busy.length).toBe(1);
    expect(busy[0].getAttribute("data-lucide")).toBe("loader-circle");
    expect(busy[0].querySelector("path")).not.toBeNull();
  });

  it("keeps a single project chevron icon while children appear and disappear", async () => {
    installFakeLucide();
    const withTask = { ...resource("project-a", "Project A"), children: [resource("project-a.task1", "Task 1", "task")] };
    const withoutTask = resource("project-a", "Project A");
    const channel = createModelChannel(model({ projects: [withTask] }));
    await mountShell(channel);
    for (let round = 0; round < 4; round++) {
      channel.publish(model({ projects: [withoutTask] }));
      await tick();
      channel.publish(model({ projects: [withTask] }));
      await tick();
    }
    const chevron = document.querySelector(".tree-item .chevron");
    expect(chevron).not.toBeNull();
    expect(chevron!.querySelectorAll("[data-lucide]").length).toBe(1);
    channel.publish(model({ projects: [withoutTask] }));
    await tick();
    expect(document.querySelector(".tree-item .chevron")!.querySelectorAll("[data-lucide]").length).toBe(0);
  });

  it("keeps a single scheduler nav icon while the scheduler item toggles", async () => {
    installFakeLucide();
    const channel = createModelChannel(model({ scheduler: schedulerItem() } as Partial<AppShellModel>));
    await mountShell(channel);
    for (let round = 0; round < 4; round++) {
      channel.publish(model({ scheduler: null } as Partial<AppShellModel>));
      await tick();
      channel.publish(model({ scheduler: schedulerItem() } as Partial<AppShellModel>));
      await tick();
    }
    const row = document.querySelector('[data-component-owner="scheduler-nav"] button');
    expect(row).not.toBeNull();
    expect(row!.querySelectorAll('[data-lucide="clock-3"]').length).toBe(1);
  });
});
