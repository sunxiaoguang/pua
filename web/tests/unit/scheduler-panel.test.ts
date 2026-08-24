import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import SchedulerPanel from "../../src/components/SchedulerPanel.svelte";
import type { SchedulerMutationCallbacks } from "../../src/models/detail";
import type { ScheduleRecord, SchedulerConfigRecord } from "../../src/models/workspace";

const mounted: Array<ReturnType<typeof mount>> = [];

const config: SchedulerConfigRecord = {
  schemaVersion: 2,
  agentBinding: { kind: "profile", name: "default" },
  schedules: [],
};

const schedule: ScheduleRecord = {
  id: "schedule-0123456789abcdef01234567",
  revision: "1",
  description: "Recover target",
  condition: "once",
  target: "project1.task1",
  state: "active",
  trigger: { type: "at", at: "2026-08-23T09:00:00Z" },
  createdAt: "2026-08-23T08:00:00Z",
  updatedAt: "2026-08-23T08:00:00Z",
  effectiveState: "active",
};

function schedulerActions(overrides: Partial<SchedulerMutationCallbacks> = {}): SchedulerMutationCallbacks {
  return {
    validateTarget: (resourceId) => {
      const target = resourceId.trim();
      if (!target) return "Target resource is required.";
      if (target === "scheduler") return "The Scheduler resource cannot be a schedule target.";
      return target === "workspace" || target === "project1.task1"
        ? ""
        : "Target must be an open resource in the current Workspace.";
    },
    save: vi.fn(async () => true),
    setPaused: vi.fn(async () => true),
    remove: vi.fn(async () => true),
    ...overrides,
  };
}

function mountPanel(panelConfig = config, actions = schedulerActions()) {
  const target = document.createElement("section");
  document.body.append(target);
  const component = mount(SchedulerPanel, {
    target,
    props: {
      config: panelConfig,
      actions,
    },
  });
  mounted.push(component);
  return { target, actions };
}

function inputValue(element: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  element.value = value;
  element.dispatchEvent(new Event("input", { bubbles: true }));
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function editorFields(target: HTMLElement) {
  return {
    description: target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!,
    condition: target.querySelector<HTMLTextAreaElement>("textarea")!,
    target: target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")!,
    save: target.querySelector<HTMLButtonElement>(".schedule-editor > button")!,
  };
}

afterEach(async () => {
  while (mounted.length) await unmount(mounted.pop()!);
  document.body.replaceChildren();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("SchedulerPanel", () => {
  it("declares its owner boundary on each visual root", () => {
    const { target } = mountPanel();

    expect(Array.from(target.children).map((root) => root.getAttribute("data-component-owner"))).toEqual([
      "scheduler-panel",
      "scheduler-panel",
    ]);
  });

  it("marks an unknown target invalid and prevents the schedule request", async () => {
    const { target, actions } = mountPanel();
    await tick();

    inputValue(target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!, "Review the release");
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "when the release is ready");
    const targetInput = target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")!;
    inputValue(targetInput, "not-a-resource");
    await tick();

    expect(targetInput.getAttribute("aria-invalid")).toBe("true");
    expect(targetInput.getAttribute("aria-describedby")).toBe("schedule-target-error");
    expect(target.querySelector("#schedule-target-error")?.textContent).toContain("open resource in the current Workspace");
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(true);

    target.querySelector<HTMLButtonElement>(".schedule-editor > button")!.click();
    expect(actions.save).not.toHaveBeenCalled();
  });

  it("rejects the Scheduler management resource as an execution target", async () => {
    const { target, actions } = mountPanel();
    await tick();

    inputValue(target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!, "Review the release");
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "when the release is ready");
    const targetInput = target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")!;
    inputValue(targetInput, "scheduler");
    await tick();

    expect(targetInput.getAttribute("aria-invalid")).toBe("true");
    expect(target.querySelector("#schedule-target-error")?.textContent).toContain("cannot be a schedule target");
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(true);
    target.querySelector<HTMLButtonElement>(".schedule-editor > button")!.click();
    expect(actions.save).not.toHaveBeenCalled();
  });

  it("accepts a resource resolved from the current Workspace", async () => {
    const save = vi.fn(async () => true);
    const { target } = mountPanel(config, schedulerActions({ save }));
    await tick();

    inputValue(target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!, "Review the release");
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "when the release is ready");
    const targetInput = target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")!;
    inputValue(targetInput, "project1.task1");
    await tick();

    expect(targetInput.getAttribute("aria-invalid")).toBe("false");
    const addButton = target.querySelector<HTMLButtonElement>(".schedule-editor > button")!;
    expect(addButton.disabled).toBe(false);
    addButton.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith({
      description: "Review the release",
      condition: "when the release is ready",
      target: "project1.task1",
    });
  });

  it("clears only the unchanged form after a successful save", async () => {
    const completion = deferred<boolean>();
    const save = vi.fn(() => completion.promise);
    const { target } = mountPanel(config, schedulerActions({ save }));
    await tick();
    const fields = editorFields(target);

    inputValue(fields.description, "Review the release");
    inputValue(fields.condition, "when the release is ready");
    await tick();
    fields.save.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());

    completion.resolve(true);
    await vi.waitFor(() => expect(fields.description.value).toBe(""));
    expect(fields.condition.value).toBe("");
    expect(fields.target.value).toBe("workspace");
  });

  it.each([
    { completed: true, label: "success" },
    { completed: false, label: "failure" },
  ])("preserves field edits that overlap a late save $label", async ({ completed }) => {
    const completion = deferred<boolean>();
    const save = vi.fn(() => completion.promise);
    const { target } = mountPanel(config, schedulerActions({ save }));
    await tick();
    const fields = editorFields(target);

    inputValue(fields.description, "Review the release");
    inputValue(fields.condition, "when the release is ready");
    await tick();
    fields.save.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    inputValue(fields.description, "Review the newer draft");

    completion.resolve(completed);
    await vi.waitFor(() => expect(fields.save.classList.contains("busy")).toBe(false));
    expect(fields.description.value).toBe("Review the newer draft");
    expect(fields.condition.value).toBe("when the release is ready");
  });

  it("preserves another schedule selected while a save is pending", async () => {
    const completion = deferred<boolean>();
    const save = vi.fn(() => completion.promise);
    const otherSchedule = {
      ...schedule,
      id: "schedule-fedcba9876543210fedcba98",
      revision: "7",
      description: "Publish target",
      condition: "daily",
      target: "workspace",
    };
    const { target } = mountPanel({ ...config, schedules: [schedule, otherSchedule] }, schedulerActions({ save }));
    await tick();
    const editButtons = Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button"))
      .filter((button) => button.textContent?.trim() === "Edit");

    editButtons[0].click();
    await tick();
    editorFields(target).save.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenLastCalledWith(expect.objectContaining({
      scheduleId: schedule.id,
      expectedRevision: "1",
    }));
    editButtons[1].click();
    await tick();

    completion.resolve(true);
    await vi.waitFor(() => expect(editorFields(target).save.classList.contains("busy")).toBe(false));
    expect(target.querySelector(".schedule-editor-heading strong")?.textContent).toBe("Edit schedule");
    expect(editorFields(target).description.value).toBe("Publish target");
    expect(editorFields(target).condition.value).toBe("daily");
    expect(editorFields(target).target.value).toBe("workspace");
    expect(target.querySelector(".schedule-list article.editing code")?.textContent).toContain(otherSchedule.id);

    editorFields(target).save.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledTimes(2));
    expect(save).toHaveBeenLastCalledWith(expect.objectContaining({
      scheduleId: otherSchedule.id,
      expectedRevision: "7",
    }));
  });

  it("submits the revision displayed when a stale concurrent edit began", async () => {
    const save = vi.fn(async () => true);
    const concurrentlyUpdated = { ...schedule };
    const { target } = mountPanel({ ...config, schedules: [concurrentlyUpdated] }, schedulerActions({ save }));
    await tick();

    const editButton = Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button"))
      .find((button) => button.textContent?.trim() === "Edit")!;
    expect(target.querySelector(".schedule-list article code")?.textContent).toContain("r1");
    editButton.click();
    await tick();

    concurrentlyUpdated.revision = "2";
    inputValue(editorFields(target).description, "Review without overwriting the concurrent change");
    editorFields(target).save.click();

    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith({
      scheduleId: schedule.id,
      expectedRevision: "1",
      description: "Review without overwriting the concurrent change",
      condition: schedule.condition,
      target: schedule.target,
    });
  });

  it.each(["9007199254740992", "18446744073709551615"])("snapshots full-width revision %s exactly when editing", async (revision) => {
    const save = vi.fn(async () => true);
    const fullWidth = { ...schedule, revision };
    const { target } = mountPanel({ ...config, schedules: [fullWidth] }, schedulerActions({ save }));
    await tick();

    const editButton = Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button"))
      .find((button) => button.textContent?.trim() === "Edit")!;
    expect(target.querySelector(".schedule-list article code")?.textContent).toContain(`r${revision}`);
    editButton.click();
    await tick();
    editorFields(target).save.click();

    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith(expect.objectContaining({
      scheduleId: fullWidth.id,
      expectedRevision: revision,
    }));
  });

  it.each(["18446744073709551616", 9007199254740992 as unknown as string])("does not edit a schedule with malformed revision %j", async (revision) => {
    const malformed = { ...schedule, revision };
    const { target, actions } = mountPanel({ ...config, schedules: [malformed] }, schedulerActions());
    await tick();

    const editButton = Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button"))
      .find((button) => button.textContent?.trim() === "Edit")!;
    expect(editButton.disabled).toBe(true);
    editButton.click();
    await tick();
    expect(target.querySelector(".schedule-editor-heading strong")?.textContent).toBe("Add schedule");
    expect(actions.save).not.toHaveBeenCalled();
  });

  it("preserves a new draft started after cancelling a pending edit", async () => {
    const completion = deferred<boolean>();
    const save = vi.fn(() => completion.promise);
    const { target } = mountPanel({ ...config, schedules: [schedule] }, schedulerActions({ save }));
    await tick();
    const editButton = Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button"))
      .find((button) => button.textContent?.trim() === "Edit")!;

    editButton.click();
    await tick();
    editorFields(target).save.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    const cancel = Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-editor-heading button"))
      .find((button) => button.textContent?.trim() === "Cancel edit")!;
    cancel.click();
    await tick();
    inputValue(editorFields(target).description, "Create a new draft");
    inputValue(editorFields(target).condition, "next week");

    completion.resolve(true);
    await vi.waitFor(() => expect(editorFields(target).save.classList.contains("busy")).toBe(false));
    expect(target.querySelector(".schedule-editor-heading strong")?.textContent).toBe("Add schedule");
    expect(editorFields(target).description.value).toBe("Create a new draft");
    expect(editorFields(target).condition.value).toBe("next week");
    expect(target.querySelector(".schedule-list article.editing")).toBeNull();
  });

  it("offers direct recovery for a schedule requiring attention", async () => {
    const setPaused = vi.fn(async () => true);
    const { target } = mountPanel({
      ...config,
      schedules: [{
        ...schedule,
        effectiveState: "attention_required",
        lastOutcome: "attention_required",
      }],
    }, schedulerActions({ setPaused }));
    await tick();

    const resume = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Resume");
    expect(resume).toBeDefined();
    resume!.click();
    await vi.waitFor(() => expect(setPaused).toHaveBeenCalledOnce());
    expect(setPaused).toHaveBeenCalledWith("schedule-0123456789abcdef01234567", false);
  });

  it("renders the last occurrence and outcome together", () => {
    const lastOccurrenceAt = "2026-08-23T09:00:00.123456789Z";
    const { target } = mountPanel({
      ...config,
      schedules: [{
        ...schedule,
        lastOccurrenceAt,
        lastOutcome: "completed",
      }],
    });

    const details = Object.fromEntries(Array.from(target.querySelectorAll("dl > div"), (row) => [
      row.querySelector("dt")?.textContent,
      row.querySelector("dd")?.textContent,
    ]));
    expect(details["Last occurrence"]).toBe(lastOccurrenceAt);
    expect(details["Last outcome"]).toBe("completed");
  });

  it("omits the last occurrence row when no occurrence is recorded", () => {
    const { target } = mountPanel({
      ...config,
      schedules: [{ ...schedule, lastOutcome: "pending" }],
    });

    const labels = Array.from(target.querySelectorAll("dt"), (label) => label.textContent);
    expect(labels).not.toContain("Last occurrence");
    expect(labels).toContain("Last outcome");
  });

  it("keeps a schedule row pending while its callback is unresolved", async () => {
    let resolve!: (completed: boolean) => void;
    const setPaused = vi.fn(() => new Promise<boolean>((done) => { resolve = done; }));
    const { target } = mountPanel({ ...config, schedules: [schedule] }, schedulerActions({ setPaused }));
    await tick();

    const pause = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Pause")!;
    pause.click();
    await vi.waitFor(() => expect(pause.disabled).toBe(true));
    pause.click();
    expect(setPaused).toHaveBeenCalledOnce();

    resolve(true);
    await vi.waitFor(() => expect(pause.disabled).toBe(false));
  });
});
