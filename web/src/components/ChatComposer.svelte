<script lang="ts">
  import "./ChatComposer.css";

  import { onMount, tick } from "svelte";

  import AgentBindingSelector from "./AgentBindingSelector.svelte";
  import type { ModelChannel } from "./model-channel";
  import Icon from "./Icon.svelte";
  import type { ComposerModel } from "./models";
  import type { ResourceAgentBindingModel } from "../models/detail";

  let { channel }: { channel: ModelChannel<ComposerModel> } = $props();
  // svelte-ignore state_referenced_locally
  const initialModel = channel.current();
  let model = $state(initialModel);
  let identity = $state(initialModel.identity);
  let resetVersion = $state(initialModel.draftResetVersion);
  let draft = $state(initialModel.draft);
  let sending = $state(false);
  let pendingText = $state("");
  let error = $state("");
  let queueError = $state("");
  let input: HTMLTextAreaElement | undefined = $state();

  const blocked = $derived(Boolean(model.unavailableReason) || sending || model.sending);
  const sendDisabled = $derived(blocked || !draft.trim());

  onMount(() => channel.subscribe((next) => {
    const previous = model;
    model = next;
    if (next.identity !== identity) {
      identity = next.identity;
      resetVersion = next.draftResetVersion;
      draft = next.draft;
      sending = false;
      pendingText = "";
      error = "";
      queueError = "";
    } else if (next.draftResetVersion !== resetVersion) {
      resetVersion = next.draftResetVersion;
      draft = next.draft;
      error = "";
    }
  }));

  $effect(() => {
    draft;
    void tick().then(resize);
  });

  function context() {
    return { workspaceId: model.workspaceId, resourceId: model.resourceId, draftKey: model.draftKey };
  }

  function updateDraft(value: string): void {
    draft = value;
    error = "";
    model.onDraft(value, context());
  }

  async function send(event?: SubmitEvent): Promise<void> {
    event?.preventDefault();
    const text = draft;
    if (blocked || !text.trim() || !model.workspaceId || !model.resourceId) return;
    const requestIdentity = identity;
    const requestContext = context();
    sending = true;
    pendingText = text;
    error = "";
    try {
      const result = await model.onSend(text, requestContext);
      if (identity !== requestIdentity) return;
      if (!result.accepted) {
        pendingText = "";
        error = "Message was not accepted. Please try again.";
        return;
      }
      if (result.clear && draft === text) updateDraft("");
      pendingText = "";
    } catch (reason) {
      if (identity === requestIdentity) {
        pendingText = "";
        error = reason instanceof Error ? reason.message : String(reason);
      }
    } finally {
      if (identity === requestIdentity) {
        sending = false;
        await tick();
        input?.focus({ preventScroll: true });
      }
    }
  }

  async function steerWaiting(messageId: string, canPromote: boolean): Promise<void> {
    if (!canPromote || !model.canSteerWaiting || model.steeringMessageId) return;
    queueError = "";
    try {
      await model.onSteerWaiting(messageId);
    } catch (reason) {
      queueError = reason instanceof Error ? reason.message : String(reason);
    }
  }

  function keydown(event: KeyboardEvent): void {
    if (event.key !== "Enter" || event.isComposing || event.keyCode === 229) return;
    if (event.metaKey || event.ctrlKey) {
      event.preventDefault();
      void send();
      return;
    }
    if (event.shiftKey) return;
    if ((event.currentTarget as HTMLTextAreaElement | null)?.value.includes("\n")) return;
    event.preventDefault();
    void send();
  }

  function resize(): void {
    if (!input) return;
    input.style.height = "auto";
    const nextHeight = Math.min(input.scrollHeight, 160);
    input.style.height = `${nextHeight}px`;
    input.style.overflowY = input.scrollHeight > 160 ? "auto" : "hidden";
  }

  function selectBinding(binding: ResourceAgentBindingModel): void {
    void model.onSaveAgentBinding(binding);
  }
</script>

{#if model.stopNotice}
  <div class="chat-turn-stop-notice" role="status">
    <span>{model.stopNotice}</span>
    <button type="button" class="chat-turn-stop-dismiss" aria-label="Dismiss turn stop notice" onclick={model.onDismissStopNotice}>Dismiss</button>
  </div>
{/if}
{#if model.waitingMessages.length}
  <section class="chat-message-queue" aria-label="Waiting messages">
    <div class="chat-message-queue-header"><span>Waiting messages</span><span class="chat-message-count">{model.waitingMessages.length}</span></div>
    <div class="chat-message-list">
      {#each model.waitingMessages as message (message.messageId)}
        <div class="chat-message-item" data-message-id={message.messageId}>
          <span class="chat-message-text" title={message.text}>{message.text}</span>
          <span class="chat-message-mode">{message.actualMode || message.requestedMode}</span>
          <button type="button" class="chat-message-steer" disabled={!message.canPromote || !model.canSteerWaiting || Boolean(model.steeringMessageId)} title={!message.canPromote ? "This generated message cannot be inserted into another turn" : model.canSteerWaiting ? "Insert this waiting message into the current turn" : "Available when the current turn supports steer"} aria-label={`Insert waiting message into current turn: ${message.text}`} onclick={() => steerWaiting(message.messageId, message.canPromote)}>
            {#if model.steeringMessageId === message.messageId}<Icon name="loader-circle" />{:else}<Icon name="corner-up-left" />{/if}
            <span>Insert now</span>
          </button>
        </div>
      {/each}
    </div>
    {#if queueError}<div class="chat-message-queue-error" role="alert">{queueError}</div>{/if}
  </section>
{/if}
{#if pendingText}
  <div class="chat-send-feedback" data-send-state="submitting" role="status" aria-live="polite">
    <Icon name="loader-circle" />
    <span class="chat-send-feedback-content"><strong>Submitting</strong><span class="chat-send-feedback-text">{pendingText}</span></span>
  </div>
{/if}
<form id="chatForm" class="chat-input" onsubmit={send}>
    <textarea id="chatInput" bind:this={input} rows="1" autocomplete="off" data-agent-draft-key={model.draftKey} placeholder={model.unavailableReason || "Message this resource"} disabled={blocked} value={draft} oninput={(event) => updateDraft(event.currentTarget.value)} onkeydown={keydown}></textarea>
    <div class="chat-composer-bar">
      <button type="button" id="agentUploadButton" class="chat-upload-button" title="Upload files" aria-label="Upload files" disabled={Boolean(model.unavailableReason)} onclick={model.onOpenUpload}><Icon name="plus" /></button>
      <div class="chat-composer-options">
        <span class="chat-agent-binding"><AgentBindingSelector value={model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={blocked || model.bindingSaving} ariaLabel="Binding target" onSelect={selectBinding} /></span>
        {#if model.canEndTurn}
          <button type="button" id="agentEndTurnButton" class="chat-composer-action chat-end-turn-button" class:busy={model.endingTurn} disabled={model.endingTurn} title="End current turn" aria-label="End current turn" onclick={model.onEndTurn}><span class="chat-composer-icon chat-composer-icon-idle"><Icon name="pause" /></span><span class="chat-composer-icon chat-composer-icon-busy"><Icon name="loader-circle" /></span></button>
        {:else if model.canEndGeneration}
          <button type="button" id="agentEndGenerationButton" class="chat-composer-action chat-end-generation-button" class:busy={model.endingGeneration} disabled={model.endingGeneration} title="End current generation" aria-label="End current generation" onclick={model.onEndGeneration}><span class="chat-composer-icon chat-composer-icon-idle"><Icon name="archive" /></span><span class="chat-composer-icon chat-composer-icon-busy"><Icon name="loader-circle" /></span></button>
        {/if}
        <button type="submit" class="chat-send-button ghost-button" class:busy={sending} title={sending ? "Sending..." : model.unavailableReason || "Send input"} aria-label={sending ? "Sending..." : model.unavailableReason || "Send input"} disabled={sendDisabled}><span class="chat-composer-icon chat-composer-icon-idle"><Icon name="send" /></span><span class="chat-composer-icon chat-composer-icon-busy"><Icon name="loader-circle" /></span></button>
      </div>
    </div>
  </form>
  {#if error}<div class="chat-composer-error" role="alert"><span>{error}</span><button type="button" class="secondary-button" disabled={sending} onclick={() => send()}>Retry</button></div>{/if}
