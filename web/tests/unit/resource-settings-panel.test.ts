import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ResourceSettingsPanel from "../../src/components/ResourceSettingsPanel.svelte";
import type { DetailPanelModel } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];

afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

function model(overrides: Partial<DetailPanelModel> = {}): DetailPanelModel {
  return {
    identity: "ws-test:scheduler:scheduler",
    workspaceId: "ws-test",
    workspaceName: "Test workspace",
    resourceId: "scheduler",
    resourceType: "scheduler",
    resourceTitle: "Scheduler",
    parent: null,
    loading: false,
    detail: {
      id: "scheduler",
      type: "scheduler",
      title: "Scheduler",
      path: "scheduler",
      scheduler: {
        schemaVersion: 2,
        agentBinding: { kind: "profile", name: "default" },
        schedules: [],
      },
    },
    wiki: null,
    workspaceAgents: null,
    workspaceDefaults: { project: { kind: "profile", name: "default" }, task: { kind: "profile", name: "default" } },
    workspaceUsers: [],
    currentUserName: "User",
    generationPolicy: { enabled: true, maxTurns: 20, maxAccumulatedTurnMinutes: 120 },
    stallWatchdogPolicy: { enabled: true, timeoutMinutes: 30 },
    agentBinding: { kind: "profile", name: "default" },
    agentProfiles: [{ key: "default", description: "Default", agentName: "fake-agent" }],
    agents: [{ id: "fake-agent", label: "Fake Agent", summary: "fake" }],
    resolveResourceTitle: () => null,
    onNavigate: vi.fn(),
    onCreateTask: vi.fn(),
    onArchive: vi.fn(),
    onSaveWorkspaceAgents: vi.fn(async () => ({ path: "AGENTS.md", content: "", contentHash: "saved" })),
    onSaveMarkdownFile: vi.fn(async (path: string, content: string) => ({ path, content, contentHash: "saved" })),
    onDeleteArtifact: vi.fn(async () => undefined),
    onSaveAgentBinding: vi.fn(async () => undefined),
    onRenameResource: vi.fn(async () => undefined),
    onSaveDescription: vi.fn(async () => undefined),
    onSaveWorkspaceDefaults: vi.fn(async () => undefined),
    onSaveWorkspaceUserPreference: vi.fn(async () => undefined),
    onSwitchWorkspaceUser: vi.fn(async () => undefined),
    onAddWorkspaceUser: vi.fn(async () => undefined),
    onDeleteWorkspaceUser: vi.fn(async () => undefined),
    onSaveGenerationPolicy: vi.fn(async () => undefined),
    onSaveStallWatchdogPolicy: vi.fn(async () => undefined),
    onSaveTaskDefault: vi.fn(async () => undefined),
    onToast: vi.fn(),
    ...overrides,
  };
}

describe("ResourceSettingsPanel", () => {
  it("switches and adds Workspace users without auto-switching the new user", async () => {
    const onSwitchWorkspaceUser = vi.fn(async () => undefined);
    const onAddWorkspaceUser = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: { model: model({ resourceType: "workspace", resourceId: "workspace", workspaceUsers: [{ version: 1, name: "Alice", preference: "" }, { version: 1, name: "Bob", preference: "" }], currentUserName: "Alice", onSwitchWorkspaceUser, onAddWorkspaceUser }) },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const selector = target.querySelector<HTMLSelectElement>("#workspaceCurrentUser")!;
    selector.value = "Bob";
    selector.dispatchEvent(new Event("change", { bubbles: true }));
    await vi.waitFor(() => expect(onSwitchWorkspaceUser).toHaveBeenCalledWith("Bob"));

    const input = target.querySelector<HTMLInputElement>("#workspaceNewUser")!;
    await vi.waitFor(() => expect(input.disabled).toBe(false));
    input.value = "Carol";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    input.form!.requestSubmit();
    await vi.waitFor(() => expect(onAddWorkspaceUser).toHaveBeenCalledWith("Carol"));
    expect(onSwitchWorkspaceUser).toHaveBeenCalledTimes(1);
  });

  it("keeps the Workspace Generation Save disabled until the policy changes", async () => {
    const onSaveGenerationPolicy = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: {
        model: model({
          identity: "ws-test:workspace:workspace",
          resourceId: "workspace",
          resourceType: "workspace",
          resourceTitle: "Test workspace",
          detail: null,
          onSaveGenerationPolicy,
        }),
      },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const save = target.querySelector<HTMLButtonElement>(".resource-settings-policy-controls .secondary-button")!;
    expect(save.textContent).toContain("Save");
    expect(save.disabled).toBe(true);
    expect(save.hasAttribute("disabled")).toBe(true);

    const maxTurns = target.querySelector<HTMLInputElement>('[aria-label="Maximum Turns per Generation"]')!;
    maxTurns.value = "25";
    maxTurns.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(save.disabled).toBe(false);
  });

  it("saves the Workspace Turn stall watchdog policy", async () => {
    const onSaveStallWatchdogPolicy = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: {
        model: model({
          identity: "ws-test:workspace:workspace",
          resourceId: "workspace",
          resourceType: "workspace",
          resourceTitle: "Test workspace",
          detail: null,
          onSaveStallWatchdogPolicy,
        }),
      },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const enabled = target.querySelector<HTMLInputElement>('[aria-label="Enable Turn stall watchdog"]')!;
    const timeout = target.querySelector<HTMLInputElement>('[aria-label="Turn stall watchdog timeout in minutes"]')!;
    const save = target.querySelector<HTMLButtonElement>(".resource-settings-stall-watchdog-controls button")!;
    expect(enabled.checked).toBe(true);
    expect(timeout.value).toBe("30");
    expect(save.disabled).toBe(true);

    enabled.click();
    timeout.value = "45";
    timeout.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    save.click();
    await vi.waitFor(() => expect(onSaveStallWatchdogPolicy).toHaveBeenCalledWith({ enabled: false, timeoutMinutes: 45 }));
  });

  it("keeps Scheduler settings focused on the compilation Agent", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, { target, props: { model: model() } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector('[aria-label="Scheduler wake interval in minutes"]')).toBeNull();
    expect(target.textContent).toContain("Compiles and manages schedule definitions");
    expect(target.querySelector('[aria-label="Scheduler Agent binding"]')).not.toBeNull();
    expect(fetch).not.toHaveBeenCalled();
  });
});
