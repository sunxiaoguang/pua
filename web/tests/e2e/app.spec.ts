import { expect, test, type Page, type Route } from "@playwright/test";

const now = "2026-08-10T12:00:00Z";

interface Harness {
  inputBodies: Array<Record<string, unknown>>;
  taskBodies: Array<Record<string, unknown>>;
  previewBodies: Array<Record<string, unknown>>;
  settingsBodies: Array<Record<string, unknown>>;
  uploadNames: string[];
  streamRequests: string[];
  treeRequests: number;
  agentsBodies: Array<Record<string, unknown>>;
  uiStateBodies: Array<Record<string, unknown>>;
  steeredMessageIds: string[];
  schedulerBodies: Array<{ method: string; path: string; body?: Record<string, unknown> }>;
  bindingBodies: Array<Record<string, unknown>>;
  resourceStateBodies: Array<{ method: string; path: string; body?: Record<string, unknown> }>;
  markdownBodies: Array<{ path: string; content: string; expectedContentHash: string }>;
  finishTurn: () => void;
  schedulerAgentCreateDefinition: (definition: MockScheduleDefinition) => string;
  schedulerAgentUpdateDefinition: (scheduleId: string, definition: Partial<MockScheduleDefinition>) => void;
}

const templates = [
  {
    name: "feature-a", title: "Feature A", description: "First template", valid: true, taskTitle: "{{ summary }}",
    fields: [{ name: "summary", type: "text", label: "Summary", required: true, hasDefault: false }],
  },
  {
    name: "feature-b", title: "Feature B", description: "Second template", valid: true, taskTitle: "{{ summary }}",
    fields: [{ name: "summary", type: "text", label: "Summary", required: true, hasDefault: false }],
  },
];

const longDetailBody = Array.from({ length: 60 }, (_, index) => `Stable detail paragraph ${index + 1}.`).join("\n\n");

const project = {
  id: "project1",
  type: "project",
  title: "Migration project",
  path: "project1-migration",
  archived: false,
  children: [
    {
      id: "project1.task1",
      type: "task",
      title: "Infrastructure task",
      path: "project1-migration/task1-infrastructure",
      archived: false,
      state: "not_started",
    },
    {
      id: "project1.task2",
      type: "task",
      title: "Follow-up task",
      path: "project1-migration/task2-follow-up",
      archived: false,
      state: "not_started",
    },
  ],
};

type MockProject = typeof project;
type MockTask = MockProject["children"][number];
type MockResource = MockProject | MockTask;
type MockActivityResource = {
  id: string;
  type: string;
  title: string;
  path: string;
  archived: boolean;
  state?: string;
  userState?: { readTurnNumber?: number };
  latestTurnNumber?: number;
  unreadCount?: number;
  runtime?: { activeTurn?: boolean; [key: string]: unknown };
};

const schedulerResource = {
  id: "scheduler",
  type: "scheduler",
  title: "Scheduler",
  path: "scheduler",
  archived: false,
  agentBinding: { kind: "profile", name: "fast" },
};

type MockScheduleDefinition = {
  description: string;
  condition: string;
  target: string;
};

type MockSchedule = MockScheduleDefinition & {
  id: string;
  revision: string;
  state: "active" | "paused" | "completed" | "needs_compilation";
  effectiveState: string;
  trigger?: { type: "at"; at: string };
  nextRunAt?: string;
  lastOutcome?: string;
  createdAt: string;
  updatedAt: string;
};

function incrementMockSchedulerRevision(revision: string): string {
  return (BigInt(revision) + 1n).toString();
}

function resourceDetail(resource: MockResource) {
  const resourceReference = resource.id === "project1.task1" ? "\n\nRelated: [[project1.task2]]." : "";
  return {
    ...resource,
    files: [
      { name: resource.type === "project" ? "project.md" : "task.md", path: `${resource.path}/${resource.type === "project" ? "project.md" : "task.md"}`, content: `# ${resource.title}\n\nBaseline content with a stable selection target.${resourceReference}\n\n${longDetailBody}`, contentHash: `${resource.id}-brief-v1` },
    ],
    artifacts: [{ name: "notes.md", path: `${resource.path}/artifacts/notes.md`, type: "file", size: 24 }],
    repos: resource.type === "task" ? [{ name: "pua", worktreePath: `${resource.path}/worktree/pua`, branch: "topic", targetBranch: "master" }] : [],
    templates: resource?.type === "project" ? templates : [],
  };
}

function detail(id: string) {
  const resource = id === project.id ? project : project.children.find((item) => item.id === id);
  if (!resource) throw new Error(`unknown resource ${id}`);
  return resourceDetail(resource);
}

function historyEvents(generationId: string) {
  return Array.from({ length: 32 }, (_, index) => ({
    id: index + 33,
    time: `2026-08-10T12:${String(index).padStart(2, "0")}:00Z`,
    type: index % 2 === 0 ? "message.input" : "message.assistant.delta",
    sessionId: "agenthub-session-1",
    turnId: `turn-${index}`,
    data: {
      text: `${generationId} baseline message ${index + 1}`,
      ...(index % 2 === 0 ? { role: "user", sender: { name: "Test User" } } : {}),
    },
  }));
}

function historyTurns(generationId: string) {
  return historyEvents(generationId).map((event) => ({
    id: event.turnId, turnId: event.turnId, status: "completed", closed: true,
    startEventId: event.id, endEventId: event.id, firstEventId: event.id, lastEventId: event.id,
    items: [{
      type: "message", role: event.type === "message.input" ? "user" : "assistant",
      sender: event.data.sender, text: event.data.text, startEventId: event.id, endEventId: event.id,
      startedAt: event.time, endedAt: event.time, durationMs: 0, count: 1,
    }],
  }));
}

type ConversationFixture = "default" | "narrow-layout";

const resourceGeneration = {
  generation: 1, generationId: "gen-1", title: "Infrastructure task", status: "idle",
  agentName: "test-agent", createdAt: now, updatedAt: now,
};

function narrowLayoutTurn() {
  const startedAt = "2026-08-10T12:00:00Z";
  const longToken = "narrow-layout-token-".repeat(18);
  const codeLine = "0123456789abcdef".repeat(24);
  const items = [
    { type: "message", role: "user", sender: { name: "Test User" }, text: "An ordinary message must stay inside the narrow chat column.", startEventId: 1, endEventId: 1, startedAt, endedAt: startedAt },
    { type: "message", role: "assistant", text: "## Rich text\n\nThis message has **bold text**, a list, and a safe [link](https://example.com).", startEventId: 2, endEventId: 2, startedAt, endedAt: startedAt },
    { type: "message", role: "assistant", text: `Long token: ${longToken}`, startEventId: 3, endEventId: 3, startedAt, endedAt: startedAt },
    { type: "message", role: "assistant", text: `| Field | Value |\n| --- | --- |\n| token | ${longToken} |`, startEventId: 4, endEventId: 4, startedAt, endedAt: startedAt },
    { type: "message", role: "assistant", text: ["Code block keeps its own local horizontal scroll:", "", "```text", codeLine, "```"].join("\n"), startEventId: 5, endEventId: 5, startedAt, endedAt: startedAt },
    { type: "thinking", count: 1, startEventId: 6, endEventId: 6, startedAt, endedAt: startedAt },
    { type: "tool", count: 2, startEventId: 7, endEventId: 7, startedAt, endedAt: startedAt },
  ];
  return {
    id: "turn-narrow-layout", turnId: "turn-narrow-layout", status: "completed", closed: true,
    startEventId: 1, endEventId: 7, firstEventId: 1, lastEventId: 7, items,
  };
}

function conversationTurns(fixture: ConversationFixture) {
  return fixture === "narrow-layout" ? [narrowLayoutTurn()] : historyTurns("gen-1");
}

function resourceTurnSummaries(fixture: ConversationFixture = "default") {
  return conversationTurns(fixture).map((turn) => ({
    reference: `ref-${turn.turnId}`, turnId: turn.turnId, status: turn.status, closed: turn.closed,
    startedAt: turn.items[0].startedAt, durationMs: 0, triggerPreview: turn.items[0].text,
    eventCount: turn.items.length, toolEventCount: turn.items.filter((item) => item.type === "tool").length, startEventId: turn.startEventId, lastEventId: turn.lastEventId,
    endEventId: turn.endEventId, generation: resourceGeneration,
  }));
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

function mockSystemInfo(workspaces: Array<{ name: string; path: string }>) {
  return {
    pua: {
      address: "127.0.0.1", port: "4936", configPath: "/tmp/pua/serve.json", workspaces,
      buildBranch: "master", buildCommit: "pua-commit",
    },
    agentHub: {
      mode: "embedded", address: "127.0.0.1", port: "4936", endpoint: "http://127.0.0.1:4936/agenthub",
      connected: true, compatible: true, version: "hub-commit", error: "",
      paths: { config: "/tmp/agenthub/config.json", sessions: "/tmp/agenthub/sessions", archive: "/tmp/agenthub/sessions/Archive", logs: "/tmp/agenthub/logs" },
    },
  };
}

async function installMockApi(page: Page, lastResourceId = "project1.task1", withWaitingMessage = false, initialTurnRunning = false, startWithoutRuntime = false, extraAgents: string[] = [], initialIdleStatus: "idle" | "idle-suspended" = "idle", settingsRefreshDelayMs = 0, conversationFixture: ConversationFixture = "default"): Promise<Harness> {
  const harness: Harness = {
    inputBodies: [], taskBodies: [], previewBodies: [], settingsBodies: [], uploadNames: [], streamRequests: [], treeRequests: 0, agentsBodies: [], uiStateBodies: [], steeredMessageIds: [], schedulerBodies: [], bindingBodies: [], resourceStateBodies: [], markdownBodies: [],
    finishTurn: () => undefined,
    schedulerAgentCreateDefinition: () => { throw new Error("Scheduler mock is not initialized."); },
    schedulerAgentUpdateDefinition: () => { throw new Error("Scheduler mock is not initialized."); },
  };
  let waitingMessages = withWaitingMessage ? [{ messageId: "msg-waiting", resourceId: "project1.task1", text: "Review the mailbox change now", status: "waiting", acceptedAt: now, requestedMode: "enqueue", actualMode: "enqueue", canPromote: true }] : [];
  const resourceStates: Record<string, { readTurnNumber?: number }> = {};
  let runtimeExists = !startWithoutRuntime;
  let turnRunning = initialTurnRunning;
  let turnNumber = initialTurnRunning ? 1 : 0;
  let completedTurnNumber = 0;
  harness.finishTurn = () => {
    if (!turnRunning) return;
    turnRunning = false;
    completedTurnNumber = turnNumber;
  };
  let createdProject: MockProject | null = null;
  let createdTask: MockTask | null = null;
  let scheduleSequence = 0;
  let schedulerMessageSequence = 0;
  let savedTaskBrief: { content: string; contentHash: string } | null = null;
  let users = [{ version: 1, name: "User", preference: "" }];
  let schedulerConfig = {
    schemaVersion: 2,
    agentBinding: { kind: "profile" as const, name: "fast" },
    schedules: [] as MockSchedule[],
  };
  harness.schedulerAgentCreateDefinition = (definition) => {
    scheduleSequence += 1;
    const id = `schedule-${String(scheduleSequence).padStart(24, "0")}`;
    schedulerConfig = {
      ...schedulerConfig,
      schedules: [...schedulerConfig.schedules, {
        id,
        revision: "1",
        ...definition,
        state: "active",
        effectiveState: "active",
        trigger: { type: "at", at: "2026-08-24T09:00:00+08:00" },
        nextRunAt: "2026-08-24T09:00:00+08:00",
        createdAt: now,
        updatedAt: now,
      }],
    };
    return id;
  };
  harness.schedulerAgentUpdateDefinition = (scheduleId, definition) => {
    if (!schedulerConfig.schedules.some((schedule) => schedule.id === scheduleId)) {
      throw new Error(`Unknown Scheduler definition ${scheduleId}.`);
    }
    schedulerConfig = {
      ...schedulerConfig,
      schedules: schedulerConfig.schedules.map((schedule) => schedule.id === scheduleId ? {
        ...schedule,
        ...definition,
        revision: incrementMockSchedulerRevision(schedule.revision),
        updatedAt: now,
      } : schedule),
    };
  };
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();

    if (path === "/api/workspaces" && method === "GET") {
      return json(route, {
        version: 3,
        activeId: "ws-test",
        workspaces: [{ id: "ws-test", name: "Isolated E2E", path: "/tmp/pua-e2e" }],
        agentProfiles: [{ key: "default", agentName: "test-agent" }, { key: "fast", agentName: "test-agent" }, { key: "review", agentName: "other-agent" }],
      });
    }
    if (path === "/api/settings/system" && method === "GET") {
      return json(route, mockSystemInfo([{ name: "Isolated E2E", path: "/tmp/pua-e2e" }]));
    }
    if (path === "/api/doctor") {
      if (method === "POST") return route.fulfill({ status: 202 });
      return json(route, { checkedAt: now, checking: false, complete: true, summary: { errors: 0, warnings: 0 }, workspaces: [] });
    }
    if (path === "/api/settings/agenthub") {
      if (method === "PUT") {
        harness.settingsBodies.push(request.postDataJSON());
        return json(route, {});
      }
      if (settingsRefreshDelayMs) await new Promise((resolve) => setTimeout(resolve, settingsRefreshDelayMs));
      return json(route, {
        mode: "embedded",
        configuredEndpoint: "http://127.0.0.1:4936/agenthub",
        config: { agentProfiles: [{ key: "default", agentName: "test-agent" }, { key: "fast", agentName: "test-agent" }, { key: "review", agentName: "other-agent" }] },
        connected: true,
        compatible: true,
        catalog: {
          providers: [{ id: "test", name: "Test Provider", enabled: true }],
          agents: ["test-agent", "other-agent", ...extraAgents].map((name) => ({ name, providerId: "test", available: true })),
          probes: [],
        },
      });
    }
    if (path === "/api/settings" && method === "GET") {
      return json(route, {
        version: 3,
        activeId: "ws-test",
        workspaces: [{ id: "ws-test", name: "Isolated E2E", path: "/tmp/pua-e2e" }],
      });
    }
    if (path === "/api/workspaces/ws-test/users") {
      if (method === "POST") {
        const name = String((request.postDataJSON() as { name?: string }).name || "");
        if (!users.some((user) => user.name === name)) users = [...users, { version: 1, name, preference: "" }];
        return json(route, users.find((user) => user.name === name));
      }
      if (method === "GET") return json(route, { users });
    }
    const userMatch = path.match(/^\/api\/workspaces\/ws-test\/users\/([A-Za-z0-9_-]+)$/);
    if (userMatch && method === "PUT") {
      const name = userMatch[1];
      const preference = String((request.postDataJSON() as { preference?: string }).preference || "");
      users = users.map((user) => user.name === name ? { ...user, preference } : user);
      return json(route, users.find((user) => user.name === name));
    }
    if (userMatch && method === "DELETE") {
      users = users.filter((user) => user.name !== userMatch[1]);
      return route.fulfill({ status: 204 });
    }
    if (path === "/api/workspaces/ws-test/ui-state") {
      if (method === "PUT") {
        harness.uiStateBodies.push(request.postDataJSON());
        return json(route, {});
      }
      return json(route, {
        version: 1,
        expandedProjects: ["project1"],
        lastResourceId,
        projectOrder: [],
        taskOrder: {},
      });
    }
    if (path === "/api/workspaces/ws-test/tree") {
      harness.treeRequests += 1;
      const tasks = [...project.children, ...(createdTask ? [createdTask] : [])];
      const projectSnapshot = {
        ...project,
        userState: resourceStates[project.id],
        children: tasks.map((resource) => resource.id === "project1.task1" ? {
          ...resource,
          state: turnRunning ? "in_progress" : resource.state,
          userState: resourceStates[resource.id],
          latestTurnNumber: completedTurnNumber,
          unreadCount: Math.max(0, completedTurnNumber - Number(resourceStates[resource.id]?.readTurnNumber || 0)),
          ...(runtimeExists ? { runtime: { generation: 1, generationId: "gen-1", status: turnRunning ? "running" : initialIdleStatus, agentName: "test-agent", updatedAt: now, resumable: initialIdleStatus === "idle-suspended", turnNumber, activeTurn: turnRunning } } : {}),
        } : { ...resource, userState: resourceStates[resource.id], latestTurnNumber: 0, unreadCount: 0 }),
      };
      const activityCandidates: MockActivityResource[] = [projectSnapshot, ...projectSnapshot.children].map((item) => ({ ...item, children: undefined }));
      const activity = {
        running: activityCandidates.filter((item) => item.runtime?.activeTurn),
        unread: activityCandidates.filter((item) => Number(item.unreadCount || 0) > 0),
        problems: activityCandidates.filter((item) => item.type === "task" && (item.state === "blocked" || item.state === "error")),
      };
      return json(route, {
        root: "/tmp/pua-e2e",
        workspace: { id: "workspace", type: "workspace", title: "Isolated E2E", path: ".", archived: false, latestTurnNumber: 0, unreadCount: 0 },
        scheduler: { ...schedulerResource, scheduler: schedulerConfig },
        projects: [projectSnapshot, ...(createdProject ? [{ ...createdProject, userState: resourceStates[createdProject.id] }] : [])],
        activity,
        wiki: { exists: true, entries: [
          { name: "index.md", path: "wiki/index.md", type: "file", size: 28 },
          { name: "link-preview.md", path: "wiki/link-preview.md", type: "file", size: 28 },
        ] },
      });
    }
    const readMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)\/read$/);
    if (readMatch && method === "PUT") {
      const resourceId = decodeURIComponent(readMatch[1]);
      const body = request.postDataJSON() as { throughTurnNumber?: number };
      const current = resourceStates[resourceId] || {};
      resourceStates[resourceId] = { ...current, readTurnNumber: Math.max(Number(current.readTurnNumber || 0), Number(body.throughTurnNumber || 0)) };
      harness.resourceStateBodies.push({ method, path, body });
      return json(route, resourceStates[resourceId]);
    }
    if (path === "/api/workspaces/ws-test/scheduler" && method === "GET") {
      return json(route, schedulerConfig);
    }
    if (path === "/api/workspaces/ws-test/scheduler" && method === "POST") {
      const body = request.postDataJSON() as Record<string, unknown>;
      harness.schedulerBodies.push({ method, path, body });
      schedulerMessageSequence += 1;
      return json(route, { messageId: `msg-schedule-${schedulerMessageSequence}`, resourceId: "scheduler", requestedMode: "enqueue", actualMode: "enqueue", status: "waiting" }, 202);
    }
    const scheduleMutation = path.match(/^\/api\/workspaces\/ws-test\/scheduler\/(schedule-[0-9]+)$/);
    if (scheduleMutation && method === "PUT") {
      const body = request.postDataJSON() as Record<string, unknown>;
      harness.schedulerBodies.push({ method, path, body });
      schedulerMessageSequence += 1;
      return json(route, { messageId: `msg-schedule-${schedulerMessageSequence}`, resourceId: "scheduler", requestedMode: "enqueue", actualMode: "enqueue", status: "waiting" }, 202);
    }
    const scheduleStateMutation = path.match(/^\/api\/workspaces\/ws-test\/scheduler\/(schedule-[0-9]+)\/(pause|resume)$/);
    if (scheduleStateMutation && method === "POST") {
      harness.schedulerBodies.push({ method, path });
      const paused = scheduleStateMutation[2] === "pause";
      const index = schedulerConfig.schedules.findIndex((schedule) => schedule.id === scheduleStateMutation[1]);
      schedulerConfig = {
        ...schedulerConfig,
        schedules: schedulerConfig.schedules.map((schedule, scheduleIndex) => scheduleIndex === index ? {
          ...schedule,
          revision: incrementMockSchedulerRevision(schedule.revision),
          state: paused ? "paused" : "active",
          effectiveState: paused ? "paused" : "active",
          nextRunAt: paused ? undefined : schedule.nextRunAt,
          updatedAt: now,
        } : schedule),
      };
      return json(route, schedulerConfig.schedules[index]);
    }
    if (scheduleMutation && method === "DELETE") {
      harness.schedulerBodies.push({ method, path });
      const removed = schedulerConfig.schedules.find((schedule) => schedule.id === scheduleMutation[1]);
      schedulerConfig = { ...schedulerConfig, schedules: schedulerConfig.schedules.filter((schedule) => schedule.id !== scheduleMutation[1]) };
      return json(route, removed);
    }
    const statusMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)\/status$/);
    if (statusMatch && method === "GET") {
      const resourceId = decodeURIComponent(statusMatch[1]);
      const visible = waitingMessages.filter((message) => message.resourceId === resourceId);
      return json(route, {
        resourceId, sessionState: "working", exists: true, archived: false, acceptsMessages: true,
        canSteerWaiting: true, waitingMessages: visible, messages: { waiting: visible.length },
        ...(resourceId === "project1.task1" ? {
          generation: { generation: 1, generationId: "gen-1", status: turnRunning ? "running" : "idle" },
          session: { id: "session-1", state: turnRunning ? "running" : "idle", ...(turnRunning ? { currentTurnId: "turn-stream" } : {}) },
        } : {}),
      });
    }
    const steerMatch = path.match(/^\/api\/workspaces\/ws-test\/messages\/(.+)\/steer$/);
    if (steerMatch && method === "POST") {
      const messageId = decodeURIComponent(steerMatch[1]);
      harness.steeredMessageIds.push(messageId);
      waitingMessages = waitingMessages.filter((message) => message.messageId !== messageId);
      return json(route, { messageId, status: "delivered", actualMode: "steer" });
    }
    const historyTurnDetailMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/project1\.task1\/history\/turns\/(.+)$/);
    if (historyTurnDetailMatch && method === "GET") {
      const reference = decodeURIComponent(historyTurnDetailMatch[1]);
      const summaries = resourceTurnSummaries(conversationFixture);
      const summary = summaries.find((turn) => turn.reference === reference);
      if (!summary) return json(route, { error: "turn not found" }, 404);
      const source = conversationTurns(conversationFixture).find((turn) => turn.turnId === summary.turnId)!;
      return json(route, { turn: summary, items: source.items, latestEventId: 64 });
    }
    if (path === "/api/workspaces/ws-test/resources/project1.task1/history/turns" && method === "GET") {
      if (url.searchParams.has("cursor")) {
        const summary = {
          reference: "ref-turn-older", turnId: "turn-older", status: "completed", closed: true,
          startedAt: now, durationMs: 0, triggerPreview: "gen-1 older history", eventCount: 2, toolEventCount: 0,
          startEventId: 1, lastEventId: 32, endEventId: 32, generation: resourceGeneration,
        };
        return json(route, { resourceId: "project1.task1", segments: [{ generation: resourceGeneration, turns: [summary] }], page: { limit: 20, hasMore: false } });
      }
      return json(route, { resourceId: "project1.task1", segments: [{ generation: resourceGeneration, turns: resourceTurnSummaries(conversationFixture) }], page: { limit: 20, nextCursor: "older", hasMore: true } });
    }
    if (path === "/api/workspaces/ws-test/resources/project1.task1/stream" && method === "GET") {
      harness.streamRequests.push("project1.task1");
      const frame = {
        schema: "agenthub.semantic-events.v1", cursor: 100, mode: "replace",
        source: { eventId: 100, time: now, type: "message.assistant.delta", sessionId: "session-1", turnId: "turn-stream" },
        events: [{ id: "sem_100_0", sourceEventId: 100, index: 0, time: now, type: "message.assistant.delta", sessionId: "session-1", turnId: "turn-stream", data: { text: "SSE update for project1.task1" } }],
      };
      return route.fulfill({ status: 200, contentType: "text/event-stream", headers: { "cache-control": "no-cache" }, body: `id: 100\ndata: ${JSON.stringify(frame)}\n\n` });
    }
    if (path === "/api/workspaces/ws-test/resources/project1.task1/messages" && method === "POST") {
      harness.inputBodies.push(request.postDataJSON());
      runtimeExists = true;
      if (!turnRunning) turnNumber += 1;
      turnRunning = true;
      return json(route, { status: "delivered", messageId: "msg-e2e" });
    }
    const resourceMessagesMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)\/messages$/);
    if (resourceMessagesMatch && method === "POST") {
      harness.inputBodies.push({ resourceId: decodeURIComponent(resourceMessagesMatch[1]), ...request.postDataJSON() });
      return json(route, { status: "delivered", messageId: "msg-e2e" });
    }
    if (path === "/api/workspaces/ws-test/resources/project1.task1/uploads" && method === "POST") {
      const multipart = request.postData() || "";
      const name = multipart.match(/filename="([^"]+)"/)?.[1] || "upload.txt";
      harness.uploadNames.push(name);
      return json(route, { name, path: `artifacts/upload/${name}` }, 201);
    }
    const emptyHistoryMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)\/history\/turns$/);
    if (emptyHistoryMatch && method === "GET") return json(route, { resourceId: decodeURIComponent(emptyHistoryMatch[1]), segments: [], page: { limit: 20, hasMore: false } });
    const bindingMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)\/agent-binding$/);
    if (bindingMatch && method === "PUT") {
      harness.bindingBodies.push(request.postDataJSON());
      return json(route, { agentBinding: request.postDataJSON() });
    }
    const markdownSaveMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)\/documents$/);
    if (markdownSaveMatch && method === "PUT") {
      const resourceId = decodeURIComponent(markdownSaveMatch[1]);
      const body = request.postDataJSON() as { content: string; expectedContentHash: string };
      const filePath = url.searchParams.get("path") || "";
      harness.markdownBodies.push({ path: filePath, content: body.content, expectedContentHash: body.expectedContentHash });
      if (resourceId !== "project1.task1" || body.expectedContentHash !== (savedTaskBrief?.contentHash || "project1.task1-brief-v1")) return json(route, { error: "Markdown file changed on disk" }, 409);
      savedTaskBrief = { content: body.content, contentHash: `task-brief-saved-${harness.markdownBodies.length}` };
      return json(route, { path: filePath, name: "task.md", content: savedTaskBrief.content, contentHash: savedTaskBrief.contentHash, size: savedTaskBrief.content.length });
    }
    const resourceMatch = path.match(/^\/api\/workspaces\/ws-test\/resources\/(.+)$/);
    if (resourceMatch) {
      if (decodeURIComponent(resourceMatch[1]) === "scheduler") {
        return json(route, {
          ...schedulerResource,
          scheduler: schedulerConfig,
          files: [
            { name: "scheduler.md", path: "scheduler/scheduler.md", content: "# Scheduler context\n", contentHash: "scheduler-context-v1" },
            { name: "AGENTS.md", path: "scheduler/AGENTS.md", content: "# Scheduler guidance\n", contentHash: "scheduler-agents-v1" },
          ],
          artifacts: [],
          repos: [],
        });
      }
      const resourceId = decodeURIComponent(resourceMatch[1]);
      const value = createdProject?.id === resourceId
        ? resourceDetail(createdProject)
        : createdTask?.id === resourceId
          ? resourceDetail(createdTask)
          : detail(resourceId);
      if (resourceId === "project1.task1" && savedTaskBrief) {
        value.files[0] = { ...value.files[0], content: savedTaskBrief.content, contentHash: savedTaskBrief.contentHash };
      }
      return json(route, value);
    }
    if (path === "/api/workspaces/ws-test/files") {
      const filePath = url.searchParams.get("path") || "";
      if (method === "PUT") {
        const body = request.postDataJSON();
        harness.agentsBodies.push(body);
        return json(route, { path: "AGENTS.md", name: "AGENTS.md", content: String(body.content || ""), contentHash: "agents-saved" });
      }
      if (filePath === "AGENTS.md") return json(route, { path: "AGENTS.md", name: "AGENTS.md", content: "Workspace guidance", contentHash: "agents-v1" });
      if (filePath.startsWith("wiki/")) return json(route, { path: filePath, name: filePath.split("/").pop(), content: "# Workspace Wiki\n\nStable wiki content.", contentHash: "wiki-v1" });
      const documentCandidates: MockResource[] = [project, ...project.children];
      if (createdProject) documentCandidates.push(createdProject);
      if (createdTask) documentCandidates.push(createdTask);
      const documentFile = documentCandidates
        .map((resource) => ({ resource, file: resourceDetail(resource).files[0] }))
        .find(({ resource }) => `${resource.path}/${resource.type === "project" ? "project.md" : "task.md"}` === filePath)?.file;
      if (documentFile) {
        const file = documentFile.path === "project1-migration/task1-infrastructure/task.md" && savedTaskBrief
          ? { ...documentFile, content: savedTaskBrief.content, contentHash: savedTaskBrief.contentHash }
          : documentFile;
        return json(route, file);
      }
      return json(route, { path: filePath, name: filePath.split("/").pop(), content: `# Preview\n\nContent for ${filePath}\n\n${longDetailBody}`, contentHash: `hash-${filePath}` });
    }
    if (path === "/api/workspaces/ws-test/diff") return json(route, { path: url.searchParams.get("path"), branch: "topic", base: "master", diff: "diff --git a/a.txt b/a.txt\nnew file mode 100644\n--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1 @@\n+detail diff\n", hasChanges: true });
    if (path === "/api/workspaces/ws-test/projects" && method === "POST") {
      const body = request.postDataJSON() as Record<string, unknown>;
      createdProject = {
        id: "project2",
        type: "project",
        title: String(body.description || "Created project"),
        path: "project2-created-project",
        archived: false,
        children: [],
      };
      return json(route, createdProject, 201);
    }
    if (path === "/api/workspaces/ws-test/tasks" && method === "POST") {
      const body = request.postDataJSON() as Record<string, unknown>;
      harness.taskBodies.push(body);
      createdTask = {
        id: "project1.task3",
        type: "task",
        title: String(body.title || "Created task"),
        path: "project1-migration/task3-created-from-baseline",
        archived: false,
        state: "not_started",
      };
      return json(route, createdTask, 201);
    }
    if (path === "/api/workspaces/ws-test/tasks/preview" && method === "POST") {
      const body = request.postDataJSON();
      harness.previewBodies.push(body);
      const templateName = String(body.templateName || "");
      await new Promise((resolve) => setTimeout(resolve, templateName === "feature-a" ? 350 : 20));
      const summary = String((body.templateFields as Record<string, unknown>)?.summary || "Untitled");
      return json(route, { title: `${templateName}:${summary}`, markdown: `# ${templateName}:${summary}\n`, slug: "", template: { digest: `digest-${templateName}` } });
    }
    return json(route, { error: `Unhandled mock request: ${method} ${path}` }, 500);
  });
  return harness;
}

interface ShellHarness {
  uiStateBodies: Array<{ workspaceId: string; body: Record<string, unknown> }>;
  failNextUIStateSave(): void;
}

async function installShellMockApi(page: Page): Promise<ShellHarness> {
  let failNextSave = false;
  const uiStateBodies: ShellHarness["uiStateBodies"] = [];
  const uiStates: Record<string, Record<string, unknown>> = {
    "ws-a": { version: 1, expandedProjects: ["project1"], lastResourceId: "project1.task1", projectOrder: [], taskOrder: {} },
    "ws-b": { version: 1, expandedProjects: ["project2"], lastResourceId: "project2.task1", projectOrder: [], taskOrder: {} },
  };
  const trees = {
    "ws-a": { root: "/tmp/ws-a", projects: [project], wiki: { exists: false, entries: [] } },
    "ws-b": {
      root: "/tmp/ws-b",
      projects: [{ id: "project2", type: "project", title: "Second workspace project", path: "project2", archived: false, children: [{ id: "project2.task1", type: "task", title: "Second workspace task", path: "project2/task1", archived: false }] }],
      wiki: {
        exists: true,
        entries: [
          { name: "index.md", path: "wiki/index.md", type: "file", size: 147 },
          { name: "link-preview.md", path: "wiki/link-preview.md", type: "file", size: 102 },
        ],
      },
    },
  };
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    if (path === "/api/workspaces") {
      return json(route, { version: 3, activeId: "ws-a", workspaces: [{ id: "ws-a", name: "Workspace A", path: "/tmp/ws-a" }, { id: "ws-b", name: "Workspace B", path: "/tmp/ws-b" }], agentProfiles: [] });
    }
    if (path === "/api/doctor") {
      if (method === "POST") return route.fulfill({ status: 202 });
      return json(route, { checking: false, complete: true, summary: { errors: 0, warnings: 0 }, workspaces: [] });
    }
    if (path === "/api/settings/agenthub") {
      return json(route, { config: { agentProfiles: [] }, connected: false, compatible: false, catalog: { providers: [], agents: [], probes: [] } });
    }
    if (path === "/api/settings/system" && method === "GET") {
      return json(route, mockSystemInfo([{ name: "Workspace A", path: "/tmp/ws-a" }, { name: "Workspace B", path: "/tmp/ws-b" }]));
    }
    if (/^\/api\/workspaces\/ws-[ab]\/users$/.test(path)) {
      if (method === "POST") return json(route, { version: 1, name: "User", preference: "" });
      return json(route, { users: [{ version: 1, name: "User", preference: "" }] });
    }
    const uiStateMatch = path.match(/^\/api\/workspaces\/(ws-[ab])\/ui-state$/);
    if (uiStateMatch) {
      const workspaceId = uiStateMatch[1];
      if (method === "PUT") {
        const body = request.postDataJSON() as Record<string, unknown>;
        uiStateBodies.push({ workspaceId, body });
        if (failNextSave) {
          failNextSave = false;
          return json(route, { error: "ui state save failed" }, 500);
        }
        uiStates[workspaceId] = body;
        return json(route, body);
      }
      return json(route, uiStates[workspaceId]);
    }
    const treeMatch = path.match(/^\/api\/workspaces\/(ws-[ab])\/tree$/);
    if (treeMatch) return json(route, trees[treeMatch[1] as keyof typeof trees]);
    const statusMatch = path.match(/^\/api\/workspaces\/(ws-[ab])\/resources\/(.+)\/status$/);
    if (statusMatch) return json(route, { resourceId: decodeURIComponent(statusMatch[2]), sessionState: "idle", exists: true, archived: false, acceptsMessages: true, canSteerWaiting: false, waitingMessages: [], messages: { waiting: 0 } });
    const historyMatch = path.match(/^\/api\/workspaces\/(ws-[ab])\/resources\/(.+)\/history\/turns$/);
    if (historyMatch) return json(route, { resourceId: decodeURIComponent(historyMatch[2]), segments: [], page: { limit: 20, hasMore: false } });
    const detailMatch = path.match(/^\/api\/workspaces\/(ws-[ab])\/resources\/(.+)$/);
    if (detailMatch) {
      const id = decodeURIComponent(detailMatch[2]);
      const all = Object.values(trees).flatMap((tree) => tree.projects.flatMap((item) => [item, ...(item.children || [])]));
      const resource = all.find((item) => item.id === id);
      if (!resource) return json(route, { error: "not found" }, 404);
      return json(route, {
        ...resource,
        files: [{ name: resource.type === "project" ? "project.md" : "task.md", path: `${resource.path}/${resource.type === "project" ? "project.md" : "task.md"}`, content: `# ${resource.title}`, contentHash: `${id}-v1` }],
        artifacts: [], repos: [], templates: [],
      });
    }
    if (/^\/api\/workspaces\/ws-[ab]\/files$/.test(path)) {
      const filePath = url.searchParams.get("path") || "";
      if (filePath.startsWith("wiki/")) {
        return json(route, { path: filePath, name: filePath.split("/").pop(), content: "# Workspace Wiki\n\nStable wiki content.", contentHash: "wiki-v1" });
      }
      return json(route, { path: "AGENTS.md", name: "AGENTS.md", content: "Workspace guidance", contentHash: "agents-v1" });
    }
    return json(route, { error: `Unhandled shell request: ${method} ${path}` }, 500);
  });
  return { uiStateBodies, failNextUIStateSave: () => { failNextSave = true; } };
}

test("navigates resources and creates a task through the canonical application flow", async ({ page }) => {
  const harness = await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await expect(page.locator(".brand-copy span")).toHaveText("v0.1.0");
  await expect(page.getByRole("heading", { name: "Migration project", exact: true })).toBeVisible();
  await page.getByRole("button", { name: /Infrastructure task/ }).click();
  await expect(page).toHaveURL(/project1\.task1/);
  await expect(page.getByRole("heading", { name: /Infrastructure task/ }).first()).toBeVisible();
  const bindingSelector = page.getByRole("button", { name: "Binding target" });
  await bindingSelector.click();
  const bindingMenu = page.getByRole("listbox", { name: "Binding target" });
  await expect(bindingMenu.getByRole("group")).toHaveCount(2);
  await expect(bindingMenu.getByRole("option")).toHaveText([
    "default test-agent",
    "fast test-agent",
    "review other-agent",
    "test-agent default, fast",
    "other-agent review",
  ]);
  await bindingMenu.getByRole("option", { name: "other-agent review" }).click();
  await expect.poll(() => harness.bindingBodies).toEqual([{ kind: "agent", name: "other-agent" }]);
  await page.getByRole("button", { name: "Migration project", exact: true }).click();
  await page.getByRole("button", { name: "New Task" }).click();
  const createDialog = page.getByRole("dialog", { name: "Create task" });
  await createDialog.getByRole("button", { name: "Next", exact: true }).click();
  await createDialog.locator('input[name="title"]').fill("Created from baseline");
  await createDialog.getByRole("button", { name: "Next", exact: true }).click();
  await createDialog.locator('textarea[name="detail"]').fill("Playwright isolated task body");
  await createDialog.getByRole("button", { name: "Next", exact: true }).click();
  await createDialog.getByRole("button", { name: "Create task", exact: true }).click();

  await expect.poll(() => harness.taskBodies.length).toBe(1);
  expect(harness.taskBodies[0]).toMatchObject({
    project: "project1",
    title: "Created from baseline",
    detail: "Playwright isolated task body",
  });
  await expect(page).toHaveURL(/project1\.task3/);
  await expect(page.getByRole("heading", { name: "Created from baseline", exact: true }).first()).toBeVisible();
  await expect(page.locator("#toast")).toContainText("Task created");
});

test("opens the cached Doctor report from the brand reminder", async ({ page }) => {
  await installMockApi(page, "project1.task1");
  await page.setViewportSize({ width: 440, height: 844 });
  let refreshRequests = 0;
  await page.route("**/api/doctor", async (route) => {
    if (route.request().method() === "POST") {
      refreshRequests += 1;
      return route.fulfill({ status: 202 });
    }
    return json(route, {
      checkedAt: now,
      checking: false,
      complete: true,
      summary: { errors: 16, warnings: 0 },
      workspaces: [{
        id: "ws-test",
        name: "Isolated E2E",
        path: "/tmp/pua-e2e",
        report: {
          complete: true,
          summary: { errors: 12, warnings: 0 },
          issues: Array.from({ length: 12 }, (_, index) => ({
            severity: "error",
            code: index === 0 ? "agents_managed_section_modified" : `workspace_problem_${index}`,
            message: index === 0 ? "PUA managed AGENTS.md section has been modified" : `Workspace problem ${index}`,
            path: index === 0 ? "AGENTS.md" : `project${index}/task.md`,
            suggestion: index === 0 ? "Run pua migrate after reviewing local instructions." : "Review this workspace problem.",
          })),
        },
      }, {
        id: "ws-other",
        name: "Other Workspace",
        path: "/tmp/pua-other",
        report: {
          complete: true,
          summary: { errors: 4, warnings: 0 },
          issues: [{ severity: "error", code: "other_workspace_problem", message: "Other Workspace must stay hidden" }],
        },
      }],
    });
  });

  await page.goto("/w/ws-test/r/project1.task1");
  await expect(page.locator("#doctorButton")).toHaveAttribute("aria-label", "12 errors and 0 warnings");
  await page.locator("#mobileMenuButton").click();

  const brandBand = page.locator("#mobileSidebar .brand-band");
  const brandBandBox = (await brandBand.boundingBox())!;
  expect(brandBandBox.height).toBe(56);
  const doctorButton = page.locator("#doctorButton");
  const settingsButton = page.locator("#systemSettingsButton");
  await expect(doctorButton).toHaveAttribute("title", "Workspace problems");
  await expect(doctorButton).toHaveAttribute("type", "button");
  await expect(settingsButton).toHaveAttribute("aria-label", "Settings");
  await expect(settingsButton).toHaveAttribute("title", "Settings");
  await expect(settingsButton).toHaveAttribute("type", "button");
  for (const button of [doctorButton, settingsButton]) {
    const box = (await button.boundingBox())!;
    expect(box.width).toBeGreaterThanOrEqual(44);
    expect(box.height).toBeGreaterThanOrEqual(44);
    expect(box.x).toBeGreaterThanOrEqual(brandBandBox.x);
    expect(box.x + box.width).toBeLessThanOrEqual(brandBandBox.x + brandBandBox.width + 1);
  }

  await page.locator("#doctorButton").click();
  const dialog = page.getByRole("dialog", { name: "Workspace problems" });
  await expect(dialog).toContainText("PUA managed AGENTS.md section has been modified");
  await expect(dialog).toContainText("agents_managed_section_modified");
  await expect(dialog).not.toContainText("Other Workspace must stay hidden");
  const close = dialog.getByRole("button", { name: "Close workspace problems" });
  const closeBox = (await close.boundingBox())!;
  expect(closeBox.width).toBe(44);
  expect(closeBox.height).toBe(44);
  const content = dialog.locator(".doctor-content");
  await expect.poll(() => content.evaluate((node) => node.scrollHeight > node.clientHeight)).toBe(true);
  await content.evaluate((node) => node.scrollTo(0, node.scrollHeight));
  await expect.poll(() => content.evaluate((node) => node.scrollTop > 0)).toBe(true);
  await dialog.getByRole("button", { name: "Refresh workspace checks" }).click();
  await expect.poll(() => refreshRequests).toBe(1);

  await close.click();
  await page.locator("#mobileMenuButton").click();
  await settingsButton.click();
  await expect(page.getByRole("dialog", { name: "System Settings" })).toBeVisible();
  await expect(page.locator("body")).not.toHaveClass(/mobile-sidebar-open/);
});

test("edits Markdown source through the dialog and saves with a content hash", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");
  const panel = page.locator("#detailsPanel");
  await panel.getByRole("button", { name: "Edit", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "File preview" });
  const editor = dialog.locator('[data-component-owner="markdown-editor"]');
  await expect(editor.locator(".cm-editor")).toBeVisible();
  // Edit mode shows only Save; annotate controls and Done are absent.
  await expect(editor.getByRole("button", { name: "Add annotation" })).toHaveCount(0);
  await expect(editor.getByRole("button", { name: "Done" })).toHaveCount(0);

  await editor.locator(".cm-content").click();
  await page.keyboard.press("Control+End");
  await page.keyboard.type("\nSaved from Playwright.\n");
  await editor.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => harness.markdownBodies.length).toBe(1);
  expect(harness.markdownBodies[0]).toMatchObject({
    path: "project1-migration/task1-infrastructure/task.md",
    expectedContentHash: "project1.task1-brief-v1",
  });
  expect(harness.markdownBodies[0].content).toContain("Saved from Playwright.");
  await expect(panel.locator('[data-doc-file="task.md"] .markdown-view')).toContainText("Saved from Playwright.");
  await dialog.getByRole("button", { name: "Close" }).click();
});

test("annotates read-only Markdown through the dialog and copies the review", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");
  const panel = page.locator("#detailsPanel");
  await panel.getByRole("button", { name: "Annotate", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "File preview" });
  const editor = dialog.locator('[data-component-owner="markdown-editor"]');
  await expect(editor.locator(".cm-editor")).toBeVisible();
  // Annotate mode has no Save button; the document is read-only.
  await expect(editor.getByRole("button", { name: "Save", exact: true })).toHaveCount(0);

  await editor.locator(".cm-line", { hasText: "stable selection target" }).click({ clickCount: 3 });
  const addAnnotation = editor.getByRole("button", { name: "Add annotation" });
  await expect(addAnnotation).toBeEnabled();
  await addAnnotation.click();
  await editor.getByPlaceholder("Add a comment…").fill("Clarify the expected outcome.");

  await page.context().grantPermissions(["clipboard-read", "clipboard-write"], { origin: new URL(page.url()).origin });
  await editor.getByRole("button", { name: "Copy annotations" }).click();
  const copied = await page.evaluate(() => navigator.clipboard.readText());
  expect(copied).toContain("文件：project1-migration/task1-infrastructure/task.md");
  expect(copied).toContain("批注：Clarify the expected outcome.");
  expect(copied).not.toContain("请处理");
  expect(copied).not.toContain("以下批注");

  await editor.locator(".cm-content").click();
  await page.keyboard.press("Control+End");
  await page.keyboard.type("Should not be saved.");
  await expect(editor.locator(".cm-content")).not.toContainText("Should not be saved");
  await expect.poll(() => harness.markdownBodies.length).toBe(0);
  await dialog.getByRole("button", { name: "Close" }).click();
});

test("keeps workspace Markdown actions inside the mobile viewport without horizontal overflow", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installMockApi(page, "workspace");
  await page.goto("/w/ws-test");
  const details = page.locator("#detailsPanel");
  const editButton = details.getByRole("button", { name: "Edit", exact: true });
  const annotateButton = details.getByRole("button", { name: "Annotate", exact: true });
  await expect(editButton).toBeVisible();
  await expect(annotateButton).toBeVisible();

  // The details scroll container must not grow a horizontal scrollbar.
  const horizontalOverflow = await page.locator("#detailsContent").evaluate((el) => el.scrollWidth - el.clientWidth);
  expect(horizontalOverflow).toBeLessThanOrEqual(1);

  // Both actions stay fully inside the 390px viewport.
  for (const button of [editButton, annotateButton]) {
    const box = await button.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  }
});

test("grows the agent binding menu to fit long agent lists", async ({ page }) => {
  const extraAgents = ["gpt-5.3-codex-spark", "gpt-5.6-sol", "kimi-k3", "grok-4.5", "gemini-3.1-pro", "gpt-5.6-luna", "pi-kimi", "deepseek-v4-pro"];
  await installMockApi(page, "project1.task1", false, false, false, extraAgents);
  await page.goto("/w/ws-test/r/project1.task1");

  const bindingSelector = page.getByRole("button", { name: "Binding target" });
  await bindingSelector.click();
  const bindingMenu = page.getByRole("listbox", { name: "Binding target" });
  await expect(bindingMenu.getByRole("option")).toHaveCount(13);

  // The menu opens upward from the bottom of the viewport and should size to
  // the space above the composer instead of a fixed 260px cap, keeping every
  // option visible without scrolling.
  const options = bindingMenu.getByRole("option");
  for (let index = 0; index < 13; index += 1) {
    await expect(options.nth(index)).toBeInViewport();
  }
  expect((await bindingMenu.boundingBox())!.height).toBeGreaterThan(260);
});

test("fits binding menu columns to the longest labels", async ({ page }) => {
  const extraAgents = ["gpt-5.3-codex-spark", "gpt-5.6-sol", "kimi-k3", "grok-4.5", "gemini-3.1-pro", "gpt-5.6-luna", "pi-kimi", "deepseek-v4-pro"];
  await installMockApi(page, "project1.task1", false, false, false, extraAgents);
  await page.goto("/w/ws-test/r/project1.task1");

  await page.getByRole("button", { name: "Binding target" }).click();
  const bindingMenu = page.getByRole("listbox", { name: "Binding target" });
  await expect(bindingMenu.getByRole("option")).toHaveCount(13);

  // Column widths are measured from the longest labels, so no label should be
  // truncated on a wide viewport. Compare the fractional text width (Range)
  // against the integer clientWidth to catch sub-pixel truncation that a
  // scrollWidth > clientWidth check would miss.
  const truncated = await bindingMenu.evaluate((menu) =>
    [...menu.querySelectorAll<HTMLElement>(".agent-binding-option-primary, .agent-binding-option-secondary")]
      .filter((el) => {
        const range = document.createRange();
        range.selectNodeContents(el);
        return range.getBoundingClientRect().width > el.clientWidth + 0.01;
      })
      .map((el) => el.textContent)
  );
  expect(truncated).toEqual([]);
});

test("shows the full binding menu from the settings panel", async ({ page }) => {
  const extraAgents = ["gpt-5.3-codex-spark", "gpt-5.6-sol", "kimi-k3", "grok-4.5", "gemini-3.1-pro", "gpt-5.6-luna", "pi-kimi", "deepseek-v4-pro"];
  const harness = await installMockApi(page, "project1.task1", false, false, false, extraAgents);
  await page.goto("/w/ws-test/r/project1.task1");

  await page.getByRole("tab", { name: "Settings" }).click();
  const bindingSelector = page.getByRole("button", { name: "Task Agent binding" });
  await bindingSelector.click();
  const bindingMenu = page.getByRole("listbox", { name: "Task Agent binding" });
  await expect(bindingMenu.getByRole("option")).toHaveCount(13);

  // The settings list card clips absolutely positioned content, so the menu
  // must escape it: every option stays fully visible and clickable instead of
  // only a slice showing below the row.
  const options = bindingMenu.getByRole("option");
  for (let index = 0; index < 13; index += 1) {
    await expect(options.nth(index)).toBeInViewport();
  }
  const menuBox = (await bindingMenu.boundingBox())!;
  const listBox = (await page.locator(".resource-settings-list").last().boundingBox())!;
  expect(menuBox.y + menuBox.height).toBeGreaterThan(listBox.y + listBox.height);

  await options.last().click();
  await expect.poll(() => harness.bindingBodies.length).toBe(1);
  expect(harness.bindingBodies[0]).toMatchObject({ kind: "agent", name: "deepseek-v4-pro" });
});

test("keeps the project binding menu from covering the next selector", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  const harness = await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await page.getByRole("tab", { name: "Settings" }).click();
  const projectBinding = page.getByRole("button", { name: "Project Agent binding", exact: true });
  const taskBinding = page.getByRole("button", { name: "New Task default binding", exact: true });
  await expect(projectBinding).toBeVisible();
  await expect(taskBinding).toBeVisible();

  await projectBinding.click();
  const projectMenu = page.getByRole("listbox", { name: "Project Agent binding" });
  await expect(projectMenu).toBeVisible();

  const menuBox = (await projectMenu.boundingBox())!;
  const taskBox = (await taskBinding.boundingBox())!;
  const overlaps = menuBox.x < taskBox.x + taskBox.width
    && menuBox.x + menuBox.width > taskBox.x
    && menuBox.y < taskBox.y + taskBox.height
    && menuBox.y + menuBox.height > taskBox.y;
  expect(overlaps).toBe(false);

  // The next selector must receive the click and open its own menu. No
  // option from the Project Agent menu may be committed on this route.
  await taskBinding.click();
  await expect(projectMenu).toHaveCount(0);
  await expect(page.getByRole("listbox", { name: "New Task default binding" })).toBeVisible();
  expect(harness.bindingBodies).toHaveLength(0);
});

test("keeps Workspace Agent binding selectors at a mobile touch size without overflow", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installMockApi(page, "workspace");
  await page.goto("/w/ws-test");

  await page.getByRole("tab", { name: "Settings", exact: true }).click();
  const details = page.locator("#detailsPanel");
  const labels = ["Workspace Agent binding", "New Project default binding", "New Task default binding"];
  for (const label of labels) {
    const trigger = details.getByRole("button", { name: label, exact: true });
    await expect(trigger).toBeVisible();
    await expect(trigger).toHaveAttribute("aria-haspopup", "listbox");
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
    const box = await trigger.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThanOrEqual(44);
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(440);
  }

  const horizontalOverflow = await page.locator("#detailsContent").evaluate((element) => element.scrollWidth - element.clientWidth);
  expect(horizontalOverflow).toBeLessThanOrEqual(1);
  const documentOverflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(documentOverflow).toBeLessThanOrEqual(1);
});

test("keeps Wiki file preview controls at a 44px touch size without overflow", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installMockApi(page, "workspace");
  await page.goto("/w/ws-test");

  const panel = page.locator("#detailsPanel");
  await panel.getByRole("tab", { name: "Wiki", exact: true }).click();
  const fileRow = panel.getByRole("button", { name: /link-preview\.md/ });
  await expect(fileRow).toBeVisible();
  await expect(fileRow).toHaveAttribute("type", "button");
  await fileRow.hover();

  const download = panel.getByRole("link", { name: "Download link-preview.md", exact: true });
  await expect(download).toBeVisible();
  await expect(download).toHaveAttribute("download", "link-preview.md");
  await expect(download).toHaveAttribute("title", "Download link-preview.md");
  await expect(download).toHaveAttribute("aria-label", "Download link-preview.md");
  await expect(download).toHaveAttribute("href", /download=1/);
  const downloadBox = (await download.boundingBox())!;
  expect(downloadBox.width).toBeGreaterThanOrEqual(44);
  expect(downloadBox.height).toBeGreaterThanOrEqual(44);
  expect(downloadBox.x).toBeGreaterThanOrEqual(0);
  expect(downloadBox.x + downloadBox.width).toBeLessThanOrEqual(440);
  const contentSize = await panel.locator("#detailsContent").evaluate((node) => ({
    clientWidth: node.clientWidth,
    scrollWidth: node.scrollWidth,
  }));
  expect(contentSize.scrollWidth).toBeLessThanOrEqual(contentSize.clientWidth);

  await fileRow.click();
  const dialog = page.getByRole("dialog", { name: "File preview" });
  await expect(dialog).toContainText("link-preview.md");
  const fullScreen = dialog.getByRole("button", { name: "Open file full screen" });
  const close = dialog.getByRole("button", { name: "Close", exact: true });
  for (const button of [fullScreen, close]) {
    await expect(button).toHaveAttribute("type", "button");
    await expect.poll(async () => (await button.boundingBox())?.width ?? 0).toBeGreaterThanOrEqual(44);
    await expect.poll(async () => (await button.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
    const box = (await button.boundingBox())!;
    expect(box.width).toBeGreaterThanOrEqual(44);
    expect(box.height).toBeGreaterThanOrEqual(44);
    expect(box.x).toBeGreaterThanOrEqual(0);
    expect(box.x + box.width).toBeLessThanOrEqual(440);
  }
  await expect(fullScreen).toHaveAttribute("title", "Open file full screen");
  await expect(fullScreen).toHaveAttribute("aria-label", "Open file full screen");
  await expect(close).toHaveAttribute("title", "Close");
  await expect(close).toHaveAttribute("aria-label", "Close");

  const dialogGeometry = await dialog.evaluate((node) => ({
    clientWidth: node.clientWidth,
    scrollWidth: node.scrollWidth,
    clientHeight: node.clientHeight,
    scrollHeight: node.scrollHeight,
  }));
  expect(dialogGeometry.scrollWidth).toBeLessThanOrEqual(dialogGeometry.clientWidth);
  expect(dialogGeometry.scrollHeight).toBeLessThanOrEqual(dialogGeometry.clientHeight);

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  await close.click();
  await expect(dialog).toHaveCount(0);
});

test("navigates to a newly created project", async ({ page }) => {
  const harness = await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await page.locator("#newProjectButton").click();
  await page.locator('#createDialogForm textarea[name="description"]').fill("Created from baseline project");
  await page.locator("#createDialogForm").getByRole("button", { name: "Create", exact: true }).click();

  await expect.poll(() => harness.treeRequests).toBeGreaterThan(1);
  await expect(page).toHaveURL(/\/w\/ws-test\/r\/project2$/);
  await expect(page.getByRole("heading", { name: "Created from baseline project", exact: true }).first()).toBeVisible();
  await expect(page.locator("#toast")).toContainText("Project created");
});

test("always shows all Activity tab counts and supports resizing the panel", async ({ page }) => {
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await expect(page.getByRole("tab", { name: "Running 0", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Unread 0", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Issues 0", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Inbox 0", exact: true })).toBeVisible();

  const activityPanel = page.locator('[data-component-owner="attention-list"]');
  const initialHeight = await activityPanel.evaluate((element) => element.getBoundingClientRect().height);
  const resizeHandle = page.locator("#activityResize");
  const handleBox = await resizeHandle.boundingBox();
  if (!handleBox) throw new Error("Activity resize handle is not visible");
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y + handleBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(handleBox.x + handleBox.width / 2, handleBox.y - 60);
  await page.mouse.up();
  await expect.poll(() => activityPanel.evaluate((element) => element.getBoundingClientRect().height)).toBeGreaterThan(initialHeight + 40);
  await expect.poll(() => page.evaluate(() => JSON.parse(localStorage.getItem("pua.web.paneSizes") || "{}").sidebarAttentionHeight)).toBeGreaterThan(initialHeight + 40);
});

test("uses Task workflow state in the tree and the Activity running list", async ({ page }) => {
  await installMockApi(page, "project1.task1", false, true);
  await page.goto("/w/ws-test/r/project1.task1");

  const taskRow = page.locator("#projectTree .task-item", { hasText: "Infrastructure task" });
  await expect(taskRow.locator('[data-lucide="loader-circle"]')).toHaveCount(1);
  await expect(taskRow.locator('[data-lucide="file-text"]')).toHaveCount(0);
  await expect(taskRow.locator('[data-lucide="message-square"]')).toHaveCount(0);

  await page.getByRole("tab", { name: "Running 1", exact: true }).click();
  const activityRow = page.locator('[data-component-owner="attention-list"] button.activity-row', { hasText: "Infrastructure task" });
  await expect(activityRow).toContainText("Resource working");
  await expect(activityRow.locator('.activity-status [data-lucide="file-text"]')).toHaveCount(1);
  await expect(activityRow.locator('.activity-status [data-lucide="message-square"]')).toHaveCount(0);
});

test("keeps Task workflow state independent from a sleeping Session", async ({ page }) => {
  await installMockApi(page, "project1.task1", false, false, false, [], "idle-suspended");
  await page.goto("/w/ws-test/r/project1.task1");

  const taskRow = page.locator("#projectTree .task-item", { hasText: "Infrastructure task" });
  await expect(taskRow.locator('[data-lucide="circle"]')).toHaveCount(1);
  await expect(taskRow.locator('[data-lucide="file-text"]')).toHaveCount(0);
  await expect(taskRow.locator('[data-lucide="pause-circle"]')).toHaveCount(0);
  await expect(page.getByRole("tab", { name: "Running 0", exact: true })).toBeVisible();
});

test("highlights the selected Activity resource instead of every active turn", async ({ page }) => {
  await installMockApi(page, "project1", false, true);
  await page.goto("/w/ws-test/r/project1");

  const runningActivity = page.locator('[data-component-owner="attention-list"] button.activity-row', { hasText: "Infrastructure task" });
  await expect(runningActivity).not.toHaveClass(/\bselected\b/);
  await expect(runningActivity).not.toHaveAttribute("aria-current");
  await expect(runningActivity).toHaveAttribute("data-active-turn", "true");
  await expect(runningActivity).toContainText("Resource working");

  await page.goto("/w/ws-test/r/project1.task1");
  await expect(runningActivity).toHaveClass(/\bselected\b/);
  await expect(runningActivity).toHaveAttribute("aria-current", "page");
  await expect(runningActivity).toHaveAttribute("data-active-turn", "true");
});

test("keeps a newly created task Activity row aligned when its first turn starts", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1", false, false, true);
  await page.goto("/w/ws-test/r/project1.task1");

  const activityRow = page.locator('[data-component-owner="attention-list"] button.activity-row', { hasText: "Infrastructure task" });
  await expect(activityRow).toHaveCount(0);

  const input = page.locator("#chatInput");
  await input.fill("Start the first turn");
  await input.press("Enter");
  await expect.poll(() => harness.inputBodies.length).toBe(1);
  await expect(activityRow).toHaveCount(1);
  await expect(activityRow).toHaveAttribute("data-active-turn", "true", { timeout: 8_000 });
  await expect(activityRow.locator(".activity-status-runtime-slot [data-lucide=\"loader-circle\"]")).toBeVisible();
  await expect(activityRow.locator(".activity-status-fallback-slot [data-lucide=\"file-text\"]")).toBeHidden();

  const layout = await activityRow.evaluate((row) => ({
    directChildren: row.children.length,
    statusTop: row.querySelector(".activity-status")!.getBoundingClientRect().top,
    titleTop: row.querySelector(".activity-title")!.getBoundingClientRect().top,
  }));
  expect(layout.directChildren).toBe(2);
  expect(Math.abs(layout.titleTop - layout.statusTop)).toBeLessThan(2);
});

test("does not count a running Turn as unread, then clears it after completion when clicked again", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");

  const taskRow = page.locator("#projectTree .task-item", { hasText: "Infrastructure task" });
  await expect(taskRow.locator(".unread-badge")).toHaveCount(0);

  await page.locator("#chatInput").fill("Start a new turn while this task stays selected");
  await page.locator("#chatInput").press("Enter");
  await expect.poll(() => harness.inputBodies.length).toBe(1);
  await expect(page.getByRole("tab", { name: "Running 1", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Unread 0", exact: true })).toBeVisible();
  await expect(taskRow.locator(".unread-badge")).toHaveCount(0);
  expect(harness.resourceStateBodies.filter((entry) => entry.path.endsWith("/read"))).toHaveLength(0);

  harness.finishTurn();
  await expect(taskRow.locator(".unread-badge")).toHaveText("1", { timeout: 8_000 });
  await expect(page.getByRole("tab", { name: "Running 0", exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "Unread 1", exact: true })).toBeVisible();

  await taskRow.click();
  await expect(taskRow.locator(".unread-badge")).toHaveCount(0);
  await expect(page.getByRole("tab", { name: "Unread 0", exact: true })).toBeVisible();
  expect(harness.resourceStateBodies.filter((entry) => entry.path.endsWith("/read"))).toEqual([
    {
      method: "PUT",
      path: "/api/workspaces/ws-test/resources/project1.task1/read",
      body: { throughTurnNumber: 1 },
    },
  ]);
});

async function refreshSchedulerDetail(page: Page): Promise<void> {
  await page.locator(".breadcrumb").getByRole("button", { name: "Isolated E2E", exact: true }).click();
  await expect(page).toHaveURL(/\/w\/ws-test\/?$/);
  await expect(page.locator("#detailsPanel").getByRole("heading", { name: "Isolated E2E", exact: true })).toBeVisible();

  const detailResponse = page.waitForResponse((response) => {
    const request = response.request();
    return request.method() === "GET" && new URL(response.url()).pathname === "/api/workspaces/ws-test/resources/scheduler";
  });
  await page.locator('[data-component-owner="scheduler-nav"] button').click();
  expect((await detailResponse).status()).toBe(200);
  await expect(page).toHaveURL(/\/w\/ws-test\/r\/scheduler$/);
}

test("manages natural-language schedules from the fixed Scheduler resource", async ({ page }) => {
  const harness = await installMockApi(page);
  await page.goto("/w/ws-test/r/project1.task1");

  await page.locator('[data-component-owner="scheduler-nav"] button').click();
  await expect(page).toHaveURL(/\/w\/ws-test\/r\/scheduler$/);
  await expect(page.getByRole("heading", { name: /Scheduler/ }).first()).toBeVisible();
  await expect(page.locator(".details-tabs [role=\"tab\"]")).toHaveText(["Schedules", "Context", "Settings"]);
  await expect(page.getByRole("tab", { name: "Schedules" })).toHaveAttribute("aria-selected", "true");
  await expect(page.getByText("No schedules. Native Scheduler timing is idle.")).toBeVisible();

  await page.getByLabel("Description").fill("Notify when the release is ready");
  await page.getByLabel("Condition").fill("When the release branch is green after 09:00 Shanghai time");
  const target = page.getByLabel("Target resource ID");
  const addSchedule = page.getByRole("button", { name: "Add schedule", exact: true });
  await target.fill("not-a-resource");
  await expect(target).toHaveAttribute("aria-invalid", "true");
  await expect(target).toHaveAttribute("aria-describedby", "schedule-target-error");
  await expect(page.locator("#schedule-target-error")).toContainText("open resource in the current Workspace");
  await expect(addSchedule).toBeDisabled();
  await target.fill("project1.task1");
  await expect(target).toHaveAttribute("aria-invalid", "false");

  const createResponse = page.waitForResponse((response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/api/workspaces/ws-test/scheduler");
  await addSchedule.click();
  expect((await createResponse).status()).toBe(202);
  await expect(page.locator("#toast")).toHaveText("Schedule request sent.");
  await expect(page.getByLabel("Description")).toHaveValue("");
  await expect(page.getByLabel("Condition")).toHaveValue("");
  await expect(target).toHaveValue("workspace");
  await expect(page.locator(".schedule-list article")).toHaveCount(0);
  await expect(page.getByText("No schedules. Native Scheduler timing is idle.")).toBeVisible();

  const scheduleId = harness.schedulerAgentCreateDefinition({
    description: "Notify when the release is ready",
    condition: "When the release branch is green after 09:00 Shanghai time",
    target: "project1.task1",
  });
  await refreshSchedulerDetail(page);
  await expect(page.locator(".schedule-list article")).toContainText("Notify when the release is ready");
  await expect(page.locator(".schedule-list article")).toContainText("project1.task1");

  await page.locator(".schedule-list article").getByRole("button", { name: "Edit" }).click();
  await page.getByLabel("Description").fill("Notify after release verification");

  const updateResponse = page.waitForResponse((response) => response.request().method() === "PUT" && new URL(response.url()).pathname === `/api/workspaces/ws-test/scheduler/${scheduleId}`);
  await page.getByRole("button", { name: "Update schedule" }).click();
  expect((await updateResponse).status()).toBe(202);
  await expect(page.locator("#toast")).toHaveText("Schedule update request sent.");
  await expect(page.getByRole("button", { name: "Add schedule", exact: true })).toBeVisible();
  await expect(page.getByLabel("Description")).toHaveValue("");
  await expect(page.getByLabel("Condition")).toHaveValue("");
  await expect(page.getByLabel("Target resource ID")).toHaveValue("workspace");
  await expect(page.locator(".schedule-list article")).toContainText("Notify when the release is ready");
  await expect(page.locator(".schedule-list article")).not.toContainText("Notify after release verification");

  harness.schedulerAgentUpdateDefinition(scheduleId, { description: "Notify after release verification" });
  await refreshSchedulerDetail(page);
  await expect(page.locator(".schedule-list article")).toContainText("Notify after release verification");
  await expect(page.locator(".schedule-list article code")).toContainText("r2");

  await page.locator(".schedule-list article").getByRole("button", { name: "Remove" }).click();
  await page.getByRole("alertdialog", { name: "Remove schedule" }).getByRole("button", { name: "Remove" }).click();
  await expect(page.locator(".schedule-list article")).toHaveCount(0);
  await expect(page.getByText("No schedules. Native Scheduler timing is idle.")).toBeVisible();
  expect(harness.schedulerBodies.map(({ method }) => method)).toEqual(["POST", "PUT", "DELETE"]);
  expect(harness.schedulerBodies[0].body).toEqual({
    description: "Notify when the release is ready",
    condition: "When the release branch is green after 09:00 Shanghai time",
    target: "project1.task1",
  });
  expect(harness.schedulerBodies[1].body).toEqual({
    expectedRevision: "1",
    description: "Notify after release verification",
    condition: "When the release branch is green after 09:00 Shanghai time",
    target: "project1.task1",
  });
});

test("keeps Scheduler schedules content inside a 440px viewport", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  const harness = await installMockApi(page);
  await page.goto("/w/ws-test/r/scheduler");

  const measureSchedulerOverflow = async () => page.locator("#detailsContent").evaluate((root) => [
    { name: "details-content", element: root },
    ...Array.from(root.querySelectorAll<HTMLElement>(".schedule-editor, .schedule-list, .schedule-list > *"))
      .map((element) => ({ name: element.className || element.tagName.toLowerCase(), element })),
  ].map(({ name, element }) => ({ name, clientWidth: element.clientWidth, scrollWidth: element.scrollWidth })));
  const expectNoSchedulerOverflow = async () => {
    const dimensions = await measureSchedulerOverflow();
    for (const dimension of dimensions) {
      expect(dimension.scrollWidth - dimension.clientWidth, dimension.name).toBeLessThanOrEqual(1);
    }
    const documentSize = await page.evaluate(() => ({
      bodyClient: document.body.clientWidth,
      bodyScroll: document.body.scrollWidth,
      htmlClient: document.documentElement.clientWidth,
      htmlScroll: document.documentElement.scrollWidth,
    }));
    expect(documentSize).toEqual({ bodyClient: 440, bodyScroll: 440, htmlClient: 440, htmlScroll: 440 });
  };

  await expect(page.getByText("No schedules. Native Scheduler timing is idle.")).toBeVisible();
  await expectNoSchedulerOverflow();

  await page.getByLabel("Description").fill("Notify when the release is ready");
  await page.getByLabel("Condition").fill("When the release branch is green after 09:00 Shanghai time and the deployment checklist is complete");

  const createResponse = page.waitForResponse((response) => response.request().method() === "POST" && new URL(response.url()).pathname === "/api/workspaces/ws-test/scheduler");
  await page.getByRole("button", { name: "Add schedule", exact: true }).click();
  expect((await createResponse).status()).toBe(202);
  await expect(page.locator("#toast")).toHaveText("Schedule request sent.");
  await expect(page.getByLabel("Description")).toHaveValue("");
  await expect(page.getByLabel("Condition")).toHaveValue("");
  await expect(page.locator(".schedule-list article")).toHaveCount(0);
  await expect(page.getByText("No schedules. Native Scheduler timing is idle.")).toBeVisible();
  await expectNoSchedulerOverflow();

  harness.schedulerAgentCreateDefinition({
    description: "Notify when the release is ready",
    condition: "When the release branch is green after 09:00 Shanghai time and the deployment checklist is complete",
    target: "workspace",
  });
  await refreshSchedulerDetail(page);
  await expect(page.locator(".schedule-list article")).toContainText("Notify when the release is ready");
  await expectNoSchedulerOverflow();
});

test("keeps Scheduler schedule controls at a 44px touch size on a 440px viewport", async ({ page }) => {
  const harness = await installMockApi(page);
  await page.setViewportSize({ width: 440, height: 844 });
  await page.goto("/w/ws-test/r/scheduler");

  const editor = page.locator(".schedule-editor");
  const description = page.getByRole("textbox", { name: "Description", exact: true });
  const condition = page.getByRole("textbox", { name: "Condition", exact: true });
  const target = page.getByRole("textbox", { name: "Target resource ID", exact: true });
  const addSchedule = page.getByRole("button", { name: "Add schedule", exact: true });

  await expect(description).toHaveAttribute("placeholder", "What should the Scheduler understand?");
  await expect(condition).toHaveAttribute("placeholder", "For example: when the release branch is green after 09:00 Shanghai time");
  await expect(condition).toHaveAttribute("rows", "3");
  await expect(target).toHaveAttribute("placeholder", "workspace, project1, or project1.task1");
  await expect(addSchedule).toHaveAttribute("type", "button");
  await expect(addSchedule).toBeDisabled();

  const editorBox = await editor.boundingBox();
  expect(editorBox).not.toBeNull();
  for (const control of [description, condition, target, addSchedule]) {
    const box = await control.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThanOrEqual(44);
    expect(box!.x).toBeGreaterThanOrEqual(editorBox!.x);
    expect(box!.x + box!.width).toBeLessThanOrEqual(editorBox!.x + editorBox!.width + 1);
  }

  await description.fill("Notify when the release is ready");
  await condition.fill("When the release branch is green after 09:00 Shanghai time");
  await target.fill("project1.task1");
  await expect(addSchedule).toBeEnabled();
  await addSchedule.click();
  await expect.poll(() => harness.schedulerBodies.length).toBe(1);
  expect(harness.schedulerBodies[0].body).toEqual({
    description: "Notify when the release is ready",
    condition: "When the release branch is green after 09:00 Shanghai time",
    target: "project1.task1",
  });
});

test("keeps Svelte Detail documents, History, previews, diffs, and edits stable during refresh", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");
  const panel = page.locator("#detailsPanel");
  const content = panel.locator("#detailsContent");
  await expect(panel).toHaveAttribute("data-component-owner", "detail-panel");
  await expect(panel.locator(".resource-ref-badge")).toHaveText("#1");
  await expect(panel.getByRole("tab", { name: "Task" })).toHaveAttribute("aria-selected", "true");
  await expect.poll(() => page.evaluate(() => ({
    root: getComputedStyle(document.documentElement).overscrollBehavior,
    body: getComputedStyle(document.body).overscrollBehavior,
    content: getComputedStyle(document.getElementById("detailsContent")!).overscrollBehavior,
  }))).toEqual({ root: "none", body: "none", content: "contain" });
  const headerTop = await panel.locator(".details-header").evaluate((node) => node.getBoundingClientRect().top);
  const tabsTop = await panel.locator(".details-tabs").evaluate((node) => node.getBoundingClientRect().top);
  const documentView = panel.locator('[data-doc-file="task.md"] .markdown-view');
  await documentView.evaluate((node) => {
    node.setAttribute("data-identity-probe", "stable-document");
    const text = node.querySelector("p")?.firstChild;
    if (text) {
      const range = document.createRange();
      range.setStart(text, 0);
      range.setEnd(text, Math.min(8, text.textContent?.length || 0));
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
    }
    const content = document.getElementById("detailsContent");
    if (content) content.scrollTop = 180;
  });
  await expect.poll(() => content.evaluate((node) => node.scrollTop)).toBe(180);
  await expect.poll(() => panel.evaluate((node) => node.scrollTop)).toBe(0);
  await expect.poll(() => panel.locator(".details-header").evaluate((node) => node.getBoundingClientRect().top)).toBe(headerTop);
  await expect.poll(() => panel.locator(".details-tabs").evaluate((node) => node.getBoundingClientRect().top)).toBe(tabsTop);
  await page.waitForTimeout(5_200);
  await expect(documentView).toHaveAttribute("data-identity-probe", "stable-document");
  await expect.poll(() => page.evaluate(() => window.getSelection()?.toString())).toBe("Baseline");
  await expect.poll(() => content.evaluate((node) => node.scrollTop)).toBe(180);

  await panel.getByRole("tab", { name: "History" }).click();
  const history = panel.locator('[data-component-owner="history-timeline"]');
  await expect(history).toContainText("Generation 1");
  await expect(history).toContainText("test-agent");
  await expect(history).toContainText("Unknown provider");
  await expect(history).toContainText("Unknown model");
  const firstTurn = history.locator(".history-turn").first();
  await firstTurn.locator(".history-turn-header").click();
  await expect(firstTurn).toContainText("gen-1 baseline message 1");
  await history.getByRole("button", { name: "Load older History" }).click();
  await expect(history).toContainText("gen-1 older history");
  await expect(documentView).toHaveAttribute("data-identity-probe", "stable-document");

  await panel.getByRole("tab", { name: "Artifacts" }).click();
  await panel.getByRole("button", { name: /notes\.md/ }).click();
  const preview = page.getByRole("dialog", { name: "File preview" });
  await expect(preview).toContainText("Content for project1-migration/task1-infrastructure/artifacts/notes.md");
  await preview.locator("[data-preview-scroll]").evaluate((node) => { node.setAttribute("data-identity-probe", "stable-preview"); node.scrollTop = 40; });
  await page.waitForTimeout(5_200);
  await expect(preview.locator("[data-preview-scroll]")).toHaveAttribute("data-identity-probe", "stable-preview");
  await expect.poll(() => preview.locator("[data-preview-scroll]").evaluate((node) => node.scrollTop)).toBe(40);
  await preview.getByRole("button", { name: "Close" }).click();

  await panel.getByRole("tab", { name: "Worktrees" }).click();
  await panel.getByRole("button", { name: "View Diff" }).click();
  await expect(page.getByRole("dialog", { name: "Worktree diff" })).toContainText("detail diff");
  await page.getByRole("dialog", { name: "Worktree diff" }).getByRole("button", { name: "Close" }).click();

  await panel.locator(".breadcrumb").getByRole("button", { name: "Isolated E2E", exact: true }).click();
  await expect(panel.getByRole("tab", { name: "AGENTS.md" })).toHaveAttribute("aria-selected", "true");
  await expect(panel.locator('[data-doc-file="AGENTS.md"]')).toContainText("Workspace guidance");
  await panel.getByRole("button", { name: "Edit", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "File preview" });
  const editor = dialog.locator('[data-component-owner="markdown-editor"]');
  await expect(editor.locator(".cm-editor")).toBeVisible();
  await editor.locator(".cm-content").click();
  await page.keyboard.press("Control+End");
  await page.keyboard.type("\nEdited workspace guidance.\n");
  await editor.getByRole("button", { name: "Save", exact: true }).click();
  await expect.poll(() => harness.agentsBodies.length).toBe(1);
  expect(harness.agentsBodies[0]).toMatchObject({ expectedContentHash: "agents-v1" });
  expect(harness.agentsBodies[0].content).toContain("Edited workspace guidance.");
  await expect(panel.locator('[data-doc-file="AGENTS.md"]')).toContainText("Edited workspace guidance.");
  await dialog.getByRole("button", { name: "Close" }).click();
  await panel.getByRole("tab", { name: "Wiki" }).click();
  await panel.getByRole("button", { name: /index\.md/ }).click();
  await expect(page.getByRole("dialog", { name: "File preview" })).toContainText("Stable wiki content");
});

test("resolves Markdown resource references and navigates through the SPA", async ({ page }) => {
  await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");

  const reference = page.locator('[data-doc-file="task.md"] a[data-pua-resource-id="project1.task2"]');
  await expect(reference).toHaveText("Follow-up task");
  await expect(reference).toHaveAttribute("href", "/w/ws-test/r/project1.task2");
  await reference.click();

  await expect(page).toHaveURL(/\/w\/ws-test\/r\/project1\.task2$/);
  await expect(page.getByRole("heading", { name: "Follow-up task", exact: true })).toBeVisible();
});

test("renders History turn detail with conversation timeline components", async ({ page }) => {
  await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");
  const panel = page.locator("#detailsPanel");
  await panel.getByRole("tab", { name: "History" }).click();
  const history = panel.locator('[data-component-owner="history-timeline"]');
  const firstTurn = history.locator(".history-turn").first();
  await firstTurn.locator(".history-turn-header").click();
  const firstItem = firstTurn.locator(".history-item").first();
  await expect(firstItem.locator(".agent-message-row")).toBeVisible();
  await expect(firstItem).toContainText("gen-1 baseline message 1");
  await expect(history.locator(".history-item-label")).toHaveCount(0);
});

test("keeps narrow chat content inside the column while code blocks scroll locally", async ({ page }) => {
  await installMockApi(page, "project1.task1", false, false, false, [], "idle", 0, "narrow-layout");
  await page.addInitScript(() => {
    localStorage.setItem("pua.web.paneSizes", JSON.stringify({ sidebarWidth: 280, chatWidth: 320, sidebarAttentionHeight: 210 }));
  });
  await page.goto("/w/ws-test/r/project1.task1");

  const log = page.locator("#chatTimeline");
  await expect(log.locator(".agent-message-row.user")).toHaveCount(1);
  await expect(log.locator(".markdown-rendered table")).toHaveCount(1);
  await expect(log.locator(".markdown-rendered pre")).toHaveCount(1);
  await expect(log.locator(".agent-activity-group")).toHaveCount(1);

  await expect.poll(() => log.evaluate((element) => {
    const widthDelta = (node: Element | null) => {
      if (!node) return -1;
      const box = node as HTMLElement;
      return box.scrollWidth - box.clientWidth;
    };
    const messages = [...element.querySelectorAll<HTMLElement>(".agent-message-row")];
    const content = [...element.querySelectorAll<HTMLElement>(".agent-message-content")];
    const nonCodeContent = content.filter((node) => !node.querySelector("pre"));
    const activity = [...element.querySelectorAll<HTMLElement>(".agent-activity-group")];
    const table = element.querySelector<HTMLElement>(".markdown-rendered table");
    const code = element.querySelector<HTMLElement>(".markdown-rendered pre");
    return {
      overflowX: getComputedStyle(element).overflowX,
      overflowY: getComputedStyle(element).overflowY,
      rootOverflow: widthDelta(element),
      ordinaryMessageOverflow: widthDelta(messages.find((node) => node.classList.contains("user")) || null),
      contentOverflow: Math.max(0, ...nonCodeContent.map(widthDelta)),
      activityOverflow: Math.max(0, ...activity.map(widthDelta)),
      tableOverflow: widthDelta(table),
      codeHasLocalOverflow: Boolean(code && code.scrollWidth > code.clientWidth && code.clientWidth <= element.clientWidth),
    };
  })).toEqual({
    overflowX: "hidden",
    overflowY: "auto",
    rootOverflow: 0,
    ordinaryMessageOverflow: 0,
    contentOverflow: 0,
    activityOverflow: 0,
    tableOverflow: 0,
    codeHasLocalOverflow: true,
  });
});

test("shows a working indicator for the active Turn", async ({ page }) => {
  await installMockApi(page, "project1.task1", false, true);
  await page.goto("/w/ws-test/r/project1.task1");

  await expect(page.locator(".turn-working-indicator")).toHaveText("working...");
});

test("pages resource history, sends input, receives SSE, and preserves active reading state during refresh", async ({ page }) => {
  const harness = await installMockApi(page);
  await page.goto("/w/ws-test/r/project1.task1");

  await expect(page.locator("#chatTimeline")).toContainText("SSE update for project1.task1");
  await expect(page.locator("#chatTimeline")).toContainText("gen-1 baseline message 1");
  const historyAnchor = page.locator(".conversation-turn").filter({ hasText: "gen-1 baseline message 1" }).first();
  await expect(historyAnchor).toBeVisible();
  await historyAnchor.evaluate((node) => node.setAttribute("data-history-anchor", "stable"));
  await page.locator("#loadOlderAgentEventsButton, .load-older-events").click();
  await expect(page.locator("#chatTimeline")).toContainText("gen-1 older history");
  await expect(historyAnchor).toHaveAttribute("data-history-anchor", "stable");
  const input = page.locator("#chatInput");
  await input.fill("Preserve this draft until accepted");
  await input.press("Enter");
  await expect.poll(() => harness.inputBodies.length).toBe(1);
  expect(harness.inputBodies[0]).toMatchObject({ text: "Preserve this draft until accepted" });
  await expect(input).toBeFocused();
  await expect(input).toHaveValue("");

  await input.fill("Draft survives refresh");
  const before = await page.locator("#chatTimeline").evaluate((log) => {
    log.scrollTop = Math.max(1, Math.floor(log.scrollHeight / 3));
    const bubble = log.querySelector(".agent-message-bubble");
    const text = bubble?.firstChild;
    if (!text) throw new Error("message text is unavailable");
    const range = document.createRange();
    range.selectNodeContents(bubble);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
    return { scrollTop: log.scrollTop, selection: selection?.toString() || "" };
  });
  await input.focus();
  await page.waitForTimeout(5_300);

  await expect(input).toBeFocused();
  await expect(input).toHaveValue("Draft survives refresh");
  const after = await page.locator("#chatTimeline").evaluate((log) => ({
    scrollTop: log.scrollTop,
    selection: window.getSelection()?.toString() || "",
  }));
  expect(after.scrollTop).toBe(before.scrollTop);
  expect(after.selection).toBe(before.selection);
  expect(after.selection).not.toBe("");
  expect(harness.treeRequests).toBeGreaterThan(1);
  expect(harness.streamRequests).toContain("project1.task1");
});

test("multiline send restores single-line Enter and explicitly resumes timeline follow", async ({ page }) => {
  const harness = await installMockApi(page);
  await page.goto("/w/ws-test/r/project1.task1");

  const timeline = page.locator("#chatTimeline");
  await expect(timeline).toContainText("gen-1 baseline message 1");
  await timeline.evaluate((log) => {
    log.scrollTop = 0;
    const bubble = log.querySelector(".agent-message-bubble");
    if (!bubble) throw new Error("message bubble is unavailable");
    const range = document.createRange();
    range.selectNodeContents(bubble);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
  });
  await expect.poll(() => timeline.evaluate((log) => log.scrollHeight - log.scrollTop - log.clientHeight)).toBeGreaterThan(32);

  const input = page.locator("#chatInput");
  await input.fill("delete the newline");
  await input.press("Shift+Enter");
  await expect(input).toHaveValue("delete the newline\n");
  await input.press("Backspace");
  await expect(input).toHaveValue("delete the newline");
  await input.press("Enter");
  await expect.poll(() => harness.inputBodies.length).toBe(1);
  expect(harness.inputBodies[0]).toMatchObject({ text: "delete the newline" });
  await expect(input).toHaveValue("");

  await input.fill("first line\nsecond line");
  await input.press("Control+Enter");
  await expect.poll(() => harness.inputBodies.length).toBe(2);
  expect(harness.inputBodies[1]).toMatchObject({ text: "first line\nsecond line" });
  await expect(input).toHaveValue("");
  await expect.poll(() => timeline.evaluate((log) => log.scrollHeight - log.scrollTop - log.clientHeight)).toBeLessThanOrEqual(1);
  await expect.poll(() => page.evaluate(() => window.getSelection()?.toString() || "")).toBe("");

  await input.fill("next single-line message");
  await input.press("Enter");
  await expect.poll(() => harness.inputBodies.length).toBe(3);
  expect(harness.inputBodies[2]).toMatchObject({ text: "next single-line message" });
});

test("shows waiting messages above the composer and inserts one through steer", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1", true);
  await page.goto("/w/ws-test/r/project1.task1");

  const queue = page.locator(".chat-message-queue");
  await expect(queue).toBeVisible();
  await expect(queue).toContainText("Review the mailbox change now");
  const input = page.locator("#chatInput");
  const queueBounds = await queue.boundingBox();
  const inputBounds = await input.boundingBox();
  expect(queueBounds).not.toBeNull();
  expect(inputBounds).not.toBeNull();
  expect(queueBounds!.y + queueBounds!.height).toBeLessThanOrEqual(inputBounds!.y);

  await queue.getByRole("button", { name: /Insert waiting message/ }).click();
  await expect.poll(() => harness.steeredMessageIds).toEqual(["msg-waiting"]);
  await expect(queue).toBeHidden();
  await expect(page.locator("#toast")).toContainText("Message inserted into the current turn");
});

test("keeps resource chat free of AgentHub Session lifecycle controls", async ({ page }) => {
  await installMockApi(page);
  await page.goto("/w/ws-test/r/project1");
  await expect(page.locator("#agentStartButton")).toHaveCount(0);
  await expect(page.locator("#agentResumeButton")).toHaveCount(0);
  await expect(page.locator("#agentCloseSessionButton")).toHaveCount(0);
});

test("switches wizard templates and ignores an older preview response", async ({ page }) => {
  const harness = await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");
  await page.getByRole("button", { name: "New Task" }).click();

  const dialog = page.getByRole("dialog", { name: "Create task" });
  await dialog.getByRole("option", { name: /Feature A/ }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  // The template generates the title, so the wizard shows the generated value
  // instead of a title input.
  await expect(dialog.locator("[data-generated-title]")).toBeVisible();
  await expect(dialog.locator('input[name="title"]')).toHaveCount(0);
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByLabel("Summary *").fill("older");
  await expect.poll(() => harness.previewBodies.filter((body) => body.templateName === "feature-a" && (body.templateFields as Record<string, unknown>)?.summary === "older").length).toBe(1);

  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await dialog.getByRole("option", { name: /Feature B/ }).click();
  await page.getByRole("alertdialog", { name: "Switch template" }).getByRole("button", { name: "Discard" }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByLabel("Summary *").fill("newer");
  await expect.poll(() => harness.previewBodies.filter((body) => body.templateName === "feature-b" && (body.templateFields as Record<string, unknown>)?.summary === "newer").length).toBe(1);

  // The slower feature-a preview response must not overwrite the newer title.
  await dialog.getByRole("button", { name: "Back", exact: true }).click();
  await expect(dialog.locator("[data-generated-title]")).toContainText("feature-b:newer");
  await page.waitForTimeout(450);
  await expect(dialog.locator("[data-generated-title]")).toContainText("feature-b:newer");

  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await expect(dialog.locator("[data-summary]")).toContainText("feature-b:newer");
  await dialog.getByRole("button", { name: "Create task", exact: true }).click();
  await expect.poll(() => harness.taskBodies.length).toBe(1);
  expect(harness.previewBodies.map((body) => body.templateName)).toEqual(expect.arrayContaining(["feature-a", "feature-b"]));
  expect(harness.taskBodies[0]).toMatchObject({
    project: "project1",
    title: "",
    templateName: "feature-b",
    templateFields: { summary: "newer" },
    expectedTemplateDigest: "digest-feature-b",
  });
});

test("creates and starts a task with a chosen agent from the wizard", async ({ page }) => {
  const harness = await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");
  await page.getByRole("button", { name: "New Task" }).click();

  const dialog = page.getByRole("dialog", { name: "Create task" });
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.locator('input[name="title"]').fill("Auto started task");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.locator('textarea[name="detail"]').fill("Body");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();

  // The start step preselects the workspace default binding.
  await dialog.getByRole("radio", { name: /Create and start/ }).click();
  await expect(dialog.getByRole("button", { name: "Start agent" })).toContainText("default");
  const agentMenu = page.getByRole("listbox", { name: "Start agent" });
  await dialog.getByRole("button", { name: "Start agent" }).click();
  await agentMenu.getByRole("option", { name: "other-agent review" }).click();
  await expect(dialog.locator("[data-binding-note]")).toBeVisible();
  await expect(dialog.locator('textarea[name="startPrompt"]')).not.toBeEmpty();

  await dialog.getByRole("button", { name: "Create & start", exact: true }).click();
  await expect.poll(() => harness.taskBodies.length).toBe(1);
  // The binding is switched before the start prompt is sent.
  await expect.poll(() => harness.bindingBodies).toEqual([{ kind: "agent", name: "other-agent" }]);
  await expect.poll(() => harness.inputBodies.some((body) => body.resourceId === "project1.task3" && typeof body.text === "string" && body.text.length > 0)).toBe(true);
  await expect(page.locator("#toast")).toContainText("Task created and started");
  await expect(page).toHaveURL(/project1\.task3/);
});

test("anchors the wizard binding menu to its trigger under the Riso theme", async ({ page }) => {
  // The Riso theme once applied an feDisplacementMap filter to dialogs; a
  // filtered ancestor becomes the containing block for position:fixed
  // children, which stranded the fixed binding menu against the dialog edge
  // instead of the trigger.
  await page.addInitScript(() => window.localStorage.setItem("pua.web.themePreference", "riso"));
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");
  await page.getByRole("button", { name: "New Task" }).click();

  const dialog = page.getByRole("dialog", { name: "Create task" });
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.locator('input[name="title"]').fill("Riso binding anchor");
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  await dialog.getByRole("radio", { name: /Create and start/ }).click();

  const trigger = dialog.getByRole("button", { name: "Start agent" });
  await trigger.click();
  const menu = page.getByRole("listbox", { name: "Start agent" });
  await expect(menu).toBeVisible();

  const triggerBox = (await trigger.boundingBox())!;
  const menuBox = (await menu.boundingBox())!;
  // Right edges align and the menu hugs the trigger on either side (viewport
  // coordinates), with every option reachable.
  expect(Math.abs(menuBox.x + menuBox.width - (triggerBox.x + triggerBox.width))).toBeLessThanOrEqual(2);
  const below = Math.abs(menuBox.y - (triggerBox.y + triggerBox.height + 6));
  const above = Math.abs(menuBox.y + menuBox.height - (triggerBox.y - 6));
  expect(Math.min(below, above)).toBeLessThanOrEqual(2);
  for (const option of await menu.getByRole("option").all()) {
    await expect(option).toBeInViewport();
  }
});

test("keeps the Create task wizard usable across desktop and mobile layouts", async ({ page }) => {
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");
  await page.getByRole("button", { name: "New Task" }).click();

  const dialog = page.getByRole("dialog", { name: "Create task" });
  const wizard = dialog.locator('[data-component-owner="task-wizard"]');
  await expect(wizard).toBeVisible();
  await expect(dialog.locator(".wizard-steps li")).toHaveCount(4);
  const desktop = await wizard.evaluate((node) => ({
    bodyOverflow: getComputedStyle(node.querySelector(".wizard-body")!).overflowY,
  }));
  expect(desktop).toEqual({ bodyOverflow: "auto" });

  await dialog.getByRole("button", { name: "Next", exact: true }).click();
  const title = dialog.locator('input[name="title"]');
  await title.fill("Responsive local draft");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(title).toHaveValue("Responsive local draft");
  await expect(dialog.getByRole("button", { name: "Next", exact: true })).toBeVisible();
  await expect(dialog.locator(".wizard-steps li.active")).toContainText("Title");
  const bounds = await dialog.boundingBox();
  expect(bounds).not.toBeNull();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(844);
});

test("shows the normalized User name in the input after saving", async ({ page }) => {
  await installMockApi(page);
  await page.goto("/w/ws-test/r/project1.task1");

  await page.getByRole("button", { name: "Settings" }).click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  await settings.getByRole("button", { name: "User" }).click();
  const name = settings.getByLabel("Name");

  await name.fill("bad name");
  await expect(name).toHaveValue("badname");
  await settings.getByRole("button", { name: "Save" }).click();
  await expect(page.locator("#toast")).toContainText("User name saved as badname.");
  await expect(name).toHaveValue("badname");
});

test("preserves composer draft through upload and Settings", async ({ page }) => {
  const harness = await installMockApi(page, "project1.task1", false, false, false, [], "idle", 500);
  await page.goto("/w/ws-test/r/project1.task1");

  const input = page.locator("#chatInput");
  await input.fill("Keep this draft");
  await input.evaluate((node) => { node.dataset.identityProbe = "same-composer"; });
  await page.waitForTimeout(5_200);
  await expect(input).toHaveAttribute("data-identity-probe", "same-composer");
  await expect(input).toHaveValue("Keep this draft");

  await page.getByRole("button", { name: "Upload files" }).click();
  await page.locator("#agentUploadInput").setInputFiles({ name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("isolated upload") });
  await expect(page.getByText("artifacts/upload/notes.txt")).toBeVisible();
  await page.getByRole("dialog", { name: "Upload files" }).getByRole("button", { name: "Done" }).click();
  await expect(input).toHaveValue("Keep this draft\nartifacts/upload/notes.txt");
  expect(harness.uploadNames).toEqual(["notes.txt"]);

  await page.getByRole("button", { name: "Settings" }).click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  await settings.getByRole("button", { name: "User" }).click();
  const name = settings.getByLabel("Name");
  await page.evaluate(() => {
    const state = window as Window & { __settingsPublicationTimer?: number };
    state.__settingsPublicationTimer = window.setInterval(() => window.dispatchEvent(new StorageEvent("storage", {
      key: "pua.web.user.v1",
      newValue: JSON.stringify({ version: 1, name: "Remote_User" }),
    })), 10);
  });
  try {
    await name.fill("Locator-Probe");
    await page.waitForTimeout(100);
    await expect(name).toHaveValue("Locator-Probe");
  } finally {
    await page.evaluate(() => {
      const state = window as Window & { __settingsPublicationTimer?: number };
      if (state.__settingsPublicationTimer) window.clearInterval(state.__settingsPublicationTimer);
      delete state.__settingsPublicationTimer;
    });
  }
  await name.fill("");
  await name.pressSequentially("Sequential_Probe");
  await expect(name).toHaveValue("Sequential_Probe");
  await name.fill("");
  await name.focus();
  await page.keyboard.type("Native-Probe");
  await expect(name).toHaveValue("Native-Probe");
  await name.fill("Migration-User");
  await expect(name).toHaveValue("Migration-User");
  await settings.getByRole("button", { name: "Save" }).click();
  await settings.getByRole("button", { name: "Agents" }).click();
  await expect(settings.getByLabel("Endpoint")).toHaveCount(0);
  await expect(settings).not.toContainText("API unknown · AgentHub unknown");
  await settings.getByRole("button", { name: "Profiles" }).click();
  await settings.getByRole("button", { name: "Agents" }).click();
  await expect(settings.getByLabel("Endpoint")).toHaveCount(0);
  await expect(settings.locator(".settings-save-hint")).toBeHidden();
  await settings.getByRole("button", { name: "Close" }).click();

  await expect(input).toHaveValue("Keep this draft\nartifacts/upload/notes.txt");
});

test("manages users in the Workspace resource settings instead of System Settings", async ({ page }) => {
  await installMockApi(page, "workspace");
  await page.goto("/w/ws-test");

  await page.getByRole("tab", { name: "Settings", exact: true }).click();
  const details = page.locator("#detailsPanel");
  await expect(details.locator(".resource-settings-section-head strong", { hasText: "Users" })).toBeVisible();
  await expect(details.getByText("User", { exact: true })).toBeVisible();
  await expect(details.getByTitle("Switch to another user before deleting this user")).toBeDisabled();

  await details.getByLabel("Preference for User").fill("Prefer concise replies");
  await details.getByRole("button", { name: "Save preference" }).click();
  await expect(page.locator("#toast")).toContainText("Preferences saved for User.");

  await page.locator("#systemSettingsButton").click();
  const systemSettings = page.getByRole("dialog", { name: "System Settings" });
  await expect(systemSettings.getByRole("heading", { name: "Workspace users", exact: true })).toHaveCount(0);
});

test("keeps the workspace Users card actions inside a 390px mobile viewport", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installMockApi(page, "workspace");
  await page.goto("/w/ws-test");

  await page.getByRole("tab", { name: "Settings", exact: true }).click();
  const details = page.locator("#detailsPanel");
  const deleteButton = details.getByTitle("Switch to another user before deleting this user");
  await expect(deleteButton).toBeVisible();

  // The details scroll container must not grow a horizontal scrollbar.
  const horizontalOverflow = await page.locator("#detailsContent").evaluate((el) => el.scrollWidth - el.clientWidth);
  expect(horizontalOverflow).toBeLessThanOrEqual(1);

  // The Delete action stays fully inside the 390px viewport instead of being
  // clipped past the card's right edge.
  const box = await deleteButton.boundingBox();
  expect(box).not.toBeNull();
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(390);
});

test("keeps canonical navigation synchronized across history, workspace restore, and reorder rollback", async ({ page }) => {
  const harness = await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");
  const app = page.locator("#app");
  const detailPanel = page.locator("#detailsPanel");
  await expect(app).toHaveAttribute("data-component-owner", "app-shell");
  await expect(detailPanel).toHaveAttribute("data-component-owner", "detail-panel");
  await detailPanel.evaluate((node) => { node.dataset.identityProbe = "persistent-detail"; });

  await page.getByRole("button", { name: /Follow-up task/ }).click();
  await expect(page).toHaveURL(/\/w\/ws-a\/r\/project1\.task2$/);
  await expect(page.locator('#projectTree .tree-item.active')).toContainText("Follow-up task");
  await page.goBack();
  await expect(page).toHaveURL(/\/w\/ws-a\/r\/project1\.task1$/);
  await expect(page.locator('#projectTree .tree-item.active')).toContainText("Infrastructure task");
  await page.goForward();
  await expect(page).toHaveURL(/\/w\/ws-a\/r\/project1\.task2$/);
  await expect(detailPanel).toHaveAttribute("data-identity-probe", "persistent-detail");

  await page.locator("#workspaceSwitcher").click();
  await page.getByRole("option", { name: /Workspace B/ }).click();
  await expect(page).toHaveURL(/\/w\/ws-b\/r\/project2\.task1$/);
  await expect(page.getByRole("heading", { name: "Second workspace task", exact: true })).toBeVisible();
  await page.locator("#workspaceSwitcher").click();
  await page.getByRole("option", { name: /Workspace A/ }).click();
  await expect(page).toHaveURL(/\/w\/ws-a\/r\/project1\.task2$/);
  await expect(page.locator('#projectTree .tree-item.active')).toContainText("Follow-up task");

  await page.locator("#treeEditButton").click();
  const tasks = page.locator("#projectTree .task-group .tree-item");
  await expect(tasks).toHaveCount(2);
  await expect(tasks.first().locator(".drag-handle")).toBeVisible();
  const before = await tasks.locator(".name-text").allTextContents();
  harness.failNextUIStateSave();
  await page.evaluate(() => {
    const rows = [...document.querySelectorAll<HTMLElement>("#projectTree .task-group .tree-item")];
    const handle = rows[0]?.querySelector<HTMLElement>(".drag-handle");
    if (!handle || !rows[1]) throw new Error("task drag targets missing");
    const transfer = new DataTransfer();
    handle.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: transfer }));
    const rect = rows[1].getBoundingClientRect();
    rows[1].dispatchEvent(new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer: transfer, clientY: rect.bottom - 1 }));
    rows[1].dispatchEvent(new DragEvent("drop", { bubbles: true, cancelable: true, dataTransfer: transfer, clientY: rect.bottom - 1 }));
  });
  await expect(page.locator("#toast")).toContainText("ui state save failed");
  await expect.poll(() => tasks.locator(".name-text").allTextContents()).toEqual(before);
  expect(harness.uiStateBodies.some((entry) => entry.workspaceId === "ws-a" && Object.keys((entry.body.taskOrder as Record<string, unknown>) || {}).length > 0)).toBe(true);
});

test("keeps mobile navigation in the Svelte app shell and stacks both panes with a draggable divider", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");

  await page.locator("#mobileMenuButton").click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);
  await page.locator("#mobileSidebarBackdrop").click();
  await expect(page.locator("body")).not.toHaveClass(/mobile-sidebar-open/);

  // Single-column mode stacks details above chat; both stay visible.
  await expect(page.locator("#detailsPanel")).toBeVisible();
  await expect(page.locator("#agentPanel")).toBeVisible();
  await expect(page.locator("#mobileChatButton")).toHaveCount(0);
  const divider = page.locator("#detailsResizeY");
  await expect(divider).toBeVisible();

  // The divider drags with touch pointers and keeps both header bands visible.
  const panelHeight = async (selector: string) => (await page.locator(selector).boundingBox())!.height;
  const chatBefore = await panelHeight("#agentPanel");
  const box = (await divider.boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2 - 120, { steps: 5 });
  await page.mouse.up();
  expect(await panelHeight("#agentPanel")).toBeGreaterThan(chatBefore + 80);

  // The stacked panes and the chosen height persist across reloads.
  await page.reload();
  await expect(page.locator("#detailsPanel")).toBeVisible();
  await expect(page.locator("#agentPanel")).toBeVisible();
  expect(await panelHeight("#agentPanel")).toBeGreaterThan(chatBefore + 80);
});

test("closes the 440px navigation drawer without changing the selected resource", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");

  const menuButton = page.locator("#mobileMenuButton");
  const menuBox = await menuButton.boundingBox();
  const toolbarBox = await page.locator(".mobile-toolbar").boundingBox();
  const detailsBox = await page.locator("#detailsPanel").boundingBox();
  expect(menuBox).not.toBeNull();
  expect(toolbarBox).not.toBeNull();
  expect(detailsBox).not.toBeNull();
  expect(menuBox!.width).toBe(44);
  expect(menuBox!.height).toBe(44);
  expect(toolbarBox!.height).toBe(52);
  expect(detailsBox!.y).toBe(52);

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  await menuButton.click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);
  await page.getByRole("button", { name: "Close navigation" }).click();

  await expect(page.locator("body")).not.toHaveClass(/mobile-sidebar-open/);
  await expect(page).toHaveURL(/\/w\/ws-a\/r\/project1\.task1$/);
  await expect(page.locator("#projectTree .tree-item.active")).toContainText("Infrastructure task");
});

test("keeps the 440px workspace drawer controls at a 44px touch size", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-b");

  await page.locator("#mobileMenuButton").click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);

  const head = page.locator(".workspace-switcher-head");
  const openWorkspace = page.locator("#workspaceOpen");
  const switchWorkspace = page.locator("#workspaceSwitcher");
  await expect(openWorkspace).toBeVisible();
  await expect(switchWorkspace).toBeVisible();
  await expect.poll(async () => (await head.boundingBox())?.x ?? -1).toBeGreaterThanOrEqual(0);
  await expect(openWorkspace).toHaveAttribute("title", "Open workspace");
  await expect(openWorkspace).toHaveAttribute("aria-label", "Open workspace");
  await expect(switchWorkspace).toHaveAttribute("title", "Switch workspace");
  await expect(switchWorkspace).toHaveAttribute("aria-haspopup", "listbox");

  const headBox = (await head.boundingBox())!;
  const openBox = (await openWorkspace.boundingBox())!;
  const switchBox = (await switchWorkspace.boundingBox())!;
  expect(headBox.height).toBe(46);
  expect(openBox.height).toBeGreaterThanOrEqual(44);
  expect(switchBox.height).toBeGreaterThanOrEqual(44);
  expect(switchBox.width).toBe(32);
  expect(openBox.x).toBeGreaterThanOrEqual(headBox.x);
  expect(openBox.x + openBox.width).toBeLessThanOrEqual(headBox.x + headBox.width);
  expect(switchBox.x).toBeGreaterThanOrEqual(headBox.x);
  expect(switchBox.x + switchBox.width).toBeLessThanOrEqual(headBox.x + headBox.width);

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  await switchWorkspace.click();
  await expect(page.locator("#workspaceMenu")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator("#workspaceMenu")).toBeHidden();
  await page.getByRole("button", { name: "Close navigation" }).click();
  await expect(page.locator("body")).not.toHaveClass(/mobile-sidebar-open/);
  await expect(page).toHaveURL(/\/w\/ws-b$/);
});

test("keeps the Scheduler navigation drawer entry at a 44px touch size", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installMockApi(page);
  await page.goto("/w/ws-test/r/project1.task1");

  await page.locator("#mobileMenuButton").click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);

  const drawer = page.locator("#mobileSidebar");
  const scheduler = page.locator('[data-component-owner="scheduler-nav"] > button');
  await expect(scheduler).toBeVisible();
  await expect(scheduler).toBeEnabled();
  await expect(scheduler).toHaveAttribute("title", "Workspace Scheduler");
  await expect(scheduler).toHaveAttribute("type", "button");
  await expect(scheduler).toHaveAccessibleName("Scheduler Natural-language schedules");

  // The drawer slides in, so wait for its transform to settle before comparing
  // the entry's geometry with the drawer bounds.
  await expect.poll(async () => (await drawer.boundingBox())?.x ?? -1).toBe(0);
  const drawerBox = (await drawer.boundingBox())!;
  const schedulerBox = (await scheduler.boundingBox())!;
  expect(schedulerBox.height).toBeGreaterThanOrEqual(44);
  expect(schedulerBox.x).toBeGreaterThanOrEqual(drawerBox.x);
  expect(schedulerBox.x + schedulerBox.width).toBeLessThanOrEqual(drawerBox.x + drawerBox.width + 1);

  const documentSize = await page.evaluate(() => ({
    bodyClient: document.body.clientWidth,
    bodyScroll: document.body.scrollWidth,
    htmlClient: document.documentElement.clientWidth,
    htmlScroll: document.documentElement.scrollWidth,
  }));
  expect(documentSize).toEqual({ bodyClient: 440, bodyScroll: 440, htmlClient: 440, htmlScroll: 440 });

  await scheduler.click();
  await expect(page).toHaveURL(/\/w\/ws-test\/r\/scheduler$/);
});

test("keeps the 440px Projects actions at a 44px touch size", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-b");

  await page.locator("#mobileMenuButton").click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);

  const tree = page.locator('[data-component-owner="project-tree"]');
  const title = tree.locator(".section-title");
  const label = title.locator(".section-label");
  const reorder = tree.locator("#treeEditButton");
  const create = tree.locator("#newProjectButton");
  await expect.poll(async () => (await tree.boundingBox())?.x ?? -1).toBeGreaterThanOrEqual(0);
  await expect(label).toHaveText("Projects");
  await expect(reorder).toHaveAttribute("type", "button");
  await expect(reorder).toHaveAttribute("title", "Reorder projects");
  await expect(reorder).toHaveAttribute("aria-pressed", "false");
  await expect(create).toHaveAttribute("type", "button");
  await expect(create).toHaveAttribute("title", "New project");

  const titleBox = (await title.boundingBox())!;
  const firstProject = tree.locator("#projectTree > .tree-item").first();
  const firstProjectBox = (await firstProject.boundingBox())!;
  expect(titleBox.height).toBeGreaterThanOrEqual(44);
  for (const button of [reorder, create]) {
    const box = (await button.boundingBox())!;
    expect(box.width).toBeGreaterThanOrEqual(44);
    expect(box.height).toBeGreaterThanOrEqual(44);
    expect(box.x).toBeGreaterThanOrEqual(titleBox.x);
    expect(box.x + box.width).toBeLessThanOrEqual(titleBox.x + titleBox.width + 1);
    expect(box.y + box.height).toBeLessThanOrEqual(firstProjectBox.y);
  }

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  await reorder.click();
  await expect(reorder).toHaveAttribute("title", "Done reordering");
  await expect(reorder).toHaveAttribute("aria-pressed", "true");
  await expect(tree).toHaveClass(/editing/);
  await reorder.click();
  await expect(reorder).toHaveAttribute("title", "Reorder projects");

  await create.click();
  await expect(page.getByRole("dialog", { name: "Create project" })).toBeVisible();
  await page.getByRole("dialog", { name: "Create project" }).getByRole("button", { name: "Close" }).click();
  await expect(page.getByRole("dialog", { name: "Create project" })).toHaveCount(0);
});

test("keeps 440px Projects resource rows at a 44px touch size without changing tree semantics", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");

  await page.locator("#mobileMenuButton").click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);

  const projectRow = page.locator("#projectTree > .tree-item").first();
  const taskRows = page.locator("#projectTree .task-item");
  await expect.poll(async () => (await projectRow.boundingBox())?.x ?? -1).toBeGreaterThanOrEqual(0);
  await expect(projectRow).toHaveAttribute("type", "button");
  await expect(projectRow).toHaveAttribute("aria-label", /Open tasks: 2 tasks; 0 working/);
  await expect(taskRows).toHaveCount(2);
  await expect(taskRows.first()).toHaveAttribute("type", "button");
  await expect(taskRows.first()).toHaveAttribute("aria-label", /Infrastructure task.*Not started/);
  await expect(taskRows.nth(1)).toHaveAttribute("aria-label", /Follow-up task.*Not started/);

  const projectBox = await projectRow.boundingBox();
  expect(projectBox).not.toBeNull();
  for (const row of [projectRow, taskRows.first(), taskRows.nth(1)]) {
    const box = await row.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThanOrEqual(44);
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(440);
    // The favorite star is gone; rows keep plain button semantics only.
    await expect(row.locator('[role="checkbox"]')).toHaveCount(0);
  }

  const taskBox = await taskRows.first().boundingBox();
  expect(taskBox).not.toBeNull();
  expect(taskBox!.x).toBeGreaterThan(projectBox!.x);
  await expect(taskRows.first().locator(".task-state-icon")).toHaveCount(1);

  const treeSection = page.locator("#mobileSidebar .tree-section");
  await expect(treeSection).toBeVisible();
  expect(await treeSection.evaluate((node) => getComputedStyle(node).overflowY)).toBe("auto");

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  await taskRows.nth(1).click();
  await expect(page).toHaveURL(/\/w\/ws-a\/r\/project1\.task2$/);
  await expect(page.locator("#projectTree .tree-item.active")).toContainText("Follow-up task");
});

test("keeps Workspace Wiki file rows at a 44px touch size without overflow", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-b");

  const panel = page.locator("#detailsPanel");
  await panel.getByRole("tab", { name: "Wiki", exact: true }).click();
  const browser = panel.locator('[data-component-owner="file-browser"]');
  const rows = browser.locator("button.artifact-row.file");
  await expect(rows).toHaveCount(2);

  for (const name of ["index.md", "link-preview.md"]) {
    const row = rows.filter({ hasText: name });
    await expect(row).toHaveCount(1);
    await expect(row).toHaveAttribute("type", "button");
    await expect(row).toHaveAccessibleName(new RegExp(`${name} \\d+ B`));
    const box = await row.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThanOrEqual(44);
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(440);

    const download = row.locator("a.artifact-download");
    await expect(download).toHaveAttribute("download", name);
    await expect(download).toHaveAttribute("aria-label", `Download ${name}`);
    await expect(download).toHaveAttribute("href", new RegExp(`path=wiki%2F${name}&download=1$`));
  }

  const previewRow = rows.filter({ hasText: "link-preview.md" });
  await previewRow.click();
  await expect(page.getByRole("dialog", { name: "File preview" })).toContainText("Stable wiki content");
  await expect(previewRow).toHaveClass(/active/);
  const activeBox = await previewRow.boundingBox();
  expect(activeBox).not.toBeNull();
  expect(activeBox!.height).toBeGreaterThanOrEqual(44);

  const detailsSize = await page.locator("#detailsContent").evaluate((node) => ({
    clientWidth: node.clientWidth,
    scrollWidth: node.scrollWidth,
  }));
  expect(detailsSize.scrollWidth).toBeLessThanOrEqual(detailsSize.clientWidth);

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  await page.getByRole("dialog", { name: "File preview" }).getByRole("button", { name: "Close" }).click();
});

test("stacks details above chat with a draggable divider in the two-column layout", async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 800 });
  await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");

  // Sidebar stays visible; details stack above chat behind a horizontal divider.
  await expect(page.locator("#mobileSidebar")).toBeVisible();
  await expect(page.locator("#detailsResize")).toBeHidden();
  await expect(page.locator("#detailsResizeY")).toBeVisible();
  await expect(page.locator("#detailsPanel")).toBeVisible();
  await expect(page.locator("#agentPanel")).toBeVisible();

  const panelHeight = async (selector: string) => (await page.locator(selector).boundingBox())!.height;
  const dragDivider = async (deltaY: number) => {
    const box = (await page.locator("#detailsResizeY").boundingBox())!;
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.mouse.down();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2 + deltaY, { steps: 5 });
    await page.mouse.up();
  };

  // Dragging the divider up grows the chat pane and persists the new height.
  const chatBefore = await panelHeight("#agentPanel");
  await dragDivider(-120);
  expect(await panelHeight("#agentPanel")).toBeGreaterThan(chatBefore + 80);
  const saved = await page.evaluate(() => JSON.parse(window.localStorage.getItem("pua.web.paneSizes") || "{}"));
  expect(saved.chatHeight).toBeGreaterThan(320);

  // Dragging the divider to the bottom shrinks the chat pane to its header band.
  await dragDivider(2000);
  expect(await panelHeight("#agentPanel")).toBeLessThanOrEqual(64);

  // Dragging the divider to the top shrinks the details pane to its header band.
  await dragDivider(-2000);
  expect(await panelHeight("#detailsPanel")).toBeLessThanOrEqual(64);

  // Widening back to the three-column layout shows both panes side by side.
  await page.setViewportSize({ width: 1500, height: 800 });
  await expect(page.locator("#detailsResizeY")).toBeHidden();
  await expect(page.locator("#detailsPanel")).toBeVisible();
  await expect(page.locator("#agentPanel")).toBeVisible();
  await expect(page.locator("#detailsResize")).toBeVisible();
});

test("lets users switch the layout from the Appearance settings tab, including the collapsed-sidebar split", async ({ page }) => {
  await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");

  // Desktop default viewport (1500px): auto resolves to the three-column layout.
  await expect(page.locator("body")).toHaveAttribute("data-layout", "three");
  await expect(page.locator("#detailsResizeY")).toBeHidden();

  const openAppearanceTab = async () => {
    await page.locator("#systemSettingsButton").click();
    await page.locator(".settings-tab", { hasText: "Appearance" }).click();
    await expect(page.locator('[data-component-owner="appearance-settings-panel"]')).toBeVisible();
  };

  // Scope to the workspace layout group; the theme group reuses the same classes.
  const layoutGroup = page.locator('[role="radiogroup"][aria-label="Workspace layout"]');

  // Two columns: details stack above chat even on a wide window.
  await openAppearanceTab();
  await expect(layoutGroup.locator(".layout-option.active")).toContainText("Auto");
  await page.locator(".layout-option", { hasText: "Two columns" }).click();
  await expect(page.locator("body")).toHaveAttribute("data-layout", "two");
  await expect(page.locator("#detailsResize")).toBeHidden();
  await expect(page.locator("#detailsResizeY")).toBeVisible();
  await expect(page.locator("#detailsPanel")).toBeVisible();
  await expect(page.locator("#agentPanel")).toBeVisible();

  // Split: the sidebar collapses into a drawer, details and chat sit side by side.
  await page.locator(".layout-option", { hasText: "Split" }).click();
  await expect(page.locator("body")).toHaveAttribute("data-layout", "split");
  await expect(page.locator("#mobileSidebar")).toBeHidden();
  await expect(page.locator(".workspace-toolbar")).toBeVisible();
  await expect(page.locator("#detailsPanel")).toBeVisible();
  await expect(page.locator("#agentPanel")).toBeVisible();
  await expect(page.locator("#detailsResize")).toBeVisible();
  await expect(page.locator("#sidebarResize")).toBeHidden();
  await page.locator(".settings-close").click();

  // The drawer opens from the toolbar and closes from the backdrop.
  await page.locator("#splitMenuButton").click();
  await expect(page.locator("body")).toHaveClass(/mobile-sidebar-open/);
  await expect(page.locator("#mobileSidebar")).toBeVisible();
  await page.locator("#mobileSidebarBackdrop").click({ position: { x: 800, y: 400 } });
  await expect(page.locator("body")).not.toHaveClass(/mobile-sidebar-open/);

  // The preference persists across reloads; switching back to auto restores the follow-width behavior.
  await page.reload();
  await expect(page.locator("body")).toHaveAttribute("data-layout", "split");
  await page.locator("#splitMenuButton").click();
  await openAppearanceTab();
  await expect(layoutGroup.locator(".layout-option.active")).toContainText("Split");
  await page.locator(".layout-option", { hasText: "Auto" }).click();
  await expect(page.locator("body")).toHaveAttribute("data-layout", "three");
  await expect(page.locator("#mobileSidebar")).toBeVisible();
});

test("lets users scale the text of each column independently from the Appearance settings tab", async ({ page }) => {
  await installShellMockApi(page);
  await page.goto("/w/ws-a/r/project1.task1");

  await page.locator("#systemSettingsButton").click();
  await page.locator(".settings-tab", { hasText: "Appearance" }).click();

  const rootStyle = () => page.evaluate(() => ({
    sidebar: document.documentElement.style.getPropertyValue("--sidebar-font-scale"),
    details: document.documentElement.style.getPropertyValue("--details-font-scale"),
    chat: document.documentElement.style.getPropertyValue("--chat-font-scale"),
  }));
  expect(await rootStyle()).toEqual({ sidebar: "1", details: "1", chat: "1" });

  await page.locator('input[aria-label="Sidebar text size"]').fill("125");
  await page.locator('input[aria-label="Chat text size"]').fill("85");
  expect(await rootStyle()).toEqual({ sidebar: "1.25", details: "1", chat: "0.85" });
  await expect(page.locator('input[aria-label="Sidebar text size"]')).toHaveValue("125");

  // Scales persist across reloads and the reset restores every column.
  await page.reload();
  expect(await rootStyle()).toEqual({ sidebar: "1.25", details: "1", chat: "0.85" });
  await page.locator("#systemSettingsButton").click();
  await page.locator(".settings-tab", { hasText: "Appearance" }).click();
  await page.locator(".appearance-reset").click();
  expect(await rootStyle()).toEqual({ sidebar: "1", details: "1", chat: "1" });
  await expect(page.locator(".appearance-reset")).toBeDisabled();
});

test("keeps every task details tab reachable without horizontal scrolling in a 390px mobile viewport", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installMockApi(page, "project1.task1");
  await page.goto("/w/ws-test/r/project1.task1");

  const panel = page.locator("#detailsPanel");
  const tabs = panel.locator(".details-tabs");
  await expect(tabs).toBeVisible();
  await expect(tabs.locator('[role="tab"]')).toHaveText(["Task", "History", "Artifacts", "Worktrees", "Settings"]);

  // The tab strip itself must not overflow the pane horizontally.
  const tabStrip = await tabs.evaluate((node) => ({ client: node.clientWidth, scroll: node.scrollWidth }));
  expect(tabStrip.scroll).toBeLessThanOrEqual(tabStrip.client);

  // Every tab, including the trailing Settings tab, stays inside the viewport.
  for (const tab of await tabs.locator('[role="tab"]').all()) {
    const box = await tab.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  }

  // The trailing tab can be activated directly, without scrolling it into view.
  const settingsTab = tabs.getByRole("tab", { name: "Settings" });
  await settingsTab.click();
  await expect(settingsTab).toHaveAttribute("aria-selected", "true");
});

test("keeps Project detail tabs at a 44px touch target in a 440px viewport", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  const panel = page.locator("#detailsPanel");
  const tabs = panel.locator(".details-tabs");
  await expect(tabs).toBeVisible();
  await expect(tabs.locator('[role="tab"]')).toHaveText(["Project", "History", "Artifacts", "Settings"]);

  const tabStrip = await tabs.evaluate((node) => ({ client: node.clientWidth, scroll: node.scrollWidth }));
  expect(tabStrip.scroll).toBeLessThanOrEqual(tabStrip.client);

  for (const tab of await tabs.locator('[role="tab"]').all()) {
    const box = await tab.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThanOrEqual(44);
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(440);
  }

  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    scrollWidth: document.documentElement.scrollWidth,
    clientWidth: document.documentElement.clientWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);

  const settingsTab = tabs.getByRole("tab", { name: "Settings" });
  await settingsTab.click();
  await expect(settingsTab).toHaveAttribute("aria-selected", "true");
});

test("keeps the Workspace Generation lifecycle Save target at 44px in a 440px viewport", async ({ page }) => {
  await page.setViewportSize({ width: 440, height: 844 });
  await installShellMockApi(page);
  await page.goto("/w/ws-b");

  const panel = page.locator("#detailsPanel");
  const settingsTab = panel.getByRole("tab", { name: "Settings", exact: true });
  await expect(settingsTab).toBeVisible();
  await settingsTab.click();

  const save = panel.locator(".resource-settings-policy-controls").getByRole("button", { name: "Save", exact: true });
  await expect(save).toBeDisabled();
  const saveBox = (await save.boundingBox())!;
  expect(saveBox.height).toBeGreaterThanOrEqual(44);

  const budgets = [
    "Maximum Turns per Generation",
    "Maximum accumulated Turn minutes per Generation",
  ];
  const contentBox = (await panel.locator("#detailsContent").boundingBox())!;
  for (const ariaLabel of budgets) {
    const input = panel.locator(`input[aria-label="${ariaLabel}"]`);
    await expect(input).toHaveAttribute("type", "number");
    await expect(input).toHaveAttribute("aria-label", ariaLabel);
    const geometry = await input.evaluate((element) => {
      const inputBox = element.getBoundingClientRect();
      const labelBox = element.closest("label")?.getBoundingClientRect();
      return {
        input: { x: inputBox.x, right: inputBox.right, height: inputBox.height },
        label: labelBox ? { x: labelBox.x, right: labelBox.right, height: labelBox.height } : null,
      };
    });
    expect(geometry.label).not.toBeNull();
    expect(geometry.input.height).toBeGreaterThanOrEqual(44);
    expect(geometry.label!.height).toBeGreaterThanOrEqual(44);
    for (const box of [geometry.input, geometry.label!]) {
      expect(box.x).toBeGreaterThanOrEqual(contentBox.x);
      expect(box.right).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
    }
  }

  // Changing a valid budget still enables the existing Save action; the
  // control remains a normal number input with the same labels and handler.
  await panel.locator('input[aria-label="Maximum Turns per Generation"]').fill("25");
  await expect(save).toBeEnabled();

  expect(saveBox.x).toBeGreaterThanOrEqual(contentBox.x);
  expect(saveBox.x + saveBox.width).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
  const overflow = await panel.locator("#detailsContent").evaluate((node) => node.scrollWidth - node.clientWidth);
  expect(overflow).toBeLessThanOrEqual(1);
  const documentOverflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(documentOverflow).toBeLessThanOrEqual(1);
});

test("keeps Scheduler Agent settings at a 44px touch size in a 440px viewport", async ({ page }) => {
  await installMockApi(page);
  await page.setViewportSize({ width: 440, height: 844 });
  await page.goto("/w/ws-test/r/scheduler");

  const panel = page.locator("#detailsPanel");
  await panel.getByRole("tab", { name: "Settings", exact: true }).click();

  const content = panel.locator("#detailsContent");
  const contentBox = (await content.boundingBox())!;
  const binding = panel.getByRole("button", { name: "Scheduler Agent binding", exact: true });

  await expect(binding).toHaveAttribute("type", "button");
  await expect(binding).toHaveAttribute("aria-haspopup", "listbox");
  await expect(binding).toHaveAttribute("aria-expanded", "false");
  await expect(panel.getByText("Native timing runs in the Server.")).toBeVisible();
  await expect(panel.getByRole("spinbutton", { name: "Scheduler wake interval in minutes" })).toHaveCount(0);

  for (const control of [binding]) {
    const box = await control.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.height).toBeGreaterThanOrEqual(44);
    expect(box!.x).toBeGreaterThanOrEqual(contentBox.x);
    expect(box!.x + box!.width).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
  }

  await binding.click();
  const menu = page.getByRole("listbox", { name: "Scheduler Agent binding", exact: true });
  await expect(menu).toBeVisible();
  await expect(binding).toHaveAttribute("aria-expanded", "true");
  await expect(menu.getByRole("option")).toHaveCount(5);
  await page.keyboard.press("Escape");
  await expect(menu).toHaveCount(0);
  await expect(binding).toHaveAttribute("aria-expanded", "false");

  const detailsSize = await content.evaluate((node) => ({ clientWidth: node.clientWidth, scrollWidth: node.scrollWidth }));
  expect(detailsSize.scrollWidth).toBeLessThanOrEqual(detailsSize.clientWidth);
  const documentSize = await page.evaluate(() => ({
    body: document.body.getBoundingClientRect().width,
    html: document.documentElement.getBoundingClientRect().width,
    clientWidth: document.documentElement.clientWidth,
    scrollWidth: document.documentElement.scrollWidth,
  }));
  expect(documentSize.body).toBe(440);
  expect(documentSize.html).toBe(440);
  expect(documentSize.scrollWidth).toBeLessThanOrEqual(documentSize.clientWidth);
});

test("opens read-only System information by default", async ({ page }) => {
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await page.locator("#systemSettingsButton").click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  await expect(settings.getByRole("button", { name: "System" })).toHaveAttribute("aria-current", "page");
  const panel = settings.locator('[data-component-owner="system-info-panel"]');
  await expect(panel.getByRole("heading", { name: "System Information" })).toBeVisible();
  await expect(panel).toContainText("/tmp/pua/serve.json");
  await expect(panel).toContainText("/tmp/pua-e2e");
  await expect(panel).toContainText("/tmp/agenthub/sessions");
  await expect(panel.locator("input, select, textarea, [type=submit]")).toHaveCount(0);

  const cards = panel.locator(".system-info-card");
  await expect(cards).toHaveCount(2);
  const puaBox = (await cards.nth(0).boundingBox())!;
  const agentHubBox = (await cards.nth(1).boundingBox())!;
  expect(agentHubBox.y).toBeGreaterThanOrEqual(puaBox.y + puaBox.height);
});

test("shows an unavailable AgentHub without hiding PUA system information", async ({ page }) => {
  await installMockApi(page, "project1");
  await page.route("**/api/settings/system", (route) => json(route, {
    ...mockSystemInfo([{ name: "Isolated E2E", path: "/tmp/pua-e2e" }]),
    agentHub: {
      mode: "external", address: "127.0.0.1", port: "4646", endpoint: "http://127.0.0.1:4646/agenthub",
      connected: false, compatible: false, version: "", paths: { config: "", sessions: "", archive: "", logs: "" }, error: "connection refused",
    },
  }));
  await page.goto("/w/ws-test/r/project1");

  await page.locator("#systemSettingsButton").click();
  const panel = page.locator('[data-component-owner="system-info-panel"]');
  await expect(panel).toContainText("Unavailable");
  await expect(panel).toContainText("connection refused");
  await expect(panel).toContainText("/tmp/pua/serve.json");
});

test("keeps every System Settings tab reachable without horizontal scrolling in a 390px mobile viewport", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await page.locator("#mobileMenuButton").click();
  await page.locator("#systemSettingsButton").click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  const tabs = settings.locator(".settings-tabs");
  await expect(tabs).toBeVisible();
  await expect(tabs.locator(".settings-tab")).toHaveText(["System", "Workspace", "Appearance", "Agents", "Profiles", "Notifications"]);

  // The tab strip itself must not overflow the modal horizontally.
  const tabStrip = await tabs.evaluate((node) => ({ client: node.clientWidth, scroll: node.scrollWidth }));
  expect(tabStrip.scroll).toBeLessThanOrEqual(tabStrip.client);

  // Every tab, including the trailing Agents/Profiles/Notifications tabs, stays inside the viewport.
  for (const tab of await tabs.locator(".settings-tab").all()) {
    const box = await tab.boundingBox();
    expect(box).not.toBeNull();
    expect(box!.x).toBeGreaterThanOrEqual(0);
    expect(box!.x + box!.width).toBeLessThanOrEqual(390);
  }

  // The trailing tab can be activated directly, without scrolling it into view.
  const notificationsTab = tabs.locator(".settings-tab", { hasText: "Notifications" });
  await notificationsTab.click();
  await expect(notificationsTab).toHaveAttribute("aria-current", "page");
});

test("keeps the System Settings close control in the dialog header at a 220px viewport", async ({ page }) => {
  await page.setViewportSize({ width: 220, height: 844 });
  await installMockApi(page, "project1");
  await page.goto("/w/ws-test/r/project1");

  await page.locator("#mobileMenuButton").click();
  await page.locator("#systemSettingsButton").click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  const close = settings.locator(".settings-close");
  await expect(close).toBeVisible();
  const heading = settings.locator("h2", { hasText: "System Information" });
  await expect(heading).toBeVisible();

  // The close control keeps its own header row above the content heading
  // instead of sharing the heading's line (the 220px regression).
  await expect.poll(async () => Math.round((await close.boundingBox())?.width || 0)).toBe(44);
  const closeBox = (await close.boundingBox())!;
  const headingBox = (await heading.boundingBox())!;
  expect(closeBox.width).toBe(44);
  expect(closeBox.height).toBe(44);
  expect(closeBox.y + closeBox.height).toBeLessThanOrEqual(headingBox.y);

  // The close control stays fully inside the narrow modal.
  const modalBox = (await settings.boundingBox())!;
  expect(closeBox.x).toBeGreaterThanOrEqual(modalBox.x);
  expect(closeBox.x + closeBox.width).toBeLessThanOrEqual(modalBox.x + modalBox.width + 1);
  const contentOverflow = await settings.locator(".settings-content").evaluate((node) => node.scrollWidth - node.clientWidth);
  expect(contentOverflow).toBeLessThanOrEqual(0);

  // Scrolling the panel content does not move the close control.
  await settings.locator(".settings-body").evaluate((node) => node.scrollTo(0, 400));
  const scrolledCloseBox = (await close.boundingBox())!;
  expect(Math.abs(scrolledCloseBox.y - closeBox.y)).toBeLessThanOrEqual(1);

  // And it still closes the dialog.
  await close.click();
  await expect(settings).toBeHidden();
});

test("keeps Profiles settings cards inside the 390px mobile viewport without horizontal overflow", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await installMockApi(page, "project1.task1");
  // Later-registered routes win: serve profiles with realistic long
  // descriptions like the production environment that surfaced the bug.
  const describedProfiles = [
    { key: "default", description: "Balanced, recommended agent", agentName: "test-agent" },
    { key: "fast", description: "Faster responses for simple tasks", agentName: "test-agent" },
    { key: "reasoning", description: "More thorough reasoning for complex tasks", agentName: "other-agent" },
  ];
  await page.route("**/api/workspaces", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        version: 3,
        activeId: "ws-test",
        workspaces: [{ id: "ws-test", name: "Isolated E2E", path: "/tmp/pua-e2e" }],
        agentProfiles: describedProfiles,
      }),
    });
  });
  await page.route("**/api/settings/agenthub", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        config: { agentProfiles: describedProfiles },
        connected: true,
        compatible: true,
        catalog: {
          providers: [{ id: "test", name: "Test Provider", enabled: true }],
          agents: ["test-agent", "other-agent"].map((name) => ({ name, providerId: "test", available: true })),
          probes: [],
        },
      }),
    });
  });
  await page.goto("/w/ws-test/r/project1.task1");

  await page.getByRole("button", { name: "Open navigation" }).click();
  await page.locator("#systemSettingsButton").click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  await settings.getByRole("button", { name: "Profiles" }).click();
  const addProfile = settings.getByRole("button", { name: "Add profile" });
  await expect(addProfile).toBeVisible();

  const assertNoHorizontalOverflow = async () => {
    // The settings content viewport must not grow a horizontal scrollbar.
    const overflow = await settings.locator(".settings-content").evaluate((node) => node.scrollWidth - node.clientWidth);
    expect(overflow).toBeLessThanOrEqual(1);

    // Every profile card, its delete action and the Add profile button stay
    // inside the modal content area.
    const contentBox = (await settings.locator(".settings-content").boundingBox())!;
    for (const card of await settings.locator(".settings-profile-card").all()) {
      const box = await card.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(contentBox.x);
      expect(box!.x + box!.width).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
    }
    for (const action of await settings.locator(".settings-profile-card .icon-button.danger").all()) {
      const box = await action.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x + box!.width).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
    }
    const addBox = (await addProfile.boundingBox())!;
    expect(addBox.x + addBox.width).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
  };
  await assertNoHorizontalOverflow();

  // Expanding a card to edit it must not reintroduce overflow either.
  await settings.getByRole("button", { name: "Delete profile reasoning" }).waitFor();
  await settings.locator(".settings-profile-card-toggle", { hasText: "reasoning" }).click();
  await expect(settings.getByLabel("Summary")).toBeVisible();
  await assertNoHorizontalOverflow();
});

test("keeps Workspace settings card actions inside a 220px mobile viewport without left clipping", async ({ page }) => {
  await page.setViewportSize({ width: 220, height: 844 });
  await installMockApi(page, "project1");
  // Later-registered routes win: list several workspaces like the production
  // environment that surfaced the clipped "PUA default" action buttons.
  await page.route("**/api/workspaces", (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    return route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        version: 3,
        activeId: "ws-test",
        workspaces: [
          { id: "ws-test", name: "Isolated E2E", path: "/tmp/pua-e2e" },
          { id: "ws-alpha", name: "Alpha Projects", path: "/tmp/pua-alpha" },
          { id: "ws-beta", name: "Beta Projects", path: "/tmp/pua-beta" },
        ],
        agentProfiles: [{ key: "default", agentName: "test-agent" }],
      }),
    });
  });
  await page.goto("/w/ws-test/r/project1");

  await page.getByRole("button", { name: "Open navigation" }).click();
  await page.locator("#systemSettingsButton").click();
  const settings = page.getByRole("dialog", { name: "System Settings" });
  const entries = settings.locator(".settings-workspace-entry");
  await expect(entries).toHaveCount(3);

  // The settings content viewport must not grow a horizontal scrollbar.
  const content = settings.locator(".settings-content");
  const overflow = await content.evaluate((node) => node.scrollWidth - node.clientWidth);
  expect(overflow).toBeLessThanOrEqual(1);

  // Every workspace card action (icon picker, rename, remove) stays fully
  // inside the modal content area instead of being clipped past its left edge.
  const contentBox = (await content.boundingBox())!;
  for (const entry of await entries.all()) {
    for (const action of await entry.locator(".settings-row-actions button").all()) {
      const box = await action.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(contentBox.x);
      expect(box!.x + box!.width).toBeLessThanOrEqual(contentBox.x + contentBox.width + 1);
    }
  }
});
