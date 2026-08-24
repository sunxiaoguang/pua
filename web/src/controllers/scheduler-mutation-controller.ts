import { RequestCoordinator, StaleResponseError } from "../api/client";
import type { SchedulerSaveInput } from "../models/detail";
import { isSchedulerRevision } from "../models/workspace";

export interface SchedulerMutationContext {
	workspaceId: string;
	navigationVersion: number;
	selectedId: string;
}

export interface SchedulerMutationDependencies {
	context(): SchedulerMutationContext;
	isCurrentWorkspace(workspaceId: string, navigationVersion: number): boolean;
	resolveResourceTitle(resourceId: string): string | null;
	request<T>(path: string, init?: RequestInit): Promise<T>;
}

export interface SchedulerMutationLease {
	isCurrent(): boolean;
	release(): void;
}

export interface SchedulerMutationOperations {
	validateTarget(target: string): string;
	save(input: SchedulerSaveInput): Promise<SchedulerMutationLease | null>;
	setPaused(scheduleId: string, paused: boolean): Promise<SchedulerMutationLease | null>;
	remove(scheduleId: string): Promise<SchedulerMutationLease | null>;
}

export function createSchedulerMutationController(dependencies: SchedulerMutationDependencies) {
	const requests = new RequestCoordinator();

	function operations(): SchedulerMutationOperations {
		const identity = dependencies.context();
		return {
			validateTarget: (target) => validateTarget(identity, target),
			save: (input) => save(identity, input),
			setPaused: (scheduleId, paused) => mutate(
				identity,
				`schedule:${scheduleId}`,
				`/api/workspaces/${encodeURIComponent(identity.workspaceId)}/scheduler/${encodeURIComponent(scheduleId)}/${paused ? "pause" : "resume"}`,
				{ method: "POST" }
			),
			remove: (scheduleId) => mutate(
				identity,
				`schedule:${scheduleId}`,
				`/api/workspaces/${encodeURIComponent(identity.workspaceId)}/scheduler/${encodeURIComponent(scheduleId)}`,
				{ method: "DELETE" }
			),
		};
	}

	function validateTarget(identity: SchedulerMutationContext, value: string): string {
		const resourceId = value.trim();
		if (!resourceId) return "Target resource is required.";
		if (!identityIsCurrent(identity)) return "Target must be an open resource in the current Workspace.";
		if (resourceId === "scheduler") return "The Scheduler resource cannot be a schedule target.";
		if (resourceId === "workspace") return "";
		return dependencies.resolveResourceTitle(resourceId)
			? ""
			: "Target must be an open resource in the current Workspace.";
	}

	async function save(identity: SchedulerMutationContext, input: SchedulerSaveInput): Promise<SchedulerMutationLease | null> {
		if (!input.description.trim() || !input.condition.trim() || validateTarget(identity, input.target)) return null;
		const scheduleId = input.scheduleId || "";
		if (scheduleId && !isSchedulerRevision(input.expectedRevision)) return null;
		const body = {
			description: input.description,
			condition: input.condition,
			target: input.target,
			...(scheduleId ? { expectedRevision: input.expectedRevision } : {}),
		};
		return mutate(
			identity,
			"save",
			`/api/workspaces/${encodeURIComponent(identity.workspaceId)}/scheduler${scheduleId ? `/${encodeURIComponent(scheduleId)}` : ""}`,
			{
				method: scheduleId ? "PUT" : "POST",
				body: JSON.stringify(body),
			}
		);
	}

	async function mutate(identity: SchedulerMutationContext, key: string, path: string, init: RequestInit): Promise<SchedulerMutationLease | null> {
		if (!identityIsCurrent(identity)) return null;
		const ticket = requests.begin(`scheduler:${key}`);
		const isCurrent = (): boolean => {
			try {
				requests.assertCurrent(ticket);
				return identityIsCurrent(identity);
			} catch (_) {
				return false;
			}
		};
		try {
			await dependencies.request(path, { ...init, signal: ticket.controller.signal });
			requests.assertCurrent(ticket);
			if (!identityIsCurrent(identity)) throw new StaleResponseError(ticket.scope);
			let released = false;
			return {
				isCurrent: () => !released && isCurrent(),
				release: () => {
					if (released) return;
					released = true;
					requests.finish(ticket);
				},
			};
		} catch (reason) {
			const stale = reason instanceof StaleResponseError || !isCurrent();
			requests.finish(ticket);
			if (stale) return null;
			throw reason;
		}
	}

	function identityIsCurrent(identity: SchedulerMutationContext): boolean {
		const current = dependencies.context();
		return identity.selectedId === "scheduler"
			&& current.selectedId === identity.selectedId
			&& dependencies.isCurrentWorkspace(identity.workspaceId, identity.navigationVersion);
	}

	function invalidate(): void {
		requests.dispose();
	}

	return { operations, invalidate };
}
