<script lang="ts">
  import "./ResourceSettingsPanel.css";

  import type { ResourceAgentBindingModel } from "../models/detail";
  import AgentBindingSelector from "./AgentBindingSelector.svelte";
  import Icon from "./Icon.svelte";
  import type { DetailPanelModel } from "./models";
  import { sanitizeUserNameInput, validateUserName } from "../controllers/user-settings-controller";

  let { model, onOpenTemplate }: { model: DetailPanelModel; onOpenTemplate?: (path: string) => void } = $props();

  let pending = $state("");
  let generationPolicyEnabled = $state(true);
  let generationMaxTurns = $state(20);
  let generationMaxMinutes = $state(120);
  let stallWatchdogEnabled = $state(true);
  let stallWatchdogMinutes = $state(30);
  let nameEditing = $state(false);
  let nameDraft = $state("");
  let nameInput = $state<HTMLInputElement | null>(null);
  let descEditing = $state(false);
  let descDraft = $state("");
  let descInput = $state<HTMLInputElement | null>(null);
  let preferenceDrafts = $state<Record<string, string>>({});
  let newUserName = $state("");
  $effect(() => {
    generationPolicyEnabled = model.generationPolicy.enabled;
    generationMaxTurns = model.generationPolicy.maxTurns;
    generationMaxMinutes = model.generationPolicy.maxAccumulatedTurnMinutes;
  });
  $effect(() => {
    stallWatchdogEnabled = model.stallWatchdogPolicy.enabled;
    stallWatchdogMinutes = model.stallWatchdogPolicy.timeoutMinutes;
  });
  $effect(() => {
    if (nameEditing && nameInput) nameInput.focus();
  });
  $effect(() => {
    if (descEditing && descInput) descInput.focus();
  });

  const taskDefault = $derived<ResourceAgentBindingModel>(model.detail?.taskDefault?.name ? { kind: model.detail.taskDefault.kind, name: model.detail.taskDefault.name } : { kind: "profile", name: "" });
  const description = $derived(model.detail?.description || "");

  async function run(key: string, action: () => Promise<void>): Promise<void> {
    if (pending) return;
    pending = key;
    try {
      await action();
    } catch (reason) {
      model.onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      pending = "";
    }
  }

  function saveOwnBinding(binding: ResourceAgentBindingModel): void {
    void run("binding", () => model.onSaveAgentBinding(binding));
  }

  function saveWorkspaceDefault(kind: "project" | "task", binding: ResourceAgentBindingModel): void {
    const defaults = { ...model.workspaceDefaults, [kind]: binding };
    void run(`default:${kind}`, () => model.onSaveWorkspaceDefaults(defaults));
  }

  function saveUserPreference(name: string, fallback: string): void {
    const preference = preferenceDrafts[name] ?? fallback;
    void run(`user:${name}`, async () => {
      await model.onSaveWorkspaceUserPreference(name, preference);
      delete preferenceDrafts[name];
    });
  }

  function deleteUser(name: string): void {
    void run(`delete-user:${name}`, () => model.onDeleteWorkspaceUser(name));
  }

  function switchUser(name: string): void {
    if (!name || name === model.currentUserName) return;
    void run(`switch-user:${name}`, () => model.onSwitchWorkspaceUser(name));
  }

  function addUser(): void {
    let name: string;
    try { name = validateUserName(newUserName); }
    catch (reason) { model.onToast(reason instanceof Error ? reason.message : String(reason)); return; }
    void run("add-user", async () => {
      await model.onAddWorkspaceUser(name);
      newUserName = "";
    });
  }

  function saveGenerationPolicy(): void {
    if (!Number.isInteger(generationMaxTurns) || generationMaxTurns < 1 || generationMaxTurns > 100000) return;
    if (!Number.isInteger(generationMaxMinutes) || generationMaxMinutes < 1 || generationMaxMinutes > 525600) return;
    void run("generationPolicy", () => model.onSaveGenerationPolicy({
      enabled: generationPolicyEnabled,
      maxTurns: generationMaxTurns,
      maxAccumulatedTurnMinutes: generationMaxMinutes,
    }));
  }

  function saveStallWatchdogPolicy(): void {
    if (!Number.isInteger(stallWatchdogMinutes) || stallWatchdogMinutes < 1 || stallWatchdogMinutes > 525600) return;
    void run("stallWatchdogPolicy", () => model.onSaveStallWatchdogPolicy({
      enabled: stallWatchdogEnabled,
      timeoutMinutes: stallWatchdogMinutes,
    }));
  }

  function saveTaskDefault(binding: ResourceAgentBindingModel): void {
    void run("taskDefault", () => model.onSaveTaskDefault(model.resourceId, binding.name ? binding : null));
  }

  function startNameEdit(): void {
    if (pending) return;
    nameDraft = model.resourceTitle;
    nameEditing = true;
  }

  function cancelNameEdit(): void {
    nameEditing = false;
    nameDraft = "";
  }

  function saveName(): void {
    const title = nameDraft.trim();
    if (!title || title === model.resourceTitle) {
      cancelNameEdit();
      return;
    }
    void run("title", async () => {
      await model.onRenameResource(title);
      nameEditing = false;
      nameDraft = "";
    });
  }

  function nameKeydown(event: KeyboardEvent): void {
    if (event.key === "Enter") {
      event.preventDefault();
      saveName();
    } else if (event.key === "Escape") {
      event.preventDefault();
      cancelNameEdit();
    }
  }

  function startDescEdit(): void {
    if (pending) return;
    descDraft = description;
    descEditing = true;
  }

  function cancelDescEdit(): void {
    descEditing = false;
    descDraft = "";
  }

  function saveDesc(): void {
    const value = descDraft.trim();
    if (value === description.trim()) {
      cancelDescEdit();
      return;
    }
    void run("description", async () => {
      await model.onSaveDescription(value);
      descEditing = false;
      descDraft = "";
    });
  }

  function descKeydown(event: KeyboardEvent): void {
    if (event.key === "Enter") {
      event.preventDefault();
      saveDesc();
    } else if (event.key === "Escape") {
      event.preventDefault();
      cancelDescEdit();
    }
  }

</script>

<div class="resource-settings" data-component-owner="resource-settings-panel">
  {#if model.resourceType === "workspace"}
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>Generation lifecycle</strong>
        <span>Start a fresh Generation after either completed-Turn budget is reached. Active Turns and approvals are never interrupted.</span>
      </div>
      <div class="resource-settings-list">
        <label class="resource-settings-row resource-settings-policy-toggle">
          <span class="resource-settings-row-label"><strong>Automatic rotation</strong><span>Stop, archive, and retire the current Generation at its next safe terminal boundary.</span></span>
          <input type="checkbox" bind:checked={generationPolicyEnabled} disabled={Boolean(pending)} aria-label="Enable automatic Generation rotation" />
        </label>
        <div class="resource-settings-row resource-settings-policy-budgets">
          <div class="resource-settings-row-label"><strong>Budgets</strong><span>Rotate after either limit: completed Turns or accumulated active Turn time. Idle time is excluded.</span></div>
          <div class="resource-settings-policy-controls">
            <label><input type="number" min="1" max="100000" step="1" bind:value={generationMaxTurns} disabled={Boolean(pending)} aria-label="Maximum Turns per Generation" /><span>Turns</span></label>
            <label><input type="number" min="1" max="525600" step="1" bind:value={generationMaxMinutes} disabled={Boolean(pending)} aria-label="Maximum accumulated Turn minutes per Generation" /><span>minutes</span></label>
            <button type="button" class="secondary-button" disabled={Boolean(pending) || !Number.isInteger(generationMaxTurns) || generationMaxTurns < 1 || !Number.isInteger(generationMaxMinutes) || generationMaxMinutes < 1 || (generationPolicyEnabled === model.generationPolicy.enabled && generationMaxTurns === model.generationPolicy.maxTurns && generationMaxMinutes === model.generationPolicy.maxAccumulatedTurnMinutes)} onclick={saveGenerationPolicy}><Icon name="save" /><span>Save</span></button>
          </div>
        </div>
      </div>
    </section>
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>Turn stall watchdog</strong>
        <span>Applies to every resource. Stop and resume the same Session after a running Turn has had no effective activity for the configured time.</span>
      </div>
      <div class="resource-settings-list">
        <label class="resource-settings-row resource-settings-policy-toggle">
          <span class="resource-settings-row-label"><strong>Automatic recovery</strong><span>Approval-waiting Turns are left untouched; a stalled running Turn is stopped once and resumed from the same Session.</span></span>
          <input type="checkbox" bind:checked={stallWatchdogEnabled} disabled={Boolean(pending)} aria-label="Enable Turn stall watchdog" />
        </label>
        <div class="resource-settings-row resource-settings-policy-budgets">
          <div class="resource-settings-row-label"><strong>Timeout</strong><span>Effective activity includes messages, reasoning, tools, approvals, provider errors, and Turn terminal events.</span></div>
          <div class="resource-settings-policy-controls resource-settings-stall-watchdog-controls">
            <label><input type="number" min="1" max="525600" step="1" bind:value={stallWatchdogMinutes} disabled={Boolean(pending)} aria-label="Turn stall watchdog timeout in minutes" /><span>minutes</span></label>
            <button type="button" class="secondary-button" disabled={Boolean(pending) || !Number.isInteger(stallWatchdogMinutes) || stallWatchdogMinutes < 1 || stallWatchdogMinutes > 525600 || (stallWatchdogEnabled === model.stallWatchdogPolicy.enabled && stallWatchdogMinutes === model.stallWatchdogPolicy.timeoutMinutes)} onclick={saveStallWatchdogPolicy}><Icon name="save" /><span>Save</span></button>
          </div>
        </div>
      </div>
    </section>
    <section class="resource-settings-section resource-settings-agent-bindings">
      <div class="resource-settings-section-head">
        <strong>Agent Bindings</strong>
        <span>Which agent profile runs this Workspace and resources created under it.</span>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>Workspace Agent</strong><span>Runs the Workspace Agent itself. Matches the selector in the chat composer.</span></div>
          <AgentBindingSelector value={model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} ariaLabel="Workspace Agent binding" onSelect={saveOwnBinding} />
        </div>
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>New Project default</strong><span>Applied once when a Project is created in this Workspace.</span></div>
          <AgentBindingSelector value={model.workspaceDefaults.project} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} ariaLabel="New Project default binding" onSelect={(value) => saveWorkspaceDefault("project", value)} />
        </div>
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>New Task default</strong><span>Applied once when a Task is created, unless its Project overrides it.</span></div>
          <AgentBindingSelector value={model.workspaceDefaults.task} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} ariaLabel="New Task default binding" onSelect={(value) => saveWorkspaceDefault("task", value)} />
        </div>
      </div>
    </section>
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>Users</strong>
        <span>Workspace-local users and the preferences Agents can query with the CLI.</span>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row resource-settings-user-controls">
          <label class="resource-settings-row-label" for="workspaceCurrentUser"><strong>Current user</strong><span>Controls personal read state, preferences, and inbox in this browser.</span></label>
          <select id="workspaceCurrentUser" value={model.currentUserName} disabled={Boolean(pending)} onchange={(event) => switchUser((event.currentTarget as HTMLSelectElement).value)}>
            {#each model.workspaceUsers as user (user.name)}<option value={user.name}>{user.name}</option>{/each}
          </select>
        </div>
        <form class="resource-settings-row resource-settings-add-user" onsubmit={(event) => { event.preventDefault(); addUser(); }}>
          <label class="resource-settings-row-label" for="workspaceNewUser"><strong>Add user</strong><span>Creates another identity in this Workspace without switching to it.</span></label>
          <div><input id="workspaceNewUser" value={newUserName} placeholder="e.g. Alice" disabled={Boolean(pending)} oninput={(event) => newUserName = sanitizeUserNameInput((event.currentTarget as HTMLInputElement).value)} /><button type="submit" class="secondary-button" disabled={Boolean(pending) || !newUserName}><Icon name="plus" /><span>{pending === "add-user" ? "Adding..." : "Add"}</span></button></div>
        </form>
        {#each model.workspaceUsers as user (user.name)}
          <div class="resource-settings-user-row">
            <div class="resource-settings-user-head">
              <strong>{user.name}</strong>
              <div class="resource-settings-user-actions">
                {#if user.name === model.currentUserName}<span class="resource-settings-current-user">Current</span>{/if}
                <button type="button" class="secondary-button danger" title={user.name === model.currentUserName ? "Switch to another user before deleting this user" : `Delete ${user.name}`} disabled={Boolean(pending) || user.name === model.currentUserName} onclick={() => deleteUser(user.name)}><Icon name="trash-2" /><span>Delete</span></button>
              </div>
            </div>
            <label class="resource-settings-user-preference">
              <span>Preference</span>
              <textarea value={preferenceDrafts[user.name] ?? user.preference} aria-label={`Preference for ${user.name}`} disabled={Boolean(pending)} oninput={(event) => preferenceDrafts[user.name] = (event.currentTarget as HTMLTextAreaElement).value} placeholder="How should Agents address this user or shape their replies?"></textarea>
            </label>
            <button type="button" class="secondary-button resource-settings-user-save" disabled={Boolean(pending)} onclick={() => saveUserPreference(user.name, user.preference)}><Icon name="save" /><span>{pending === `user:${user.name}` ? "Saving..." : "Save preference"}</span></button>
          </div>
        {:else}
          <div class="resource-settings-row"><span class="resource-settings-value-text">No users registered in this Workspace.</span></div>
        {/each}
      </div>
    </section>
  {:else if model.resourceType === "scheduler"}
    <section class="resource-settings-section resource-settings-scheduler-agent">
      <div class="resource-settings-section-head">
        <strong>Agent</strong>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>Scheduler Agent</strong><span>Compiles and manages schedule definitions. Native timing runs in the Server.</span></div>
          <AgentBindingSelector value={model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} ariaLabel="Scheduler Agent binding" onSelect={saveOwnBinding} />
        </div>
      </div>
    </section>
  {:else if model.resourceType === "project"}
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>General</strong>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row resource-settings-name-row" class:editing={nameEditing}>
          {#if nameEditing}
            <div class="resource-settings-row-label">
              <strong>Name</strong>
              <input class="resource-settings-name-input" type="text" bind:this={nameInput} bind:value={nameDraft} aria-label="Project name" disabled={Boolean(pending)} onkeydown={nameKeydown} />
            </div>
            <button type="button" class="secondary-button" disabled={Boolean(pending) || !nameDraft.trim()} onclick={saveName}><Icon name="save" /><span>Save</span></button>
          {:else}
            <div class="resource-settings-row-label"><strong>Name</strong><span>Shown in the sidebar and header.</span></div>
            <div class="resource-settings-row-value">
              <span class="resource-settings-value-text">{model.resourceTitle}</span>
              <button type="button" class="secondary-button" disabled={Boolean(pending) || Boolean(model.detail?.archived)} onclick={startNameEdit}><Icon name="pencil" /><span>Edit</span></button>
            </div>
          {/if}
        </div>
        <div class="resource-settings-row resource-settings-desc-row" class:editing={descEditing}>
          {#if descEditing}
            <div class="resource-settings-row-label">
              <strong>Description</strong>
              <input class="resource-settings-desc-input" type="text" bind:this={descInput} bind:value={descDraft} aria-label="Project description" disabled={Boolean(pending)} onkeydown={descKeydown} />
            </div>
            <button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={saveDesc}><Icon name="save" /><span>Save</span></button>
          {:else}
            <div class="resource-settings-row-label"><strong>Description</strong><span>Short summary of this Project, from project.json.</span></div>
            <div class="resource-settings-row-value">
              {#if description.trim()}
                <span class="resource-settings-value-text">{description}</span>
              {:else}
                <span class="resource-settings-value-text resource-settings-value-empty">No description</span>
              {/if}
              <button type="button" class="secondary-button" disabled={Boolean(pending) || Boolean(model.detail?.archived)} onclick={startDescEdit}><Icon name="pencil" /><span>Edit</span></button>
            </div>
          {/if}
        </div>
      </div>
    </section>
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>Agent Bindings</strong>
        <span>Which agent profile runs this Project and Tasks created under it.</span>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>Project Agent</strong><span>Runs the Project Agent itself. Matches the selector in the chat composer.</span></div>
          <AgentBindingSelector value={model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} ariaLabel="Project Agent binding" onSelect={saveOwnBinding} />
        </div>
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>New Task default</strong><span>Applied once when a Task is created in this Project. Inherit uses the Workspace default.</span></div>
          <AgentBindingSelector value={taskDefault} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} allowInherit={true} inheritLabel="Inherit (Workspace default)" ariaLabel="New Task default binding" onSelect={saveTaskDefault} />
        </div>
      </div>
    </section>
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>Task Templates</strong>
        <span>Templates from templates/*.md, offered when creating a Task in this Project.</span>
      </div>
      <div class="resource-settings-list resource-settings-template-list">
        {#if model.detail?.templates?.length}
          {#each model.detail.templates as template (template.name)}
            <button type="button" class:invalid={!template.valid} class="resource-settings-row template-row" onclick={() => template.path && onOpenTemplate?.(template.path)}><Icon name="file-text" /><span><strong>{template.title || template.name}</strong><small>{template.name} · v{template.schemaVersion || "?"} · {template.valid ? `${(template.fields || []).length} fields` : `invalid${template.errors?.[0]?.message ? `: ${template.errors[0].message}` : ""}`}</small></span><Icon name="chevron-right" /></button>
          {/each}
        {:else}
          <div class="empty-list-row"><Icon name="layout-template" /><span>No task templates in templates/*.md.</span></div>
        {/if}
      </div>
    </section>
  {:else if model.resourceType === "task"}
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>General</strong>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row resource-settings-name-row" class:editing={nameEditing}>
          {#if nameEditing}
            <div class="resource-settings-row-label">
              <strong>Name</strong>
              <input class="resource-settings-name-input" type="text" bind:this={nameInput} bind:value={nameDraft} aria-label="Task name" disabled={Boolean(pending)} onkeydown={nameKeydown} />
            </div>
            <button type="button" class="secondary-button" disabled={Boolean(pending) || !nameDraft.trim()} onclick={saveName}><Icon name="save" /><span>Save</span></button>
          {:else}
            <div class="resource-settings-row-label"><strong>Name</strong><span>Shown in the sidebar and header.</span></div>
            <div class="resource-settings-row-value">
              <span class="resource-settings-value-text">{model.resourceTitle}</span>
              <button type="button" class="secondary-button" disabled={Boolean(pending) || Boolean(model.detail?.archived)} onclick={startNameEdit}><Icon name="pencil" /><span>Edit</span></button>
            </div>
          {/if}
        </div>
        <div class="resource-settings-row resource-settings-desc-row" class:editing={descEditing}>
          {#if descEditing}
            <div class="resource-settings-row-label">
              <strong>Description</strong>
              <input class="resource-settings-desc-input" type="text" bind:this={descInput} bind:value={descDraft} aria-label="Task description" disabled={Boolean(pending)} onkeydown={descKeydown} />
            </div>
            <button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={saveDesc}><Icon name="save" /><span>Save</span></button>
          {:else}
            <div class="resource-settings-row-label"><strong>Description</strong><span>Short summary of this Task, from task.json.</span></div>
            <div class="resource-settings-row-value">
              {#if description.trim()}
                <span class="resource-settings-value-text">{description}</span>
              {:else}
                <span class="resource-settings-value-text resource-settings-value-empty">No description</span>
              {/if}
              <button type="button" class="secondary-button" disabled={Boolean(pending) || Boolean(model.detail?.archived)} onclick={startDescEdit}><Icon name="pencil" /><span>Edit</span></button>
            </div>
          {/if}
        </div>
      </div>
    </section>
    <section class="resource-settings-section">
      <div class="resource-settings-section-head">
        <strong>Agent</strong>
      </div>
      <div class="resource-settings-list">
        <div class="resource-settings-row">
          <div class="resource-settings-row-label"><strong>Task Agent</strong><span>Runs the Task Agent itself. Matches the selector in the chat composer.</span></div>
          <AgentBindingSelector value={model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={Boolean(pending)} openUp={false} ariaLabel="Task Agent binding" onSelect={saveOwnBinding} />
        </div>
      </div>
    </section>
  {/if}
</div>
