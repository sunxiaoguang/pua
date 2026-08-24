<script lang="ts">
  import "./SchedulerPanel.css";

  import { confirmDialog } from "../controllers/confirm-dialog-controller";
  import type { SchedulerMutationCallbacks, SchedulerSaveInput } from "../models/detail";
  import { isSchedulerRevision } from "../models/workspace";
  import type { ScheduleRecord, SchedulerConfigRecord, SchedulerRevision } from "../models/workspace";
  import Icon from "./Icon.svelte";

  let { config, actions }: {
    config: SchedulerConfigRecord;
    actions: SchedulerMutationCallbacks;
  } = $props();
  let editingId = $state("");
  let editingRevision = $state<SchedulerRevision | undefined>();
  let description = $state("");
  let condition = $state("");
  let target = $state("workspace");
  let formGeneration = $state(0);
  let saving = $state(false);
  let pendingScheduleIds = $state(new Set<string>());
  const targetError = $derived(actions.validateTarget(target));

  function edit(schedule: ScheduleRecord): void {
    if (!isSchedulerRevision(schedule.revision)) return;
    formGeneration += 1;
    editingId = schedule.id;
    editingRevision = schedule.revision;
    description = schedule.description;
    condition = schedule.condition;
    target = schedule.target;
  }

  function clearForm(): void {
    formGeneration += 1;
    editingId = "";
    editingRevision = undefined;
    description = "";
    condition = "";
    target = "workspace";
  }

  function markFormChanged(): void {
    formGeneration += 1;
  }

  function setSchedulePending(scheduleId: string, pending: boolean): void {
    const next = new Set(pendingScheduleIds);
    if (pending) next.add(scheduleId); else next.delete(scheduleId);
    pendingScheduleIds = next;
  }

  function triggerLabel(schedule: ScheduleRecord): string {
    const trigger = schedule.trigger;
    if (!trigger) return "Needs compilation";
    if (trigger.type === "at") return `At ${trigger.at}`;
    if (trigger.type === "interval") return `Every ${trigger.everySeconds}s from ${trigger.anchorAt}`;
    return `${trigger.cron} (${trigger.timeZone})`;
  }

  async function saveSchedule(): Promise<void> {
    if (saving || !description.trim() || !condition.trim() || targetError) return;
    const submittedGeneration = formGeneration;
    const fields = {
      description,
      condition,
      target,
    };
    const submission: SchedulerSaveInput = editingId
      ? { ...fields, scheduleId: editingId, expectedRevision: editingRevision! }
      : fields;
    saving = true;
    try {
      const completed = await actions.save(submission);
      if (completed && formGeneration === submittedGeneration) clearForm();
    } finally {
      saving = false;
    }
  }

  async function remove(schedule: ScheduleRecord): Promise<void> {
    if (pendingScheduleIds.has(schedule.id)) return;
    setSchedulePending(schedule.id, true);
    try {
      if (!(await confirmDialog({ title: "Remove schedule", message: `Remove schedule ${schedule.id}?`, confirmLabel: "Remove", danger: true }))) return;
      const completed = await actions.remove(schedule.id);
      if (completed && editingId === schedule.id) clearForm();
    } finally {
      setSchedulePending(schedule.id, false);
    }
  }

  async function setPaused(schedule: ScheduleRecord, paused: boolean): Promise<void> {
    if (pendingScheduleIds.has(schedule.id)) return;
    setSchedulePending(schedule.id, true);
    try {
      await actions.setPaused(schedule.id, paused);
    } finally {
      setSchedulePending(schedule.id, false);
    }
  }
</script>

<div class="schedule-editor" data-component-owner="scheduler-panel">
  <div class="schedule-editor-heading"><div><strong>{editingId ? "Edit schedule" : "Add schedule"}</strong><span>The Scheduler Agent compiles this natural-language request into a native trigger and asks when timing or timezone is ambiguous.</span></div>{#if editingId}<button type="button" class="secondary-button" onclick={clearForm}>Cancel edit</button>{/if}</div>
  <label><span>Description</span><input bind:value={description} oninput={markFormChanged} placeholder="What should the Scheduler understand?" /></label>
  <label><span>Condition</span><textarea bind:value={condition} oninput={markFormChanged} rows="3" placeholder="For example: when the release branch is green after 09:00 Shanghai time"></textarea></label>
  <label><span>Target resource ID</span><input bind:value={target} oninput={markFormChanged} placeholder="workspace, project1, or project1.task1" aria-invalid={Boolean(targetError)} aria-describedby={targetError ? "schedule-target-error" : undefined} />{#if targetError}<small id="schedule-target-error" class="schedule-field-error" role="alert">{targetError}</small>{/if}</label>
  <button type="button" class="primary-button" class:busy={saving} class:editing={Boolean(editingId)} disabled={saving || !description.trim() || !condition.trim() || Boolean(targetError)} onclick={saveSchedule}><span class="schedule-icon schedule-icon-busy"><Icon name="loader-circle" /></span><span class="schedule-icon schedule-icon-editing"><Icon name="save" /></span><span class="schedule-icon schedule-icon-add"><Icon name="plus" /></span><span>{editingId ? "Update schedule" : "Add schedule"}</span></button>
</div>

<div class="schedule-list" data-component-owner="scheduler-panel">
  {#if config.schedules.length}
    {#each config.schedules as schedule (schedule.id)}
      <article class:editing={editingId === schedule.id}>
        <header><div><strong>{schedule.description}</strong><code>{schedule.id} · r{schedule.revision}</code></div><div><button type="button" class="secondary-button" disabled={pendingScheduleIds.has(schedule.id) || !isSchedulerRevision(schedule.revision)} onclick={() => edit(schedule)}><Icon name="pencil" /><span>Edit</span></button>{#if schedule.effectiveState === "paused" || schedule.effectiveState === "attention_required"}<button type="button" class="secondary-button" disabled={pendingScheduleIds.has(schedule.id)} onclick={() => setPaused(schedule, false)}><span>Resume</span></button>{:else if schedule.effectiveState === "active"}<button type="button" class="secondary-button" disabled={pendingScheduleIds.has(schedule.id)} onclick={() => setPaused(schedule, true)}><span>Pause</span></button>{/if}<button type="button" class="danger-button" disabled={pendingScheduleIds.has(schedule.id)} onclick={() => remove(schedule)}><Icon name="trash-2" /><span>Remove</span></button></div></header>
        <dl><div><dt>Trigger</dt><dd>{triggerLabel(schedule)}</dd></div><div><dt>Condition</dt><dd>{schedule.condition}</dd></div>{#if schedule.guard}<div><dt>Guard</dt><dd>{schedule.guard}</dd></div>{/if}<div><dt>Target</dt><dd><code>{schedule.target}</code></dd></div><div><dt>State</dt><dd>{schedule.effectiveState}</dd></div>{#if schedule.nextRunAt}<div><dt>Next run</dt><dd>{schedule.nextRunAt}</dd></div>{/if}{#if schedule.lastOccurrenceAt}<div><dt>Last occurrence</dt><dd>{schedule.lastOccurrenceAt}</dd></div>{/if}{#if schedule.lastOutcome}<div><dt>Last outcome</dt><dd>{schedule.lastOutcome}</dd></div>{/if}{#if schedule.lastError}<div><dt>Error</dt><dd>{schedule.lastError}</dd></div>{/if}</dl>
      </article>
    {/each}
  {:else}
    <div class="empty-list-row"><Icon name="calendar-clock" /><span>No schedules. Native Scheduler timing is idle.</span></div>
  {/if}
</div>
