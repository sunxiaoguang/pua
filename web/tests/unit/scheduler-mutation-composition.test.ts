import { afterEach, describe, expect, it, vi } from "vitest";

import type { AppShellModel } from "../../src/models/shell";
import type { DetailPanelModel } from "../../src/models/detail";

function json(value: unknown, status = 200): Response {
	return {
		ok: status >= 200 && status < 300,
		status,
		statusText: status === 200 ? "OK" : "Error",
		headers: new Headers({ "content-type": "application/json" }),
		json: async () => value,
	} as unknown as Response;
}

const scheduler = {
	id: "scheduler",
	type: "scheduler",
	title: "Scheduler",
	path: "scheduler",
	archived: false,
	agentBinding: { kind: "profile", name: "default" },
};

const schedulerDetail = {
	...scheduler,
	createdAt: "2026-01-01T00:00:00Z",
	updatedAt: "2026-01-01T00:00:00Z",
	artifacts: [],
	files: [],
	scheduler: {
		schemaVersion: 1,
		agentBinding: { kind: "profile", name: "default" },
		schedules: [],
	},
};

describe("Scheduler mutation app composition", () => {
	let stopPUAApp: (() => void) | null = null;

	afterEach(() => {
		stopPUAApp?.();
		stopPUAApp = null;
		document.body.replaceChildren();
		vi.restoreAllMocks();
		vi.unstubAllGlobals();
	});

	it("refreshes Tree and detail before publishing and toasting a successful mutation", async () => {
		window.history.replaceState({}, "", "/");
		vi.stubGlobal("matchMedia", (query: string) => ({
			matches: false,
			media: query,
			onchange: null,
			addListener: vi.fn(),
			removeListener: vi.fn(),
			addEventListener: vi.fn(),
			removeEventListener: vi.fn(),
			dispatchEvent: () => false,
		}));

		const events: string[] = [];
		const tree = {
			agentBinding: { kind: "profile", name: "default" },
			scheduler,
			projects: [],
			activity: { running: [], unread: [], problems: [] },
			wiki: { exists: false },
		};
		vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
			const url = new URL(String(input), window.location.origin);
			const method = init?.method || "GET";
			if (url.pathname === "/api/workspaces" && method === "GET") {
				return json({ activeId: "ws-test", workspaces: [{ id: "ws-test", instanceId: "instance-test", name: "Test workspace", path: "/tmp/ws-test" }], agents: [], agentProfiles: [] });
			}
			if (url.pathname === "/api/settings/agenthub" && method === "GET") {
				return json({ connected: false, compatible: false, catalog: { providers: [], agents: [] }, config: { agentProfiles: [] } });
			}
			if (url.pathname === "/api/workspaces/ws-test/users") return json({ users: [{ version: 1, name: "User", preference: "" }] });
			if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "GET") return json({});
			if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "PUT") return json({});
			if (url.pathname === "/api/workspaces/ws-test/tree" && method === "GET") {
				events.push("tree");
				return json(tree);
			}
			if (url.pathname === "/api/workspaces/ws-test/resources/scheduler" && method === "GET") {
				events.push("detail");
				return json(schedulerDetail);
			}
			if (url.pathname === "/api/workspaces/ws-test/resources/scheduler/status" && method === "GET") {
				return json({ acceptsMessages: true, waitingMessages: [], canSteerWaiting: false, session: { state: "idle" } });
			}
			if (url.pathname === "/api/workspaces/ws-test/users/User/messages" && method === "GET") return json({ messages: [] });
			if (url.pathname === "/api/workspaces/ws-test/scheduler" && method === "POST") {
				events.push("mutation");
				return json({});
			}
			throw new Error(`Unexpected ${method} ${url.pathname}${url.search}`);
		}));

		const appShellModels: AppShellModel[] = [];
		const detailModels: DetailPanelModel[] = [];
		const publisher = {
			renderAppShell: vi.fn((model: AppShellModel) => { appShellModels.push(model); }),
			renderCreateDialog: vi.fn(),
			renderSettings: vi.fn(),
			renderUploadDialog: vi.fn(),
			renderComposer: vi.fn(),
			renderEventTimeline: vi.fn(),
			renderAgentPanelHeader: vi.fn(),
			renderDetailPanel: vi.fn((model: DetailPanelModel) => {
				detailModels.push(model);
				if (events.length) events.push("publish-detail");
			}),
			renderToast: vi.fn(({ message }: { message: string }) => events.push(`toast:${message}`)),
		};
		const controller = await import("../../src/app-controller");
		stopPUAApp = controller.stopPUAApp;
		controller.startPUAApp(publisher);
		await vi.waitFor(() => expect(appShellModels.at(-1)?.loading).toBe(false));

		await appShellModels.at(-1)!.onSelectResource("scheduler");
		await vi.waitFor(() => expect(detailModels.at(-1)?.detail?.id).toBe("scheduler"));
		events.length = 0;

		await expect(detailModels.at(-1)!.schedulerActions!.save({
			description: "Review release",
			condition: "when green",
			target: "workspace",
		})).resolves.toBe(true);

		expect(events.filter((event) => ["mutation", "tree", "detail"].includes(event))).toEqual([
			"mutation",
			"tree",
			"detail",
			"detail",
		]);
		const finalDetail = events.lastIndexOf("detail");
		const finalPublish = events.lastIndexOf("publish-detail");
		const successToast = events.lastIndexOf("toast:Schedule request sent.");
		expect(finalPublish).toBeGreaterThan(finalDetail);
		expect(successToast).toBeGreaterThan(finalPublish);
	});
});
