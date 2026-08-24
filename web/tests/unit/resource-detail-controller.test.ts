import { describe, expect, it, vi } from "vitest";

import { createResourceDetailController, type ResourceDetailContext, type ResourceDetailRecord } from "../../src/controllers/resource-detail-controller";

describe("ResourceDetailController", () => {
	it("loads a resource detail without History query parameters", async () => {
		const details: Record<string, ResourceDetailRecord> = {};
		const context: ResourceDetailContext = { workspaceId: "alpha", navigationVersion: 1, selectedId: "task1", detailRequestVersion: 0 };
		const request = vi.fn(async <T>(_path: string, _init?: RequestInit) => ({ id: "task1", type: "task", artifacts: [] } as T));
		const controller = createResourceDetailController({
			details,
			context: () => context,
			nextDetailRequestVersion: () => ++context.detailRequestVersion,
			isCurrentWorkspace: () => true,
			request: request as unknown as <T>(path: string, init?: RequestInit) => Promise<T>,
		});

		await controller.load("task1");

		expect(request).toHaveBeenCalledWith("/api/workspaces/alpha/resources/task1");
		expect(details.task1).toMatchObject({ id: "task1" });
	});

	it("rejects a late Resource result after selection changes", async () => {
		const details: Record<string, ResourceDetailRecord> = {};
		let context: ResourceDetailContext = { workspaceId: "alpha", navigationVersion: 1, selectedId: "task1", detailRequestVersion: 0 };
		let resolve!: (detail: ResourceDetailRecord) => void;
		const request = vi.fn(() => new Promise<ResourceDetailRecord>((done) => { resolve = done; }));
		const controller = createResourceDetailController({
			details,
			context: () => context,
			nextDetailRequestVersion: () => ++context.detailRequestVersion,
			isCurrentWorkspace: (workspaceId, navigationVersion) => workspaceId === context.workspaceId && navigationVersion === context.navigationVersion,
			request: async <T>() => await request() as T,
		});

		const pending = controller.load("task1");
		context = { ...context, selectedId: "task2", navigationVersion: 2 };
		resolve({ id: "task1", artifacts: [] });

		expect(await pending).toBeNull();
		expect(details).toEqual({});
	});

});
