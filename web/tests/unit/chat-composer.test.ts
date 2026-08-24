import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ChatComposer from "../../src/components/ChatComposer.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import type { ComposerModel } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function model(overrides: Partial<ComposerModel> = {}): ComposerModel {
  return {
    identity: "workspace-a:task-a:draft-a", workspaceId: "workspace-a", resourceId: "task-a",
    draft: "", draftKey: "draft-a", draftResetVersion: 0,
    unavailableReason: "", sending: false, canEndTurn: false, endingTurn: false, canEndGeneration: true, endingGeneration: false, stopNotice: "",
    waitingMessages: [], canSteerWaiting: false, steeringMessageId: "", onDraft: vi.fn(),
    onSend: vi.fn(async () => ({ accepted: true, clear: true })), onOpenUpload: vi.fn(), onEndTurn: vi.fn(), onEndGeneration: vi.fn(), onDismissStopNotice: vi.fn(),
    onSteerWaiting: vi.fn(async () => undefined),
    agentBinding: { kind: "profile", name: "default" },
    agentProfiles: [{ key: "default", description: "Default", agentName: "fake-agent" }],
    agents: [{ id: "fake-agent", label: "Fake Agent", summary: "fake" }],
    bindingSaving: false, onSaveAgentBinding: vi.fn(async () => undefined),
    ...overrides,
  };
}

describe("ChatComposer", () => {
  it("disables send for empty or whitespace-only drafts while keeping the input editable", async () => {
    const onSend = vi.fn(async () => ({ accepted: true, clear: true }));
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    const send = target.querySelector<HTMLButtonElement>(".chat-send-button")!;
    expect(input.disabled).toBe(false);
    expect(send.disabled).toBe(true);

    input.value = " \n ";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    await tick();
    expect(send.disabled).toBe(true);
    expect(onSend).not.toHaveBeenCalled();

    input.value = "hello";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    await tick();
    expect(send.disabled).toBe(false);
  });

  it("does not overwrite input entered before the mount subscription settles", async () => {
    const channel = createModelChannel(model());
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "typed immediately";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    await tick();
    expect(input.value).toBe("typed immediately");
  });

  it("uses the draft's actual newlines to choose the Enter behavior", async () => {
    const onSend = vi.fn(async (_text: string) => ({ accepted: true, clear: true }));
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "first line\nsecond line";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    const multilineEnter = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
    input.dispatchEvent(multilineEnter);
    expect(multilineEnter.defaultPrevented).toBe(false);
    expect(onSend).not.toHaveBeenCalled();

    input.value = "back to one line";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    const singleLineEnter = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
    input.dispatchEvent(singleLineEnter);
    expect(singleLineEnter.defaultPrevented).toBe(true);
    await vi.waitFor(() => expect(onSend).toHaveBeenCalledWith("back to one line", expect.objectContaining({ resourceId: "task-a" })));
  });

  it("returns to single-line Enter sending after a multiline message is accepted", async () => {
    const onSend = vi.fn(async (_text: string) => ({ accepted: true, clear: true }));
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "first line\nsecond line";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", ctrlKey: true, bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(input.value).toBe(""));

    input.value = "next message";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    const enter = new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true });
    input.dispatchEvent(enter);
    expect(enter.defaultPrevented).toBe(true);
    await vi.waitFor(() => expect(onSend).toHaveBeenCalledTimes(2));
    expect(onSend.mock.calls.map(([text]) => text)).toEqual(["first line\nsecond line", "next message"]);
  });

  it("does not send Enter while an input method is composing", async () => {
    const onSend = vi.fn(async () => ({ accepted: true, clear: true }));
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "composing";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    const enter = new KeyboardEvent("keydown", { key: "Enter", isComposing: true, bubbles: true, cancelable: true });
    input.dispatchEvent(enter);
    expect(enter.defaultPrevented).toBe(false);
    expect(onSend).not.toHaveBeenCalled();
  });

  it("keeps an in-progress resource draft when metadata is republished", async () => {
    const channel = createModelChannel(model());
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "typed while runs load";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    channel.publish(model({ unavailableReason: "" }));
    await tick();

    expect(input.value).toBe("typed while runs load");
  });

  it("does not let a late accepted send clear a different resource draft", async () => {
    const result = deferred<{ accepted: boolean; clear: boolean }>();
    const first = model({ onSend: vi.fn(() => result.promise) });
    const channel = createModelChannel(first);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "message for run a";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    target.querySelector<HTMLFormElement>("#chatForm")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));
    await tick();

    channel.publish(model({ identity: "workspace-a:task-b:draft-b", resourceId: "task-b", draftKey: "draft-b", draft: "draft for task b" }));
    await tick();
    result.resolve({ accepted: true, clear: true });
    await result.promise;
    await tick();

    expect(target.querySelector<HTMLTextAreaElement>("#chatInput")?.value).toBe("draft for task b");
  });

  it("keeps failed text and offers an explicit retry", async () => {
    const onSend = vi.fn().mockRejectedValueOnce(new Error("temporary failure")).mockResolvedValueOnce({ accepted: true, clear: true });
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "retry me";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    target.querySelector<HTMLFormElement>("#chatForm")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(target.querySelector("[role=alert]")?.textContent).toContain("temporary failure"));
    expect(input.value).toBe("retry me");
    expect(target.querySelector("[data-send-state=\"submitting\"]")).toBeNull();

    target.querySelector<HTMLButtonElement>("[role=alert] button")!.click();
    await vi.waitFor(() => expect(onSend).toHaveBeenCalledTimes(2));
    await tick();
    expect(input.value).toBe("");
  });

  it("shows immediate pending feedback for a slow mouse send", async () => {
    const result = deferred<{ accepted: boolean; clear: boolean }>();
    const onSend = vi.fn(() => result.promise);
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "slow mouse message";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    target.querySelector<HTMLFormElement>("#chatForm")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));
    await tick();

    const feedback = target.querySelector<HTMLElement>(".chat-send-feedback")!;
    expect(feedback.dataset.sendState).toBe("submitting");
    expect(feedback.getAttribute("role")).toBe("status");
    expect(feedback.textContent).toContain("Submitting");
    expect(feedback.textContent).toContain("slow mouse message");
    expect(input.value).toBe("slow mouse message");
    expect(onSend).toHaveBeenCalledWith("slow mouse message", expect.objectContaining({ resourceId: "task-a" }));

    result.resolve({ accepted: true, clear: true });
    await result.promise;
    await tick();
    expect(target.querySelector(".chat-send-feedback")).toBeNull();
    expect(input.value).toBe("");
  });

  it("shows the same pending feedback for keyboard send", async () => {
    const result = deferred<{ accepted: boolean; clear: boolean }>();
    const onSend = vi.fn(() => result.promise);
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "keyboard message";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", metaKey: true, bubbles: true, cancelable: true }));
    await tick();

    expect(onSend).toHaveBeenCalledTimes(1);
    expect(target.querySelector(".chat-send-feedback")?.textContent).toContain("keyboard message");
    expect(target.querySelector<HTMLButtonElement>(".chat-send-button")?.disabled).toBe(true);

    result.resolve({ accepted: true, clear: true });
    await result.promise;
    await tick();
    expect(target.querySelector(".chat-send-feedback")).toBeNull();
  });

  it("recovers the draft when the send is rejected without an exception", async () => {
    const onSend = vi.fn(async () => ({ accepted: false, clear: false }));
    const channel = createModelChannel(model({ onSend }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "keep this after rejection";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    target.querySelector<HTMLFormElement>("#chatForm")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));

    await vi.waitFor(() => expect(target.querySelector("[role=alert]")?.textContent).toContain("Message was not accepted"));
    expect(input.value).toBe("keep this after rejection");
    expect(target.querySelector(".chat-send-feedback")).toBeNull();
  });

  it("shows waiting messages above the input and steers the same message id", async () => {
    const onSteerWaiting = vi.fn(async () => undefined);
    const channel = createModelChannel(model({
      canSteerWaiting: true,
      waitingMessages: [{ messageId: "msg-waiting", text: "Please check the failing test", status: "waiting", acceptedAt: "2026-08-12T12:00:00Z", requestedMode: "enqueue", actualMode: "enqueue", canPromote: true }],
      onSteerWaiting,
    }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const queue = target.querySelector<HTMLElement>(".chat-message-queue")!;
    expect(queue.textContent).toContain("Please check the failing test");
    expect(queue.compareDocumentPosition(target.querySelector("#chatForm")!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    queue.querySelector<HTMLButtonElement>(".chat-message-steer")!.click();
    await vi.waitFor(() => expect(onSteerWaiting).toHaveBeenCalledWith("msg-waiting"));
  });

  it("keeps insert disabled when the current turn cannot steer", async () => {
    const channel = createModelChannel(model({
      waitingMessages: [{ messageId: "msg-waiting", text: "Wait here", status: "waiting", acceptedAt: "", requestedMode: "enqueue", actualMode: "enqueue", canPromote: true }],
    }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector<HTMLButtonElement>(".chat-message-steer")?.disabled).toBe(true);
  });

  it("keeps generated waiting messages non-promotable during a steer-capable turn", async () => {
    const onSteerWaiting = vi.fn(async () => undefined);
    const channel = createModelChannel(model({
      canSteerWaiting: true,
      waitingMessages: [{ messageId: "msg-occurrence", text: "Run scheduled work", status: "waiting", acceptedAt: "", requestedMode: "enqueue", actualMode: "enqueue", canPromote: false }],
      onSteerWaiting,
    }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const button = target.querySelector<HTMLButtonElement>(".chat-message-steer")!;
    expect(button.disabled).toBe(true);
    expect(button.title).toContain("generated message");
    button.click();
    expect(onSteerWaiting).not.toHaveBeenCalled();
  });

  it("shows the stop policy notice and lets the user dismiss it", async () => {
    const onDismissStopNotice = vi.fn();
    const channel = createModelChannel(model({
      stopNotice: "Turn stopped. 1 pending steer was cancelled and will not affect the next turn.",
      onDismissStopNotice,
    }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector<HTMLElement>('[role="status"]')?.textContent).toContain("will not affect the next turn");
    target.querySelector<HTMLButtonElement>('[aria-label="Dismiss turn stop notice"]')!.click();
    expect(onDismissStopNotice).toHaveBeenCalledTimes(1);
  });

  it("renders the current agent binding in the composer options bar", async () => {
    const channel = createModelChannel(model());
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const bar = target.querySelector<HTMLElement>(".chat-composer-bar")!;
    const selector = bar.querySelector<HTMLButtonElement>('[aria-label="Binding target"]')!;
    expect(selector).not.toBeNull();
    expect(selector.textContent).toContain("default (current: Fake Agent)");
    expect(selector.disabled).toBe(false);
    expect(bar.querySelector("#agentUploadButton")).not.toBeNull();
  });

  it("saves a new agent binding as soon as the selection changes", async () => {
    const onSaveAgentBinding = vi.fn(async () => undefined);
    const channel = createModelChannel(model({
      agentProfiles: [
        { key: "default", description: "Default", agentName: "fake-agent" },
        { key: "fast", description: "Fast", agentName: "fake-agent" },
      ],
      agents: [{ id: "fake-agent", label: "Fake Agent", summary: "fake" }, { id: "other-agent", label: "Other Agent", summary: "other" }],
      onSaveAgentBinding,
    }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const selector = target.querySelector<HTMLButtonElement>('[aria-label="Binding target"]')!;
    selector.click();
    await tick();
    target.querySelector<HTMLButtonElement>('[data-binding="agent:other-agent"]')!.click();
    await vi.waitFor(() => expect(onSaveAgentBinding).toHaveBeenCalledWith({ kind: "agent", name: "other-agent" }));
  });

  it("keeps the selector controlled until the saved binding is published", async () => {
    const channel = createModelChannel(model({
      agents: [{ id: "other-agent", label: "Other Agent", summary: "other" }],
      onSaveAgentBinding: vi.fn(async () => undefined),
    }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const selector = target.querySelector<HTMLButtonElement>('[aria-label="Binding target"]')!;
    selector.click();
    await tick();
    target.querySelector<HTMLButtonElement>('[data-binding="agent:other-agent"]')!.click();
    await tick();
    expect(selector.textContent).toContain("default (current: fake-agent)");

    channel.publish(model({ agentBinding: { kind: "agent", name: "other-agent" }, agents: [{ id: "other-agent", label: "Other Agent", summary: "other" }] }));
    await tick();
    expect(selector.textContent).toContain("Other Agent");
  });

  it("disables the binding selector while a binding is being saved", async () => {
    const channel = createModelChannel(model({ bindingSaving: true }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector<HTMLButtonElement>('[aria-label="Binding target"]')?.disabled).toBe(true);
  });

  it("shows end-generation only when end-turn is absent", async () => {
    const onEndGeneration = vi.fn();
    const channel = createModelChannel(model({ onEndGeneration }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector("#agentEndTurnButton")).toBeNull();
    const endGeneration = target.querySelector<HTMLButtonElement>("#agentEndGenerationButton")!;
    expect(endGeneration).not.toBeNull();
    endGeneration.click();
    expect(onEndGeneration).toHaveBeenCalledTimes(1);

    channel.publish(model({ canEndTurn: true, canEndGeneration: true }));
    await tick();
    expect(target.querySelector("#agentEndTurnButton")).not.toBeNull();
    expect(target.querySelector("#agentEndGenerationButton")).toBeNull();
  });

  it("disables end-generation while retirement is in progress", async () => {
    const channel = createModelChannel(model({ endingGeneration: true }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const button = target.querySelector<HTMLButtonElement>("#agentEndGenerationButton")!;
    expect(button.disabled).toBe(true);
    expect(button.classList.contains("busy")).toBe(true);
    expect(button.querySelector('[data-lucide="archive"]')).not.toBeNull();
    expect(button.querySelector('[data-lucide="loader-circle"]')).not.toBeNull();
  });

  it("keeps send and end-turn icons static and toggles busy state through classes", async () => {
    const channel = createModelChannel(model({ canEndTurn: true }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const send = target.querySelector<HTMLButtonElement>(".chat-send-button")!;
    const endTurn = target.querySelector<HTMLButtonElement>("#agentEndTurnButton")!;
    // Idle and busy icons are both rendered statically so the lucide
    // createIcons replacement never needs a later name update.
    expect(send.querySelector('[data-lucide="send"]')).not.toBeNull();
    expect(send.querySelector('[data-lucide="loader-circle"]')).not.toBeNull();
    expect(endTurn.querySelector('[data-lucide="pause"]')).not.toBeNull();
    expect(endTurn.querySelector('[data-lucide="loader-circle"]')).not.toBeNull();
    expect(send.classList.contains("busy")).toBe(false);
    expect(endTurn.classList.contains("busy")).toBe(false);
  });

  it("switches the send button to its busy class while a send is in flight", async () => {
    const result = deferred<{ accepted: boolean; clear: boolean }>();
    const channel = createModelChannel(model({ onSend: vi.fn(() => result.promise) }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ChatComposer, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
    input.value = "send me";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    target.querySelector<HTMLFormElement>("#chatForm")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));
    await tick();

    const send = target.querySelector<HTMLButtonElement>(".chat-send-button")!;
    expect(send.classList.contains("busy")).toBe(true);
    expect(send.disabled).toBe(true);

    result.resolve({ accepted: true, clear: true });
    await result.promise;
    await tick();
    expect(send.classList.contains("busy")).toBe(false);
  });
});
