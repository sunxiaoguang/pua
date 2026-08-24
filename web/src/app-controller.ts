import type { AgentEvent, AgentPanelHeaderModel, ComposerContext, ComposerModel, EventTimelineModel, ResourceMessageStatus, TimelineItem, UploadDialogModel } from "./models/chat";
import type { ToastModel } from "./models/common";
import type { CreateDialogModel, TaskTemplate } from "./models/create";
import type { DetailPanelModel, SchedulerMutationCallbacks } from "./models/detail";
import type { SettingsModel } from "./models/settings";
import type { AppShellModel, DoctorSnapshotModel, ShellActivityItem, ShellActivityLists, ShellDragTarget, ShellInboxMessage, ShellResourceItem, ShellStatusPresentation } from "./models/shell";
import type { AgentConfig, AgentProfile, DiffRecord, ResourceRecord, WorkspaceConfig, WorkspaceFileRecord, WorkspaceTree, WorkspaceUser } from "./models/workspace";
import type { ApiErrorResponse, ArchiveResponse } from "./api/types";
import { createAgentDraftController } from "./controllers/agent-draft-controller";
import { createAgentOperationController } from "./controllers/agent-operation-controller";
import { confirmDialog } from "./controllers/confirm-dialog-controller";
import { createCreateDialogController } from "./controllers/create-dialog-controller";
import { doctorSnapshotForWorkspace } from "./controllers/doctor-projection";
import { createNotificationController, type NotificationSource } from "./controllers/notification-controller";
import { createPaneLayoutController } from "./controllers/pane-layout-controller";
import { createThemeController } from "./controllers/theme-controller";
import { createResourceDetailController } from "./controllers/resource-detail-controller";
import { createRouteController } from "./controllers/route-controller";
import { createSchedulerMutationController, type SchedulerMutationLease } from "./controllers/scheduler-mutation-controller";
import { createSettingsController, type AgentHubData } from "./controllers/settings-controller";
import { createShellProjection } from "./controllers/shell-projection";
import {
	createSidebarFolderId,
	foldersForProject,
	moveSidebarItem,
	sanitizeSidebarFolderName,
	sidebarFolderById,
	sidebarFolderTaskIds,
	sidebarProjectRootIds,
	SIDEBAR_FOLDER_DEFAULT_NAME,
	type SidebarFolder,
} from "./controllers/sidebar-folders";
import { createUserSettingsController, validateUserName } from "./controllers/user-settings-controller";
import { ApiError } from "./api/client";
import { errorMessage } from "./runtime/errors";
import { ResourceScope } from "./runtime/resource-scope";
import { projectConversationEvents } from "./components/timeline-events";

export interface PUAViewPublisher {
  renderAppShell(model: AppShellModel): void;
  renderCreateDialog(model: CreateDialogModel): void;
  renderSettings(model: SettingsModel): void;
  renderUploadDialog(model: UploadDialogModel): void;
  renderComposer(model: ComposerModel): void;
  renderEventTimeline(model: EventTimelineModel): void;
  renderAgentPanelHeader(model: AgentPanelHeaderModel): void;
  renderDetailPanel(model: DetailPanelModel): void;
  renderToast(model: ToastModel): void;
}

let publisher: PUAViewPublisher;
let lifecycle: ResourceScope | null = null;

interface InboxMessageRecord {
	messageId: string;
	text: string;
	sourceResourceId: string;
	senderName?: string;
	createdAt?: string;
	readAt?: string;
	repliedAt?: string;
	unread?: boolean;
}

interface ControllerState {
	config: WorkspaceConfig | null;
	settingsRevision: string;
	doctor: DoctorSnapshotModel;
	tree: WorkspaceTree | null;
	inbox: InboxMessageRecord[];
	details: Record<string, ResourceRecord>;
	workspaceAgents: WorkspaceFileRecord | null;
	workspaceUsers: WorkspaceUser[];
	currentUserName: string;
	userGate: { mode: "" | "create" | "select" | "loading"; suggestedUserName: string; missingUserName: string };
	activeWorkspaceId: string;
	navigationLoading: boolean;
	navigationError: string;
	workspaceMenuOpen: boolean;
	selectedId: string;
	lastResourceId: string;
	expandedProjects: Set<string>;
	projectOrder: string[];
	taskOrder: Record<string, string[]>;
	folders: SidebarFolder[];
	folderOrder: Record<string, string[]>;
	treeEditing: boolean;
	listDrag: ShellDragTarget | null;
	expandedPaths: Set<string>;
	diff: DiffRecord | null;
	modalEnter: string;
	taskOperationalStateKey: string;
	uploadDialog: { open: boolean; identity: number; resourceId: string; items: unknown[]; nextId: number };
	autoRefreshTimer: number | null;
	autoRefreshInFlight: boolean;
	autoRefreshVersion: number;
	treeRequestVersion: number;
	navigationVersion: number;
	detailRequestVersion: number;
	workspaceAgentsRequestVersion: number;
	diffRequestVersion: number;
	messageStatus: ResourceMessageStatus | null;
	messageStatusKey: string;
	messageStatusRequestVersion: number;
	steeringMessageId: string;
	stopNotice: { key: string; text: string } | null;
	agent: {
		renderTimer: number | null;
		draftPrompt: string;
		chatDraft: string;
		chatMultiline: boolean;
		chatDraftKey: string;
		chatDraftWorkspaceId: string;
		chatDraftResourceId: string;
		chatDraftVersion: number;
		chatDraftResetVersion: number;
		skipChatDraftSync: boolean;
		agentName: string;
		optionsOpen: boolean;
		historyOpen: boolean;
		toolGroupOpen: Map<string, boolean>;
		approvalDrafts: Map<string, Record<string, unknown>>;
		renderDeferredForSelection: boolean;
	};
}

const controllerState: ControllerState = {
	config: null,
	settingsRevision: "",
	doctor: { checking: true, complete: false, summary: { errors: 0, warnings: 0 }, workspaces: [] },
	tree: null,
	inbox: [] as InboxMessageRecord[],
	details: {},
	workspaceAgents: null,
	workspaceUsers: [],
	currentUserName: "",
	userGate: { mode: "", suggestedUserName: "", missingUserName: "" },
	activeWorkspaceId: "",
	navigationLoading: true,
	navigationError: "",
	workspaceMenuOpen: false,
	selectedId: "",
	lastResourceId: "",
	expandedProjects: /* @__PURE__ */ new Set<string>(),
	projectOrder: [] as string[],
	taskOrder: {} as Record<string, string[]>,
	folders: [] as SidebarFolder[],
	folderOrder: {} as Record<string, string[]>,
	treeEditing: false,
	listDrag: null as ShellDragTarget | null,
	expandedPaths: /* @__PURE__ */ new Set<string>(),
	diff: null,
	modalEnter: "",
	taskOperationalStateKey: "",
	uploadDialog: {
		open: false,
		identity: 0,
			resourceId: "",
		items: [],
		nextId: 1
	},
		autoRefreshTimer: null as number | null,
	autoRefreshInFlight: false,
	autoRefreshVersion: 0,
	treeRequestVersion: 0,
	navigationVersion: 0,
	detailRequestVersion: 0,
	workspaceAgentsRequestVersion: 0,
	diffRequestVersion: 0,
	messageStatus: null,
	messageStatusKey: "",
	messageStatusRequestVersion: 0,
	steeringMessageId: "",
	stopNotice: null,
	agent: {
		renderTimer: null as number | null,
		draftPrompt: "",
		chatDraft: "",
		chatMultiline: false,
		chatDraftKey: "",
		chatDraftWorkspaceId: "",
		chatDraftResourceId: "",
		chatDraftVersion: 0,
		chatDraftResetVersion: 0,
		skipChatDraftSync: false,
		agentName: "",
		optionsOpen: false,
		historyOpen: false,
		toolGroupOpen: /* @__PURE__ */ new Map<string, boolean>(),
		approvalDrafts: /* @__PURE__ */ new Map<string, Record<string, unknown>>(),
		renderDeferredForSelection: false
	}
};

function clearResourceDetailState() {
	for (const id of Object.keys(controllerState.details)) delete controllerState.details[id];
}

const agentDraftController = createAgentDraftController({
	runtime: controllerState.agent,
	workspaceId: () => controllerState.activeWorkspaceId,
});
const clearResourceDraftAfterAccepted = agentDraftController.clearResourceAfterAccepted;
const clearAgentDraftMemory = agentDraftController.clearMemory;
const flushAgentDraft = agentDraftController.flush;
const restoreAgentDraftForResource = agentDraftController.restoreResource;
const updateAgentDraft = agentDraftController.update;

const agentOperations = createAgentOperationController(() => {
	if (!appBooted) return;
	renderChatPanel();
});
const paneLayoutController = createPaneLayoutController(() => renderAppShell());
const themeController = createThemeController(() => renderAppShell());
const routeController = createRouteController(() => renderAppShell());
const resourceDetailController = createResourceDetailController({
	details: controllerState.details,
	context: () => ({
		workspaceId: controllerState.activeWorkspaceId,
		navigationVersion: controllerState.navigationVersion,
		selectedId: controllerState.selectedId,
		detailRequestVersion: controllerState.detailRequestVersion
	}),
	nextDetailRequestVersion: () => ++controllerState.detailRequestVersion,
	isCurrentWorkspace: (workspaceId, navigationVersion) => isCurrentWorkspaceView(workspaceId, navigationVersion),
	request: (path, init) => api(path, init),
});
const schedulerMutationController = createSchedulerMutationController({
	context: () => ({
		workspaceId: controllerState.activeWorkspaceId,
		navigationVersion: controllerState.navigationVersion,
		selectedId: controllerState.selectedId,
	}),
	isCurrentWorkspace: (workspaceId, navigationVersion) => isCurrentWorkspaceView(workspaceId, navigationVersion),
	resolveResourceTitle: (resourceId) => resolveResourceTitle(resourceId),
	request: (path, init) => api(path, init),
});
const createDialogController = createCreateDialogController({
	workspaceId: () => controllerState.activeWorkspaceId,
	templates: (projectId) => controllerState.details[projectId]?.templates || [],
	request: (path, init) => api(path, init),
	publish: (model) => publisher.renderCreateDialog(model),
	toast,
	reloadTree: () => loadTree(),
	selectResource: (resourceId) => selectResource(resourceId),
	onOpen: () => {
		controllerState.modalEnter = "create";
	},
	confirmTemplateSwitch: () => confirmDialog({ title: "Switch template", message: "Discard edited template fields and switch templates?", confirmLabel: "Discard", danger: true }),
	agents: () => svelteAgentOptions(),
	agentProfiles: () => (controllerState.config?.agentProfiles || []).map((profile) => ({ key: profile.key, description: profile.description, agentName: profile.agentName })),
	defaultTaskBinding: (projectId) => {
		const projectDefault = controllerState.details[projectId]?.taskDefault;
		if (projectDefault?.name) return projectDefault;
		return controllerState.tree?.resourceDefaults?.task || { kind: "profile" as const, name: "default" };
	},
	currentUserName
});
const elementById = <ElementType extends HTMLElement = HTMLElement>(id: string): ElementType | null => document.getElementById(id) as ElementType | null;
const AUTO_REFRESH_INTERVAL_MS = 5e3;
interface LoadTreeOptions { updateURL?: boolean; replaceURL?: boolean }
interface LoadDetailOptions { force?: boolean }
interface FetchDetailOptions {}
interface WorkspaceAgentsOptions { force?: boolean }
interface SelectResourceOptions { clearUnread?: boolean; forceDetail?: boolean }
interface RenderOptions { skipDraftSync?: boolean }
interface UploadContext { workspaceId?: string; resourceId?: string }
interface WorkspaceIconOption { id: string; label: string; src: string; type?: string }
const DEFAULT_WORKSPACE_ICON: WorkspaceIconOption = {
	id: "",
	label: "PUA default",
	src: "/favicon.svg",
	type: "image/svg+xml"
};
const WORKSPACE_ICONS: WorkspaceIconOption[] = [
	{
		id: "home-base",
		label: "Home base",
		src: "/workspace-icons/01-home-base.png"
	},
	{
		id: "personal-tasks",
		label: "Personal tasks",
		src: "/workspace-icons/02-personal-tasks.png"
	},
	{
		id: "product-roadmap",
		label: "Product roadmap",
		src: "/workspace-icons/03-product-roadmap.png"
	},
	{
		id: "software-engineering",
		label: "Software engineering",
		src: "/workspace-icons/04-software-engineering.png"
	},
	{
		id: "design-studio",
		label: "Design studio",
		src: "/workspace-icons/05-design-studio.png"
	},
	{
		id: "marketing-campaign",
		label: "Marketing campaign",
		src: "/workspace-icons/06-marketing-campaign.png"
	},
	{
		id: "sales-pipeline",
		label: "Sales pipeline",
		src: "/workspace-icons/07-sales-pipeline.png"
	},
	{
		id: "operations",
		label: "Operations",
		src: "/workspace-icons/08-operations.png"
	},
	{
		id: "finance",
		label: "Finance",
		src: "/workspace-icons/09-finance.png"
	},
	{
		id: "research-lab",
		label: "Research lab",
		src: "/workspace-icons/10-research-lab.png"
	},
	{
		id: "learning-education",
		label: "Learning and education",
		src: "/workspace-icons/11-learning-education.png"
	},
	{
		id: "customer-support",
		label: "Customer support",
		src: "/workspace-icons/12-customer-support.png"
	},
	{
		id: "events-calendar",
		label: "Events and calendar",
		src: "/workspace-icons/13-events-calendar.png"
	},
	{
		id: "documentation-knowledge",
		label: "Documentation and knowledge",
		src: "/workspace-icons/14-documentation-knowledge.png"
	},
	{
		id: "analytics",
		label: "Analytics",
		src: "/workspace-icons/15-analytics.png"
	},
	{
		id: "community-team",
		label: "Community and team",
		src: "/workspace-icons/16-community-team.png"
	}
];
const WORKSPACE_ICON_BY_ID = new Map(WORKSPACE_ICONS.map((item) => [item.id, item]));
const {
	applyCustomOrder,
	archiveRedirectTarget,
	aggregatedUnreadCount,
	moveIdInList,
	projectTaskSummary,
	resourceRefText,
	statusModel: appShellStatusModel,
	noTaskOperationalState,
	resourceNavigationState,
	taskOperationalState,
	taskWorkflowState,
	taskOperationalStateKey,
} = createShellProjection({
	tree: () => controllerState.tree,
	findResource: (id) => findResource(id),
	agentName: (agentId) => (controllerState.config?.agents || []).find((agent) => agent.id === agentId)?.name || agentId || "PUA",
});
let uploadDialogIdentity = 0;
const settingsController = createSettingsController({
	config: () => controllerState.config || { workspaces: [], agents: [], agentProfiles: [] },
	setConfig: (config) => {
		controllerState.config = config;
		if (config.revision) controllerState.settingsRevision = config.revision;
	},
	activeWorkspaceId: () => controllerState.activeWorkspaceId,
	setActiveWorkspaceId: (id) => { controllerState.activeWorkspaceId = id; },
	selectWorkspaceResource: () => { controllerState.selectedId = "workspace"; },
	request: (path, init) => api(path, init),
	publish: (model) => publisher.renderSettings(model),
	agentOptions: svelteAgentOptions,
	workspaceIcons: [DEFAULT_WORKSPACE_ICON, ...WORKSPACE_ICONS],
	appearance: () => {
		const snapshot = paneLayoutController.snapshot();
		return {
			layout: snapshot.layout.preference,
			fontScales: snapshot.fontScales,
			theme: themeController.theme(),
			themeOptions: themeController.options()
		};
	},
	setLayoutPreference: (preference) => paneLayoutController.setLayoutPreference(preference),
	setFontScale: (column, value) => paneLayoutController.setFontScale(column, value),
	resetFontScales: () => paneLayoutController.resetFontScales(),
	setThemePreference: (theme) => themeController.setTheme(theme),
	notificationPreferences: () => notificationController?.preferences() || { browser: false, sound: false, permission: "unsupported", permissionError: "", soundError: "" },
	setBrowserNotifications: (enabled) => notificationController?.setBrowserEnabled(enabled),
	setCompletionSound: (enabled) => notificationController?.setSoundEnabled(enabled),
	flushDraft: flushAgentDraft,
	resetAgentState,
	reloadWorkspaceContext: async (initialUserName) => {
		if (initialUserName) selectWorkspaceUser(initialUserName);
		await loadWorkspaceContext();
	},
	clearWorkspaceContext: () => {
		controllerState.tree = null;
		controllerState.workspaceUsers = [];
		clearResourceDetailState();
		publishViewModels();
	},
	renderWorkspace: renderWorkspaceSelect,
	renderAgentViews: () => { applyAgentConfig(); renderChatComposer(); },
	toast
});
function svelteAgentOptions() {
	return enabledAgentConfigs().map((agent) => ({
		id: agent.id || "",
		label: agentDisplayName(agent),
		summary: agentConfigSummary(agent)
	}));
}
function publishAllViewModels() {
	renderAppShell();
	renderDetails();
	renderCreateDialog();
	renderAgentUploadDialog();
	renderChatComposer();
	renderChatPanel();
	renderSettingsModal();
}
let notificationController: ReturnType<typeof createNotificationController> | null = null;
let userSettingsController: ReturnType<typeof createUserSettingsController> | null = null;
function initializeNotificationState(workspaceId: string): void {
	notificationController?.initialize(workspaceId);
}
function establishNotificationBaseline() {
	notificationController?.establishBaseline();
}
function resourceNotificationProjections(tree: WorkspaceTree | null = controllerState.tree): NotificationSource[] {
	if (!tree) return [];
	const projections: NotificationSource[] = [];
	const append = (resource: ResourceRecord | null | undefined): void => {
		const runtime = resource?.runtime;
		if (!resource || !runtime?.generationId || !runtime.completionMarker) return;
		projections.push({
			id: runtime.generationId,
			resourceId: resource.id,
			title: resource.title || resource.id,
			generationId: runtime.generationId,
			completionMarker: runtime.completionMarker,
			completionState: runtime.completionState || "completed",
			completionAt: runtime.completionAt,
			status: runtime.status
		});
	};
	append(tree.scheduler);
	for (const project of tree.projects || []) {
		append(project);
		for (const task of project.children || []) append(task);
	}
	return projections;
}
function observeCompletionProjections(items: NotificationSource[]): void {
	notificationController?.observeProjections(items);
}
function observeCompletionEvent(event: AgentEvent, source: NotificationSource | null): void {
	if (source) notificationController?.observeEvent(event, source);
}
function clearUnreadForResource(resourceId: string): void {
	notificationController?.clearResource(resourceId);
}
function currentUserName() {
	return controllerState.currentUserName;
}
async function api<Response>(path: string, options: RequestInit = {}): Promise<Response> {
	const headers = new Headers(options.headers);
	if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
	if (path.startsWith("/api/workspaces/") && currentUserName()) headers.set("X-PUA-User", currentUserName());
	const response = await fetch(path, {
		...options,
		headers
	});
	if (!response.ok) {
		let message = `${response.status} ${response.statusText}`;
		let body: ApiErrorResponse | undefined;
		try {
			body = await response.json() as ApiErrorResponse;
			message = body.error || message;
		} catch (_) {}
		throw new ApiError(response.status, message, body);
	}
	if (response.status === 204) return null as Response;
	return response.json() as Promise<Response>;
}

async function registerWorkspaceUser(workspaceId: string, name: string): Promise<void> {
	if (!workspaceId) return;
	await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/users`, {
		method: "POST",
		body: JSON.stringify({ name })
	});
	await loadWorkspaceUsers(workspaceId);
}

function activeWorkspaceInstanceId(): string {
	const workspace = controllerState.config?.workspaces.find((item) => item.id === controllerState.activeWorkspaceId);
	return String(workspace?.instanceId || workspace?.id || "");
}

function selectWorkspaceUser(name: string): string {
	const selected = validateUserName(name);
	const instanceId = activeWorkspaceInstanceId();
	if (!userSettingsController || !instanceId) throw new Error("Workspace user selection is unavailable.");
	userSettingsController.save(instanceId, selected);
	controllerState.currentUserName = selected;
	controllerState.userGate = { mode: "", suggestedUserName: "", missingUserName: "" };
	return selected;
}

function beginWorkspaceUserTransition(name: string): void {
	controllerState.navigationVersion++;
	schedulerMutationController.invalidate();
	controllerState.autoRefreshVersion++;
	controllerState.treeRequestVersion++;
	controllerState.detailRequestVersion++;
	selectWorkspaceUser(name);
	controllerState.tree = null;
	controllerState.inbox = [];
	clearResourceDetailState();
	controllerState.navigationLoading = true;
	controllerState.navigationError = "";
	controllerState.userGate = { mode: "loading", suggestedUserName: "", missingUserName: "" };
	publishViewModels();
}

function finishWorkspaceUserTransition(): void {
	if (controllerState.userGate.mode !== "loading") return;
	controllerState.userGate = { mode: "", suggestedUserName: "", missingUserName: "" };
	publishViewModels();
}

async function resolveWorkspaceIdentity(workspaceId = controllerState.activeWorkspaceId): Promise<boolean> {
	await loadWorkspaceUsers(workspaceId);
	if (workspaceId !== controllerState.activeWorkspaceId) return false;
	const instanceId = activeWorkspaceInstanceId();
	const users = controllerState.workspaceUsers;
	const saved = userSettingsController?.selected(instanceId) || "";
	const existingNames = new Set(users.map((user) => user.name));
	let selected = existingNames.has(saved) ? saved : "";
	if (!selected && !saved) {
		const legacy = userSettingsController?.legacyCandidate() || "";
		if (existingNames.has(legacy)) selected = legacy;
	}
	if (!selected && users.length === 1) selected = users[0].name;
	if (selected) {
		if (controllerState.currentUserName && controllerState.currentUserName !== selected) {
			await saveUIState().catch((err) => console.warn("failed to save UI state before switching user", err));
			beginWorkspaceUserTransition(selected);
		} else selectWorkspaceUser(selected);
		if (saved && saved !== selected) toast(`User ${saved} is no longer available. Switched to ${selected}.`);
		return true;
	}
	if (controllerState.currentUserName) {
		await saveUIState().catch((err) => console.warn("failed to save UI state before clearing user", err));
		controllerState.navigationVersion++;
		schedulerMutationController.invalidate();
		controllerState.autoRefreshVersion++;
		controllerState.treeRequestVersion++;
		controllerState.detailRequestVersion++;
	}
	controllerState.currentUserName = "";
	controllerState.tree = null;
	controllerState.inbox = [];
	clearResourceDetailState();
	controllerState.navigationLoading = false;
	controllerState.navigationError = "";
	controllerState.userGate = {
		mode: users.length ? "select" : "create",
		suggestedUserName: String(controllerState.config?.suggestedUserName || ""),
		missingUserName: saved,
	};
	publishViewModels();
	return false;
}

async function loadWorkspaceContext(): Promise<void> {
	if (!controllerState.activeWorkspaceId) return;
	if (!await resolveWorkspaceIdentity()) return;
	try {
		await loadUIState();
		controllerState.selectedId = controllerState.lastResourceId || controllerState.selectedId || "workspace";
		await loadTree();
	} finally {
		finishWorkspaceUserTransition();
	}
}

async function resolveWorkspaceUser(name: string, create: boolean): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	if (!workspaceId) return;
	if (create) await registerWorkspaceUser(workspaceId, validateUserName(name));
	beginWorkspaceUserTransition(name);
	try {
		await loadUIState();
		controllerState.selectedId = controllerState.lastResourceId || "workspace";
		await loadTree();
	} finally {
		finishWorkspaceUserTransition();
	}
}

async function loadWorkspaceUsers(workspaceId = controllerState.activeWorkspaceId): Promise<void> {
	if (!workspaceId) {
		controllerState.workspaceUsers = [];
		return;
	}
	const result = await api<{ users?: WorkspaceUser[] }>(`/api/workspaces/${encodeURIComponent(workspaceId)}/users`);
	if (workspaceId === controllerState.activeWorkspaceId) controllerState.workspaceUsers = result.users || [];
}

async function fetchDoctorSnapshot(): Promise<DoctorSnapshotModel> {
	try {
		return await api<DoctorSnapshotModel>("/api/doctor");
	} catch (err) {
		return {
			checking: false,
			complete: false,
			summary: { errors: 0, warnings: 0 },
			error: errorMessage(err),
			workspaces: [],
		};
	}
}

// fetchSettingsRevision is the cheap auto-refresh probe for serve settings
// changes. It returns an empty string when the server predates the revision
// endpoint or the request fails, in which case polling is skipped silently.
async function fetchSettingsRevision(): Promise<string> {
	try {
		const response = await api<{ revision?: string }>("/api/settings/revision");
		return String(response.revision || "");
	} catch {
		return "";
	}
}

// refreshServerSettings reloads the workspace configuration and AgentHub
// catalog after the settings revision changed, so profile routes, workspace
// names, and agent availability edited from another tab or client propagate
// without a page reload.
async function refreshServerSettings(): Promise<void> {
	const [base, agentHub] = await Promise.all([
		api<WorkspaceConfig>("/api/workspaces"),
		api<AgentHubData>("/api/settings/agenthub"),
	]);
	controllerState.config = configWithAgentHubCatalog(base, agentHub);
	controllerState.settingsRevision = String(base.revision || "");
	applyAgentConfig();
	renderWorkspaceSelect();
	await settingsController.externalSync();
}

async function requestDoctorRefresh(): Promise<void> {
	if (controllerState.doctor.checking) return;
	controllerState.doctor = { ...controllerState.doctor, checking: true };
	renderAppShell();
	const response = await fetch("/api/doctor", { method: "POST" });
	if (!response.ok) {
		controllerState.doctor = { ...controllerState.doctor, checking: false, error: `${response.status} ${response.statusText}` };
		renderAppShell();
	}
}

async function load() {
	const route = parseRoute();
	const [base, agentHub, doctor] = await Promise.all([
		api<WorkspaceConfig>("/api/workspaces"),
		api<AgentHubData>("/api/settings/agenthub"),
		fetchDoctorSnapshot(),
	]);
	controllerState.config = configWithAgentHubCatalog(base, agentHub);
	controllerState.settingsRevision = String(base.revision || "");
	controllerState.doctor = doctor;
	applyAgentConfig();
	controllerState.activeWorkspaceId = workspaceExists(route.workspaceId) ? route.workspaceId || "" : controllerState.config?.activeId || controllerState.config?.workspaces[0]?.id || "";
	controllerState.selectedId = route.resourceId || "workspace";
	renderWorkspaceSelect();
	if (controllerState.activeWorkspaceId) {
		initializeNotificationState(controllerState.activeWorkspaceId);
		if (await resolveWorkspaceIdentity()) {
			await loadUIState();
			if (!route.resourceId && controllerState.lastResourceId) controllerState.selectedId = controllerState.lastResourceId;
			await loadTree({ replaceURL: true });
		}
	} else {
		controllerState.navigationLoading = false;
		controllerState.tree = null;
		clearResourceDetailState();
		controllerState.workspaceAgents = null;
		controllerState.workspaceUsers = [];
		controllerState.diff = null;
		resetAgentState();
		publishViewModels();
	}
}
async function loadTree(options: LoadTreeOptions = {}) {
	if (!controllerState.activeWorkspaceId) return;
	const workspaceId = controllerState.activeWorkspaceId;
	const navigationVersion = controllerState.navigationVersion;
	const treeRequestVersion = ++controllerState.treeRequestVersion;
	controllerState.navigationLoading = true;
	controllerState.navigationError = "";
	renderAppShell();
	controllerState.detailRequestVersion++;
	controllerState.workspaceAgentsRequestVersion++;
	controllerState.diffRequestVersion++;
	let tree: WorkspaceTree;
	try {
		tree = await api(`/api/workspaces/${workspaceId}/tree`);
	} catch (err) {
		if (err instanceof ApiError && (err.code === "user_required" || err.code === "user_not_found")) {
			await loadWorkspaceContext();
			return;
		}
		if (isCurrentWorkspaceView(workspaceId, navigationVersion, treeRequestVersion)) {
			controllerState.navigationLoading = false;
			controllerState.navigationError = errorMessage(err);
			renderAppShell();
		}
		throw err;
	}
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion, treeRequestVersion)) return;
	controllerState.tree = tree;
	clearResourceDetailState();
	controllerState.workspaceAgents = null;
	controllerState.diff = null;
	ensureValidSelection();
	ensureSelectedProjectExpanded(false);
	if (controllerState.selectedId === "workspace") await loadWorkspaceAgents();
	else if (controllerState.selectedId) await loadDetail(controllerState.selectedId);
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion, treeRequestVersion)) return;
	// Resource details do not depend on the AgentHub-backed status snapshot.
	// Publish the tree and detail as soon as they are ready so a slow or
	// temporarily blocked status read cannot leave a refreshed page showing
	// "Loading details..." indefinitely.
	controllerState.navigationLoading = false;
	controllerState.navigationError = "";
	publishViewModels();
	if (options.updateURL !== false) syncURL({ replace: Boolean(options.replaceURL) });
	await refreshResourceMessageStatus(workspaceId, selectedAgentResourceId());
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion, treeRequestVersion)) return;
	await refreshInbox(workspaceId);
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion, treeRequestVersion)) return;
	await markSelectedResourceRead();
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion, treeRequestVersion)) return;
	establishNotificationBaseline();
	controllerState.navigationLoading = false;
	controllerState.navigationError = "";
	publishViewModels();
	if (options.updateURL !== false) syncURL({ replace: Boolean(options.replaceURL) });
}
async function loadDetail(id: string, options: LoadDetailOptions = {}): Promise<ResourceRecord | null | undefined> {
	return resourceDetailController.load(id, options);
}
function fetchDetail(id: string, workspaceId = controllerState.activeWorkspaceId, _options: FetchDetailOptions = {}): Promise<ResourceRecord> {
	return resourceDetailController.fetch(id, workspaceId);
}
function resourceDetailSnapshot(resourceId: string): ReturnType<typeof resourceDetailController.snapshot> {
	return resourceDetailController.snapshot(resourceId);
}
function applyResourceDetail(detail: ResourceRecord): ResourceRecord | null {
	return resourceDetailController.apply(detail);
}
async function loadWorkspaceAgents(options: WorkspaceAgentsOptions = {}) {
	if (!controllerState.activeWorkspaceId || controllerState.workspaceAgents && !options.force) return;
	const workspaceId = controllerState.activeWorkspaceId;
	const navigationVersion = controllerState.navigationVersion;
	const requestVersion = ++controllerState.workspaceAgentsRequestVersion;
	try {
		const agents = await api<WorkspaceFileRecord>(`/api/workspaces/${workspaceId}/files?path=${encodeURIComponent("AGENTS.md")}`);
		if (!isCurrentWorkspaceView(workspaceId, navigationVersion) || requestVersion !== controllerState.workspaceAgentsRequestVersion) return null;
		controllerState.workspaceAgents = agents;
	} catch (err) {
		if (!isCurrentWorkspaceView(workspaceId, navigationVersion) || requestVersion !== controllerState.workspaceAgentsRequestVersion) return null;
		controllerState.workspaceAgents = {
			path: "AGENTS.md",
			name: "AGENTS.md",
			error: errorMessage(err)
		};
	}
	return controllerState.workspaceAgents;
}
async function loadUIState(workspaceId = controllerState.activeWorkspaceId, navigationVersion = controllerState.navigationVersion) {
	const uiState = await api<{ expandedProjects?: string[]; lastResourceId?: string; projectOrder?: string[]; taskOrder?: Record<string, string[]>; folders?: SidebarFolder[]; folderOrder?: Record<string, string[]> }>(`/api/workspaces/${workspaceId}/ui-state`);
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion)) return false;
	controllerState.expandedProjects = new Set(uiState.expandedProjects || []);
	controllerState.lastResourceId = uiState.lastResourceId || "";
	controllerState.projectOrder = Array.isArray(uiState.projectOrder) ? uiState.projectOrder : [];
	controllerState.taskOrder = uiState.taskOrder && typeof uiState.taskOrder === "object" ? uiState.taskOrder : {};
	controllerState.folders = sanitizeStoredFolders(uiState.folders);
	controllerState.folderOrder = uiState.folderOrder && typeof uiState.folderOrder === "object" ? uiState.folderOrder : {};
	return true;
}
function sanitizeStoredFolders(folders: unknown): SidebarFolder[] {
	if (!Array.isArray(folders)) return [];
	const seen = new Set<string>();
	const kept: SidebarFolder[] = [];
	for (const candidate of folders) {
		if (!candidate || typeof candidate !== "object") continue;
		const raw = candidate as Partial<SidebarFolder>;
		const id = String(raw.id || "");
		const projectId = String(raw.projectId || "");
		if (!id || !projectId || seen.has(id)) continue;
		seen.add(id);
		kept.push({ id, projectId, name: sanitizeSidebarFolderName(raw.name) || SIDEBAR_FOLDER_DEFAULT_NAME, expanded: Boolean(raw.expanded) });
	}
	return kept;
}
async function saveUIState() {
	if (!controllerState.activeWorkspaceId) return;
	const workspaceId = controllerState.activeWorkspaceId;
	const navigationVersion = controllerState.navigationVersion;
	const selectedId = controllerState.selectedId;
	await api(`/api/workspaces/${workspaceId}/ui-state`, {
		method: "PUT",
		body: JSON.stringify({
			version: 1,
			expandedProjects: [...controllerState.expandedProjects],
			lastResourceId: selectedId,
			projectOrder: controllerState.projectOrder,
			taskOrder: controllerState.taskOrder,
			folders: controllerState.folders,
			folderOrder: controllerState.folderOrder
		})
	});
	if (isCurrentWorkspaceView(workspaceId, navigationVersion)) controllerState.lastResourceId = selectedId;
}
function startAutoRefresh() {
	if (controllerState.autoRefreshTimer) return;
	controllerState.autoRefreshTimer = lifecycle?.interval(() => {
		autoRefresh().catch((err) => {
			console.warn("auto refresh failed", err);
		});
	}, AUTO_REFRESH_INTERVAL_MS) ?? null;
}
async function autoRefresh() {
	if (!controllerState.activeWorkspaceId || controllerState.userGate.mode || controllerState.autoRefreshInFlight || controllerState.listDrag) return;
	const refreshVersion = controllerState.autoRefreshVersion;
	const workspaceId = controllerState.activeWorkspaceId;
	const navigationVersion = controllerState.navigationVersion;
	let selectedId = controllerState.selectedId;
	controllerState.autoRefreshInFlight = true;
	try {
		const [tree, doctor, settingsRevision] = await Promise.all([fetchCurrentTree(workspaceId), fetchDoctorSnapshot(), fetchSettingsRevision()]);
		if (!tree || !isCurrentAutoRefresh(workspaceId, navigationVersion, refreshVersion)) return;
		let changed = !sameJSON(controllerState.tree, tree);
		if (changed) controllerState.tree = tree;
		if (!sameJSON(controllerState.doctor, doctor)) {
			controllerState.doctor = doctor;
			changed = true;
		}
		if (settingsRevision && controllerState.settingsRevision && settingsRevision !== controllerState.settingsRevision) {
			await refreshServerSettings();
			if (!isCurrentAutoRefresh(workspaceId, navigationVersion, refreshVersion)) return;
			changed = true;
		} else if (settingsRevision) {
			controllerState.settingsRevision = settingsRevision;
		}
		observeCompletionProjections(resourceNotificationProjections(tree));
		if (ensureValidSelection()) {
			syncURL({ replace: true });
			changed = true;
			selectedId = controllerState.selectedId;
		}
		const expandedCount = controllerState.expandedProjects.size;
		ensureSelectedProjectExpanded(false);
		changed = changed || expandedCount !== controllerState.expandedProjects.size;
		if (controllerState.selectedId === "workspace") {
			const previousAgents = controllerState.workspaceAgents;
			await loadWorkspaceAgents({ force: true });
			if (!isCurrentAutoRefresh(workspaceId, navigationVersion, refreshVersion)) return;
			if (!sameJSON(previousAgents, controllerState.workspaceAgents)) changed = true;
		} else if (selectedId) {
			const detailRequestVersion = ++controllerState.detailRequestVersion;
			const detail = await fetchDetail(selectedId, workspaceId);
			if (!isCurrentAutoRefresh(workspaceId, navigationVersion, refreshVersion) || controllerState.selectedId !== selectedId || detailRequestVersion !== controllerState.detailRequestVersion) return;
			const previousDetail = resourceDetailSnapshot(selectedId);
			applyResourceDetail(detail);
			if (!sameJSON(previousDetail, resourceDetailSnapshot(selectedId))) changed = true;
		}
		observeCompletionProjections(resourceNotificationProjections(tree));
		if (await refreshInbox(workspaceId)) changed = true;
		if (await refreshResourceMessageStatus(workspaceId, selectedAgentResourceId())) changed = true;
		if (taskOperationalStateKey() !== controllerState.taskOperationalStateKey) changed = true;
		if (changed) publishViewModels();
	} finally {
		controllerState.autoRefreshInFlight = false;
	}
}
function publishViewModels() {
	renderAppShell();
	renderDetails();
	renderChatPanel();
	renderCreateDialog();
	renderSettingsModal();
}
function renderSelectionPanels() {
	renderAppShell();
	renderDetails();
	renderChatPanel();
	renderCreateDialog();
}
function isCurrentWorkspaceView(workspaceId: string, navigationVersion: number, treeRequestVersion: number | null = null): boolean {
	return workspaceId === controllerState.activeWorkspaceId && navigationVersion === controllerState.navigationVersion && (treeRequestVersion == null || treeRequestVersion === controllerState.treeRequestVersion);
}
function isCurrentAutoRefresh(workspaceId: string, navigationVersion: number, refreshVersion: number): boolean {
	return isCurrentWorkspaceView(workspaceId, navigationVersion) && refreshVersion === controllerState.autoRefreshVersion;
}
function workspaceIconOption(workspace: { icon?: string } | null | undefined): WorkspaceIconOption {
	return WORKSPACE_ICON_BY_ID.get(String(workspace?.icon || "").trim()) || DEFAULT_WORKSPACE_ICON;
}
function updateWorkspaceFavicon(workspace: { icon?: string } | null | undefined): void {
	const option = workspaceIconOption(workspace);
	let link = document.querySelector<HTMLLinkElement>("link[rel=\"icon\"]");
	if (!link) {
		link = document.createElement("link");
		link.rel = "icon";
		document.head.appendChild(link);
	}
	link.type = "type" in option ? String(option.type || "image/png") : "image/png";
	link.href = option.src;
}
function renderWorkspaceSelect() {
	const active = controllerState.config?.workspaces?.find((workspace) => workspace.id === controllerState.activeWorkspaceId);
	updateWorkspaceFavicon(active);
	renderAppShell();
}
function appShellResourceModel(item: ResourceRecord, kind: "project" | "task", projectId = ""): ShellResourceItem {
	const taskState = kind === "task" ? taskWorkflowState(item) : resourceNavigationState(item);
	const expanded = kind === "project" && isProjectExpanded(item.id);
	const summary = kind === "project" ? projectTaskSummary(item) : null;
	const title = item.title || item.id;
	// A collapsed Project aggregates its Tasks' unread Turns into its own
	// badge; expanded Projects show only their own because each visible Task
	// renders its own badge.
	const unreadCount = kind === "project"
		? aggregatedUnreadCount(Number(item.unreadCount) || 0, item.children || [], expanded)
		: Number(item.unreadCount) || 0;
	return {
		id: item.id,
		type: kind,
		title,
		ref: resourceRefText(item.id),
		active: controllerState.selectedId === item.id,
		expanded,
		ariaLabel: [
			title,
			summary?.ariaLabel,
			taskState.label,
			unreadCount ? `${unreadCount} unread ${unreadCount === 1 ? "Turn" : "Turns"}` : ""
		].filter(Boolean).join(". "),
		statusLabel: taskState.label || "",
		status: appShellStatusModel(taskState.statusPresentation),
		summary: summary ? {
			taskLabel: summary.taskLabel,
			runningLabel: summary.runningLabel,
			ariaLabel: summary.ariaLabel
		} : null,
		children: kind === "project" ? appShellProjectChildrenModel(item) : [],
		projectId,
		unreadCount
	};
}
// appShellProjectChildrenModel builds the mixed root list of a Project:
// virtual folders and ungrouped Tasks interleaved in the stored custom order.
function appShellProjectChildrenModel(project: ResourceRecord): ShellResourceItem[] {
	const tasks = project.children || [];
	const taskById = new Map(tasks.map((task) => [task.id, task]));
	const folderById = new Map(foldersForProject(controllerState.folders, project.id).map((folder) => [folder.id, folder]));
	const rootIds = sidebarProjectRootIds(
		{ taskOrder: controllerState.taskOrder, folderOrder: controllerState.folderOrder },
		controllerState.folders,
		project.id,
		tasks.map((task) => task.id)
	);
	const children: ShellResourceItem[] = [];
	for (const id of rootIds) {
		const task = taskById.get(id);
		if (task) {
			children.push(appShellResourceModel(task, "task", project.id));
			continue;
		}
		const folder = folderById.get(id);
		if (folder) children.push(appShellFolderModel(folder, project, taskById));
	}
	return children;
}
function appShellFolderModel(folder: SidebarFolder, project: ResourceRecord, taskById: Map<string, ResourceRecord>): ShellResourceItem {
	const taskIds = sidebarFolderTaskIds(
		{ taskOrder: controllerState.taskOrder, folderOrder: controllerState.folderOrder },
		folder,
		(project.children || []).map((task) => task.id)
	);
	const children = taskIds.map((id) => taskById.get(id)).filter((task): task is ResourceRecord => Boolean(task)).map((task) => appShellResourceModel(task, "task", project.id));
	const title = folder.name || SIDEBAR_FOLDER_DEFAULT_NAME;
	// A collapsed folder aggregates the unread Turns of the Tasks it hides.
	const unreadCount = aggregatedUnreadCount(0, children, folder.expanded);
	return {
		id: folder.id,
		type: "folder",
		title,
		ref: "",
		active: false,
		expanded: folder.expanded,
		ariaLabel: [
			`Folder ${title}`,
			`${children.length} ${children.length === 1 ? "task" : "tasks"}`,
			unreadCount ? `${unreadCount} unread ${unreadCount === 1 ? "Turn" : "Turns"}` : ""
		].filter(Boolean).join(". "),
		statusLabel: "",
		status: appShellStatusModel(noTaskOperationalState().statusPresentation),
		summary: null,
		children,
		projectId: project.id,
		unreadCount
	};
}
function appShellSchedulerModel(item: ResourceRecord | null | undefined): ShellResourceItem | null {
	if (!item) return null;
	const state = resourceNavigationState(item);
	return {
		id: item.id || "scheduler",
		type: "scheduler",
		title: item.title || "Scheduler",
		ref: "",
		active: controllerState.selectedId === (item.id || "scheduler"),
		expanded: false,
		ariaLabel: ["Scheduler", state.label].filter(Boolean).join(". "),
		statusLabel: state.label || "Workspace Scheduler",
		status: appShellStatusModel(state.statusPresentation),
		summary: null,
		children: [],
		unreadCount: Number(item.unreadCount) || 0
	};
}
function appShellActivityModel(item: ResourceRecord, category: keyof ShellActivityLists): ShellActivityItem {
	const state = item.type === "task" && (item.state === "blocked" || item.state === "error") ? taskWorkflowState(item) : taskOperationalState(item);
	const type = item.type === "scheduler" || item.type === "project" || item.type === "task" ? item.type : "workspace";
	const title = item.title || item.id;
	return {
		id: item.id,
		type,
		title,
		ref: type === "project" || type === "task" ? resourceRefText(item.id) : "",
		selected: controllerState.selectedId === item.id,
		activeTurn: Boolean(item.runtime?.activeTurn),
		unreadCount: Number(item.unreadCount) || 0,
		turnNumber: category === "running" ? Number(item.runtime?.turnNumber) || 0 : Number(item.latestTurnNumber) || 0,
		agentName: String(item.runtime?.agentName || item.latestAgentName || "").trim(),
		statusLabel: state.label || (category === "unread" ? `${Number(item.unreadCount) || 0} unread` : "Active turn"),
		status: appShellStatusModel(state.statusPresentation)
	};
}
function appShellInboxModel(message: InboxMessageRecord): ShellInboxMessage {
	const resource = findResource(message.sourceResourceId || "");
	return {
		id: message.messageId,
		resourceId: message.sourceResourceId || "",
		resourceTitle: resource?.title || message.sourceResourceId || "",
		senderName: String(message.senderName || message.sourceResourceId || "").trim(),
		text: message.text || "",
		timeLabel: relativeTime(message.createdAt),
		unread: message.unread === true || !message.readAt,
		replied: Boolean(message.repliedAt)
	};
}
function renderAppShell() {
	const projects = controllerState.tree ? applyCustomOrder(controllerState.tree.projects || [], controllerState.projectOrder).map((project) => appShellResourceModel(project, "project")) : [];
	const activitySource = controllerState.tree?.activity;
	const activity: ShellActivityLists = {
		running: activitySource?.running?.map((item) => appShellActivityModel(item, "running")) || [],
		unread: activitySource?.unread?.map((item) => appShellActivityModel(item, "unread")) || [],
		problems: activitySource?.problems?.map((item) => appShellActivityModel(item, "problems")) || [],
	};
	// The inbox lists newest messages first; the Server returns them in
	// acceptance order.
	const inbox: ShellInboxMessage[] = (controllerState.inbox || []).slice().reverse().map(appShellInboxModel);
	if (controllerState.tree) controllerState.taskOperationalStateKey = taskOperationalStateKey();
	const workspaceState = resourceNavigationState(controllerState.tree?.workspace);
	publisher.renderAppShell({
		identity: controllerState.activeWorkspaceId || "no-workspace",
		loading: Boolean(controllerState.navigationLoading),
		error: controllerState.navigationError || "",
		version: "v0.1.0",
		activeWorkspaceId: controllerState.activeWorkspaceId,
		workspaceName: workspaceName(),
		userGate: { ...controllerState.userGate, users: controllerState.workspaceUsers.map((user) => ({ name: user.name, preference: user.preference })) },
		workspaces: (controllerState.config?.workspaces || []).map((workspace) => ({
			id: workspace.id,
			name: workspace.name || workspace.id,
			path: workspace.path || "",
			icon: workspace.icon || "",
			iconSrc: workspaceIconOption(workspace).src,
			status: workspace.id === controllerState.activeWorkspaceId ? appShellStatusModel(workspaceState.statusPresentation) : undefined,
			statusLabel: workspace.id === controllerState.activeWorkspaceId ? workspaceState.label : ""
		})),
		scheduler: appShellSchedulerModel(controllerState.tree?.scheduler),
		projects,
		treeEditing: controllerState.treeEditing,
		activity,
		inbox,
		doctor: doctorSnapshotForWorkspace(controllerState.doctor, controllerState.activeWorkspaceId),
		...paneLayoutController.snapshot(),
		route: routeController.projection(),
		resolveResourceTitle,
		onSwitchWorkspace: (id) => switchWorkspace(id),
		onResolveWorkspaceUser: (name, create) => resolveWorkspaceUser(name, create),
		onAddWorkspace: () => openSettings("workspace").catch((err) => toast(err.message)),
		onCreateProject: () => showProjectForm(),
		onOpenSettings: () => openSettings().catch((err) => toast(err.message)),
		onRefreshDoctor: requestDoctorRefresh,
		onToggleProject: (id) => toggleProject(id),
		onSelectResource: (id) => selectResource(id),
		onReorder: (drag, target, after) => commitListDrag(drag, target, after),
		onDragState: (drag) => {
			controllerState.listDrag = drag;
		},
		onToggleTreeEditing: () => toggleTreeEditing(),
		onCreateFolder: (projectId) => createFolder(projectId),
		onRenameFolder: (id, name) => renameFolder(id, name),
		onDeleteFolder: (id) => deleteFolder(id),
		onToggleFolder: (id) => toggleFolder(id),
		onOpenInboxMessage: (id) => openInboxMessage(id),
		onReplyInboxMessage: (id, text) => replyInboxMessage(id, text),
		onDeleteInboxMessage: (id) => deleteInboxMessage(id),
		onPanePreview: (name, value) => setPaneSize(name, value),
		onPaneCommit: (name) => savePaneSize(name),
		onPaneViewport: () => syncPaneViewport(),
		onMobileSidebar: (open) => setMobileSidebar(open),
		onHistoryNavigation: (pathname) => handleHistoryNavigation(pathname),
		onToast: toast
	});
}
async function switchWorkspace(id: string): Promise<void> {
	if (!workspaceExists(id)) return;
	controllerState.workspaceMenuOpen = false;
	if (id === controllerState.activeWorkspaceId) {
		renderWorkspaceSelect();
		return;
	}
	setMobileSidebar(false);
	flushAgentDraft();
	controllerState.navigationVersion++;
	schedulerMutationController.invalidate();
	controllerState.autoRefreshVersion++;
	controllerState.treeRequestVersion++;
	controllerState.detailRequestVersion++;
	controllerState.workspaceAgentsRequestVersion++;
	controllerState.diffRequestVersion++;
	const navigationVersion = controllerState.navigationVersion;
	await saveUIState().catch((err) => console.warn("failed to save UI state", err));
	controllerState.activeWorkspaceId = id;
	controllerState.selectedId = "workspace";
	controllerState.tree = null;
	controllerState.inbox = [];
	controllerState.treeEditing = false;
	controllerState.navigationLoading = true;
	controllerState.navigationError = "";
	clearResourceDetailState();
	initializeNotificationState(id);
	closeCreateDialog();
	resetAgentState();
	renderWorkspaceSelect();
	controllerState.currentUserName = "";
	controllerState.userGate = { mode: "", suggestedUserName: "", missingUserName: "" };
	if (!await resolveWorkspaceIdentity(id)) return;
	if (!await loadUIState(id, navigationVersion)) return;
	controllerState.selectedId = controllerState.lastResourceId || "workspace";
	await loadTree();
}
async function commitListDrag(drag: ShellDragTarget, target: ShellDragTarget, after: boolean): Promise<void> {
	const previous = {
		projectOrder: [...controllerState.projectOrder],
		taskOrder: Object.fromEntries(Object.entries(controllerState.taskOrder).map(([id, order]) => [id, Array.isArray(order) ? [...order] : []])),
		folderOrder: Object.fromEntries(Object.entries(controllerState.folderOrder).map(([id, order]) => [id, Array.isArray(order) ? [...order] : []]))
	};
	if (drag.kind === "project") {
		if (target.kind !== "project") return;
		const projects = applyCustomOrder(controllerState.tree?.projects || [], controllerState.projectOrder);
		controllerState.projectOrder = moveIdInList(projects.map((project) => project.id), drag.id, target.id, after);
	} else {
		const next = moveSidebarItem(
			{ taskOrder: controllerState.taskOrder, folderOrder: controllerState.folderOrder },
			controllerState.folders,
			projectTasksIndex(),
			drag,
			target,
			after
		);
		if (!next) return;
		controllerState.taskOrder = next.taskOrder;
		controllerState.folderOrder = next.folderOrder;
	}
	renderAppShell();
	try {
		await saveUIState();
	} catch (err) {
		controllerState.projectOrder = previous.projectOrder;
		controllerState.taskOrder = previous.taskOrder;
		controllerState.folderOrder = previous.folderOrder;
		renderAppShell();
		throw err;
	}
}
function projectTasksIndex(): Record<string, string[]> {
	const index: Record<string, string[]> = {};
	for (const project of controllerState.tree?.projects || []) {
		index[project.id] = (project.children || []).map((task) => task.id);
	}
	return index;
}
function snapshotFolderState(): { folders: SidebarFolder[]; taskOrder: Record<string, string[]>; folderOrder: Record<string, string[]> } {
	return {
		folders: controllerState.folders.map((folder) => ({ ...folder })),
		taskOrder: Object.fromEntries(Object.entries(controllerState.taskOrder).map(([id, order]) => [id, Array.isArray(order) ? [...order] : []])),
		folderOrder: Object.fromEntries(Object.entries(controllerState.folderOrder).map(([id, order]) => [id, Array.isArray(order) ? [...order] : []]))
	};
}
async function commitFolderChange(apply: () => void): Promise<void> {
	const previous = snapshotFolderState();
	apply();
	renderAppShell();
	try {
		await saveUIState();
	} catch (err) {
		controllerState.folders = previous.folders;
		controllerState.taskOrder = previous.taskOrder;
		controllerState.folderOrder = previous.folderOrder;
		renderAppShell();
		throw err;
	}
}
function toggleTreeEditing(): void {
	controllerState.treeEditing = !controllerState.treeEditing;
	renderAppShell();
}
async function createFolder(projectId: string): Promise<string> {
	const project = findResource(projectId);
	if (!project) throw new Error("Project is no longer available.");
	const folder: SidebarFolder = { id: createSidebarFolderId(), projectId, name: SIDEBAR_FOLDER_DEFAULT_NAME, expanded: true };
	await commitFolderChange(() => {
		controllerState.folders = [...controllerState.folders, folder];
		const rootIds = sidebarProjectRootIds(
			{ taskOrder: controllerState.taskOrder, folderOrder: controllerState.folderOrder },
			controllerState.folders,
			projectId,
			(project.children || []).map((task) => task.id)
		);
		controllerState.taskOrder = { ...controllerState.taskOrder, [projectId]: rootIds };
	});
	return folder.id;
}
async function renameFolder(id: string, name: string): Promise<void> {
	const folder = sidebarFolderById(controllerState.folders, id);
	if (!folder) return;
	const trimmed = sanitizeSidebarFolderName(name);
	if (!trimmed) throw new Error("Folder name is required.");
	if (trimmed === folder.name) return;
	await commitFolderChange(() => {
		controllerState.folders = controllerState.folders.map((candidate) => candidate.id === id ? { ...candidate, name: trimmed } : candidate);
	});
}
async function deleteFolder(id: string): Promise<void> {
	const folder = sidebarFolderById(controllerState.folders, id);
	if (!folder) return;
	const project = findResource(folder.projectId);
	await commitFolderChange(() => {
		const state = { taskOrder: controllerState.taskOrder, folderOrder: controllerState.folderOrder };
		const projectTaskIds = (project?.children || []).map((task) => task.id);
		const taskIds = sidebarFolderTaskIds(state, folder, projectTaskIds);
		const currentRootIds = sidebarProjectRootIds(state, controllerState.folders, folder.projectId, projectTaskIds);
		const index = currentRootIds.indexOf(id);
		// Ungrouped tasks return to the Project root where the folder used to be.
		const rootIds = currentRootIds.filter((rootId) => rootId !== id);
		rootIds.splice(index < 0 ? rootIds.length : index, 0, ...taskIds);
		controllerState.folders = controllerState.folders.filter((candidate) => candidate.id !== id);
		controllerState.taskOrder = { ...controllerState.taskOrder, [folder.projectId]: rootIds };
		const folderOrder = { ...controllerState.folderOrder };
		delete folderOrder[id];
		controllerState.folderOrder = folderOrder;
	});
}
async function toggleFolder(id: string): Promise<void> {
	const folder = sidebarFolderById(controllerState.folders, id);
	if (!folder) return;
	await commitFolderChange(() => {
		controllerState.folders = controllerState.folders.map((candidate) => candidate.id === id ? { ...candidate, expanded: !candidate.expanded } : candidate);
	});
}
async function selectResource(id: string, options: SelectResourceOptions = {}): Promise<void> {
	const selectionChanged = controllerState.selectedId !== id;
	if (options.clearUnread !== false) clearUnreadForResource(id);
	const forceDetail = selectionChanged || Boolean(options.forceDetail);
	if (forceDetail) {
		controllerState.navigationVersion++;
		schedulerMutationController.invalidate();
		controllerState.autoRefreshVersion++;
		controllerState.treeRequestVersion++;
		controllerState.detailRequestVersion++;
		controllerState.workspaceAgentsRequestVersion++;
		controllerState.diffRequestVersion++;
		if (id !== "workspace") {
			resourceDetailController.reset(id);
		}
	}
	if (selectionChanged) {
		flushAgentDraft();
		discardAgentUploadDialog();
		controllerState.diff = null;
		clearAgentDraftMemory();
		controllerState.messageStatus = null;
		controllerState.messageStatusKey = "";
		controllerState.messageStatusRequestVersion++;
		controllerState.steeringMessageId = "";
	}
	controllerState.selectedId = id;
	setMobileSidebar(false);
	ensureSelectedProjectExpanded(false);
	syncURL();
	saveUIState().catch((err) => console.warn("failed to save UI state", err));
	renderSelectionPanels();
	const detailPromise = id === "workspace"
		? loadWorkspaceAgents({ force: Boolean(options.forceDetail) })
		: loadDetail(id, { force: forceDetail });
	// Keep rejection handling attached immediately: the status request may
	// finish before the detail request, but it must not create an unhandled
	// rejection while navigation is already showing the detail.
	const statusPromise = refreshResourceMessageStatus(controllerState.activeWorkspaceId, id).then(
		() => ({ ok: true as const }),
		error => ({ ok: false as const, error })
	);
	await detailPromise;
	if (!isCurrentWorkspaceView(controllerState.activeWorkspaceId, controllerState.navigationVersion)) return;
	// Details are independent of the status/read side effects below. Render
	// them immediately so selection remains usable when the status endpoint is
	// delayed by resource reconciliation.
	renderSelectionPanels();
	const statusResult = await statusPromise;
	if (!statusResult.ok) throw statusResult.error;
	if (!isCurrentWorkspaceView(controllerState.activeWorkspaceId, controllerState.navigationVersion)) return;
	await markSelectedResourceRead();
	renderSelectionPanels();
}
async function toggleProject(id: string): Promise<void> {
	if (controllerState.expandedProjects.has(id)) controllerState.expandedProjects.delete(id);
	else controllerState.expandedProjects.add(id);
	renderAppShell();
	try {
		await saveUIState();
	} catch (err) {
		if (controllerState.expandedProjects.has(id)) controllerState.expandedProjects.delete(id);
		else controllerState.expandedProjects.add(id);
		renderAppShell();
		throw err;
	}
}
async function finishSchedulerMutation(operation: Promise<SchedulerMutationLease | null>, successMessage: string): Promise<boolean> {
	let lease: SchedulerMutationLease | null = null;
	try {
		lease = await operation;
		if (!lease?.isCurrent()) return false;
		await loadTree({ updateURL: false });
		if (!lease.isCurrent()) return false;
		await loadDetail("scheduler", { force: true });
		if (!lease.isCurrent()) return false;
		publishViewModels();
		if (!lease.isCurrent()) return false;
		toast(successMessage);
		return true;
	} catch (reason) {
		if (lease && !lease.isCurrent()) return false;
		toast(reason instanceof Error ? reason.message : String(reason));
		return false;
	} finally {
		lease?.release();
	}
}
function schedulerMutationCallbacks(): SchedulerMutationCallbacks {
	const operations = schedulerMutationController.operations();
	return {
		validateTarget: operations.validateTarget,
		save: (input) => finishSchedulerMutation(
			operations.save(input),
			input.scheduleId ? "Schedule update request sent." : "Schedule request sent."
		),
		setPaused: (scheduleId, paused) => finishSchedulerMutation(
			operations.setPaused(scheduleId, paused),
			paused ? "Schedule paused." : "Schedule resumed."
		),
		remove: (scheduleId) => finishSchedulerMutation(operations.remove(scheduleId), "Schedule removed."),
	};
}
function detailPanelModel(): DetailPanelModel {
	const workspaceId = controllerState.activeWorkspaceId || "";
	const base: DetailPanelModel = {
		identity: workspaceId ? `${workspaceId}:${controllerState.selectedId || "workspace"}` : "empty",
		workspaceId,
		workspaceName: workspaceName(),
		resourceId: controllerState.selectedId || "",
		resourceType: "",
		resourceTitle: "",
		parent: null,
		loading: false,
		detail: null,
		wiki: controllerState.tree?.wiki || null,
		workspaceAgents: controllerState.workspaceAgents,
		workspaceDefaults: {
			project: controllerState.tree?.resourceDefaults?.project || { kind: "profile", name: "default" },
			task: controllerState.tree?.resourceDefaults?.task || { kind: "profile", name: "default" }
		},
		workspaceUsers: controllerState.workspaceUsers,
		currentUserName: currentUserName(),
		generationPolicy: controllerState.tree?.generationPolicy || { enabled: true, maxTurns: 20, maxAccumulatedTurnMinutes: 120 },
		stallWatchdogPolicy: controllerState.tree?.stallWatchdogPolicy || { enabled: true, timeoutMinutes: 30 },
		agentBinding: controllerState.selectedId === "workspace"
			? controllerState.tree?.agentBinding || { kind: "profile", name: "default" }
			: findResource(controllerState.selectedId)?.agentBinding || { kind: "profile", name: "default" },
		agentProfiles: (controllerState.config?.agentProfiles || []).map((profile) => ({ key: profile.key, description: profile.description, agentName: profile.agentName })),
		agents: svelteAgentOptions(),
		resolveResourceTitle,
		onNavigate: (resourceId: string) => openBreadcrumbResource(resourceId).catch((err) => toast(errorMessage(err))),
		onCreateTask: (projectId: string) => showTaskForm(projectId),
		onArchive: (resourceId: string) => archiveResource(resourceId).catch((err) => toast(errorMessage(err))),
		onSaveWorkspaceAgents: (content: string, expectedContentHash: string) => saveWorkspaceAgentsFromDetail(content, expectedContentHash),
		onSaveMarkdownFile: (path: string, content: string, expectedContentHash: string) => saveMarkdownFileFromDetail(path, content, expectedContentHash),
		onDeleteArtifact: (path: string) => deleteArtifactFromDetail(path),
		onSaveAgentBinding: async (binding) => {
			const resourceId = controllerState.selectedId || "workspace";
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/agent-binding`, {
				method: "PUT", body: JSON.stringify(binding)
			});
			await loadTree({ updateURL: false });
			if (resourceId !== "workspace") await loadDetail(resourceId, { force: true });
			publishViewModels();
			toast("Resource agent binding saved.");
		},
		onRenameResource: async (title) => {
			const resourceId = controllerState.selectedId || "";
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/title`, {
				method: "PUT", body: JSON.stringify({ title })
			});
			await loadTree({ updateURL: false });
			await loadDetail(resourceId, { force: true });
			publishViewModels();
			toast("Resource name saved.");
		},
		onSaveDescription: async (description) => {
			const resourceId = controllerState.selectedId || "";
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/description`, {
				method: "PUT", body: JSON.stringify({ description })
			});
			await loadTree({ updateURL: false });
			await loadDetail(resourceId, { force: true });
			publishViewModels();
			toast("Resource description saved.");
		},
		onSaveWorkspaceDefaults: async (defaults) => {
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/defaults`, {
				method: "PUT", body: JSON.stringify(defaults)
			});
			await loadTree({ updateURL: false });
			publishViewModels();
			toast("Workspace default bindings saved.");
		},
		onSaveWorkspaceUserPreference: async (name, preference) => {
			const profile = await api<WorkspaceUser>(`/api/workspaces/${encodeURIComponent(workspaceId)}/users/${encodeURIComponent(name)}`, {
				method: "PUT", body: JSON.stringify({ preference })
			});
			controllerState.workspaceUsers = controllerState.workspaceUsers.map((user) => user.name === profile.name ? profile : user);
			publishViewModels();
			toast(`Preferences saved for ${name}.`);
		},
		onSwitchWorkspaceUser: async (name) => {
			if (name === currentUserName()) return;
			await saveUIState().catch((err) => console.warn("failed to save UI state before switching user", err));
			beginWorkspaceUserTransition(name);
			try {
				await loadUIState();
				controllerState.selectedId = controllerState.lastResourceId || "workspace";
				await loadTree({ updateURL: false });
			} finally {
				finishWorkspaceUserTransition();
			}
			toast(`Switched to ${name}.`);
		},
		onAddWorkspaceUser: async (name) => {
			await registerWorkspaceUser(workspaceId, validateUserName(name));
			publishViewModels();
			toast(`User ${name} added.`);
		},
		onDeleteWorkspaceUser: async (name) => {
			if (name === currentUserName()) throw new Error("Switch to another user before deleting the current user.");
			if (!(await confirmDialog({ title: "Delete user", message: `Delete ${name} and all of this user's Workspace UI state?`, confirmLabel: "Delete", danger: true }))) return;
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/users/${encodeURIComponent(name)}`, { method: "DELETE" });
			controllerState.workspaceUsers = controllerState.workspaceUsers.filter((user) => user.name !== name);
			publishViewModels();
			toast(`User ${name} deleted.`);
		},
		onSaveGenerationPolicy: async (policy) => {
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/generation-policy`, {
				method: "PUT", body: JSON.stringify(policy)
			});
			await loadTree({ updateURL: false });
			publishViewModels();
			toast("Generation policy saved.");
		},
		onSaveStallWatchdogPolicy: async (policy) => {
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/stall-watchdog-policy`, {
				method: "PUT", body: JSON.stringify(policy)
			});
			await loadTree({ updateURL: false });
			publishViewModels();
			toast("Turn stall watchdog saved.");
		},
		onSaveTaskDefault: async (projectId, binding) => {
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(projectId)}/task-default`, {
				method: "PUT", body: JSON.stringify(binding || {})
			});
			await loadTree({ updateURL: false });
			await loadDetail(projectId, { force: true });
			publishViewModels();
			toast(binding ? "Project Task default saved." : "Project Task default reset to inherit.");
		},
		schedulerActions: controllerState.selectedId === "scheduler" ? schedulerMutationCallbacks() : undefined,
		onToast: toast
	};
	if (!controllerState.tree) return base;
	if (controllerState.selectedId === "workspace") return {
		...base,
		resourceId: "workspace",
		resourceType: "workspace",
		resourceTitle: workspaceName()
		};
	const selected = findResource(controllerState.selectedId) || controllerState.tree.scheduler || controllerState.tree.projects[0];
	if (!selected) return {
		...base,
		resourceId: "workspace",
		resourceType: "workspace",
		resourceTitle: workspaceName()
	} as DetailPanelModel;
	const detail = controllerState.details[selected.id] || null;
	const parent = parentProject(selected.id);
	return {
		...base,
		identity: `${workspaceId}:${selected.id}:${selected.type}`,
		resourceId: selected.id,
		resourceType: selected.type === "scheduler" || selected.type === "project" || selected.type === "task" ? selected.type : "",
		resourceTitle: detail?.title || selected.title || selected.id,
		parent: parent && parent.id !== selected.id ? {
			id: parent.id,
			title: parent.title || parent.id
		} : null,
		loading: !detail,
		detail: resourceDetailView(detail)
	};
}
function resourceDetailView(detail: ResourceRecord | null): DetailPanelModel["detail"] {
	if (!detail || (detail.type !== "scheduler" && detail.type !== "project" && detail.type !== "task")) return null;
	return {
		...detail,
		type: detail.type,
		title: detail.title || detail.id,
		path: detail.path || ""
	};
}
function renderDetails(): void {
	publisher.renderDetailPanel(detailPanelModel());
}
async function openBreadcrumbResource(id: string): Promise<void> {
	await selectResource(id, { forceDetail: id === controllerState.selectedId && id !== "workspace" });
}
async function saveWorkspaceAgentsFromDetail(content: string, expectedContentHash: string): Promise<WorkspaceFileRecord> {
	if (!controllerState.activeWorkspaceId) throw new Error("No workspace is selected.");
	const workspaceId = controllerState.activeWorkspaceId;
	const navigationVersion = controllerState.navigationVersion;
	const saved = await api<WorkspaceFileRecord>(`/api/workspaces/${workspaceId}/files?path=${encodeURIComponent("AGENTS.md")}`, {
		method: "PUT",
		body: JSON.stringify({
			content,
			expectedContentHash
		})
	});
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion) || controllerState.selectedId !== "workspace") throw new Error("The workspace changed before AGENTS.md finished saving.");
	controllerState.workspaceAgents = saved;
	publishViewModels();
	return saved;
}
async function saveMarkdownFileFromDetail(path: string, content: string, expectedContentHash: string): Promise<WorkspaceFileRecord> {
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = controllerState.selectedId;
	if (!workspaceId || !resourceId || resourceId === "workspace" || resourceId === "scheduler") throw new Error("No editable resource is selected.");
	const navigationVersion = controllerState.navigationVersion;
	if (path.includes("/templates/")) {
		const name = path.split("/").pop()?.replace(/\.(md|markdown|mdown|mkdn)$/i, "") || "template";
		const validation = await api<TaskTemplate>(`/api/workspaces/${encodeURIComponent(workspaceId)}/templates/validate`, {
			method: "POST",
			body: JSON.stringify({ name, content })
		});
		if (!validation.valid) throw new Error(validation.errors?.[0]?.message || "The task template is invalid.");
	}
	const saved = await api<WorkspaceFileRecord>(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/documents?path=${encodeURIComponent(path)}`, {
		method: "PUT",
		body: JSON.stringify({ content, expectedContentHash })
	});
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion) || controllerState.selectedId !== resourceId) throw new Error("The resource changed before the Markdown file finished saving.");
	await loadDetail(resourceId, { force: true });
	publishViewModels();
	return saved;
}
async function deleteArtifactFromDetail(path: string): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = controllerState.selectedId;
	if (!workspaceId || !resourceId || resourceId === "workspace" || resourceId === "scheduler") throw new Error("No editable resource is selected.");
	const navigationVersion = controllerState.navigationVersion;
	await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/artifacts?path=${encodeURIComponent(path)}`, {
		method: "DELETE"
	});
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion) || controllerState.selectedId !== resourceId) throw new Error("The resource changed before the artifact finished deleting.");
	await loadDetail(resourceId, { force: true });
	publishViewModels();
	toast("Artifact deleted.");
}
function closeDiff(): void {
	controllerState.diffRequestVersion++;
	controllerState.diff = null;
	publishViewModels();
}
async function fetchCurrentTree(workspaceId = controllerState.activeWorkspaceId): Promise<WorkspaceTree | null> {
	const requestVersion = ++controllerState.treeRequestVersion;
	const navigationVersion = controllerState.navigationVersion;
	let tree: WorkspaceTree;
	try {
		tree = await api<WorkspaceTree>(`/api/workspaces/${workspaceId}/tree`);
	} catch (error) {
		if (error instanceof ApiError && (error.code === "user_required" || error.code === "user_not_found")) {
			await loadWorkspaceContext();
			return null;
		}
		throw error;
	}
	return isCurrentWorkspaceView(workspaceId, navigationVersion, requestVersion) ? tree : null;
}
// refreshInbox loads the current user's durable agent-to-user inbox. The
// inbox is Workspace-scoped but user-specific; fetch failures are silent so a
// missing user profile never breaks the surrounding refresh cycle.
async function refreshInbox(workspaceId = controllerState.activeWorkspaceId): Promise<boolean> {
	if (!workspaceId) return false;
	try {
		const response = await api<{ messages?: InboxMessageRecord[] }>(`/api/workspaces/${encodeURIComponent(workspaceId)}/users/${encodeURIComponent(currentUserName())}/messages`);
		if (workspaceId !== controllerState.activeWorkspaceId) return false;
		const messages = Array.isArray(response.messages) ? response.messages : [];
		if (sameJSON(controllerState.inbox, messages)) return false;
		controllerState.inbox = messages;
		return true;
	} catch (err) {
		console.warn("inbox refresh failed", err);
		return false;
	}
}
async function openInboxMessage(messageId: string): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	const message = controllerState.inbox.find((item) => item.messageId === messageId);
	if (!workspaceId || !message) return;
	if (message.unread || !message.readAt) {
		try {
			await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/users/${encodeURIComponent(currentUserName())}/messages/${encodeURIComponent(messageId)}/read`, { method: "PUT" });
		} catch (err) {
			console.warn("failed to mark inbox message read", err);
		}
	}
	if (message.sourceResourceId) await selectResource(message.sourceResourceId);
	await refreshInbox(workspaceId);
	publishViewModels();
}
async function replyInboxMessage(messageId: string, text: string): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	const message = controllerState.inbox.find((item) => item.messageId === messageId);
	if (!workspaceId || !message || !text.trim()) return;
	await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/users/${encodeURIComponent(currentUserName())}/messages/${encodeURIComponent(messageId)}/reply`, {
		method: "POST", body: JSON.stringify({ text: text.trim() })
	});
	await refreshInbox(workspaceId);
	if (message.sourceResourceId && message.sourceResourceId === selectedAgentResourceId()) {
		await refreshResourceMessageStatus(workspaceId, message.sourceResourceId);
	}
	publishViewModels();
	toast("Reply sent.");
}
async function deleteInboxMessage(messageId: string): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	if (!workspaceId) return;
	const confirmed = await confirmDialog({ title: "Delete message", message: "Delete this inbox message? This cannot be undone.", confirmLabel: "Delete", danger: true });
	if (!confirmed) return;
	await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/users/${encodeURIComponent(currentUserName())}/messages/${encodeURIComponent(messageId)}`, { method: "DELETE" });
	await refreshInbox(workspaceId);
	publishViewModels();
}
async function refreshTreeAfterResourceMutation(): Promise<void> {
	if (!controllerState.activeWorkspaceId || !controllerState.tree) return;
	const tree = await fetchCurrentTree(controllerState.activeWorkspaceId);
	if (tree) controllerState.tree = tree;
}
async function markSelectedResourceRead(): Promise<boolean> {
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = selectedAgentResourceId();
	if (!workspaceId || !resourceId || !controllerState.tree) return false;
	const item = findTreeResource(resourceId);
	if (!item) return false;
	const turnNumber = Number(item.latestTurnNumber) || 0;
	const readTurnNumber = Number(item.userState?.readTurnNumber) || 0;
	if (turnNumber <= 0 || turnNumber <= readTurnNumber) return false;
	const navigationVersion = controllerState.navigationVersion;
	const userState = await api<{ readTurnNumber?: number }>(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/read`, {
		method: "PUT",
		body: JSON.stringify({ throughTurnNumber: turnNumber })
	});
	if (!isCurrentWorkspaceView(workspaceId, navigationVersion) || selectedAgentResourceId() !== resourceId) return false;
	updateResourceReadState(resourceId, userState);
	return true;
}

function updateResourceReadState(resourceId: string, userState: { readTurnNumber?: number }): void {
	const tree = controllerState.tree;
	if (!tree) return;
	const update = (item: ResourceRecord | null | undefined): void => {
		if (!item || item.id !== resourceId) return;
		item.userState = userState;
		item.unreadCount = Math.max(0, (Number(item.latestTurnNumber) || 0) - (Number(userState.readTurnNumber) || 0));
	};
	update(tree.workspace);
	update(tree.scheduler);
	for (const project of tree.projects || []) {
		update(project);
		for (const task of project.children || []) update(task);
	}
	if (!tree.activity) return;
	for (const items of Object.values(tree.activity)) {
		for (const item of items) update(item);
	}
	tree.activity.unread = tree.activity.unread.filter((item) => item.id !== resourceId);
}
async function refreshResourceMessageStatus(workspaceId = controllerState.activeWorkspaceId, resourceId = selectedAgentResourceId()): Promise<boolean> {
	if (!workspaceId || !resourceId) return false;
	const requestVersion = ++controllerState.messageStatusRequestVersion;
	const key = `${workspaceId}:${resourceId}`;
	const status = await api<ResourceMessageStatus>(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/status`);
	if (requestVersion !== controllerState.messageStatusRequestVersion || workspaceId !== controllerState.activeWorkspaceId || resourceId !== selectedAgentResourceId()) return false;
	const changed = controllerState.messageStatusKey !== key || !sameJSON(controllerState.messageStatus, status);
	controllerState.messageStatusKey = key;
	controllerState.messageStatus = status;
	return changed;
}

function dismissStopNotice(): void {
	controllerState.stopNotice = null;
	renderChatComposer();
}

async function steerWaitingMessage(messageId: string): Promise<void> {
	if (!messageId || controllerState.steeringMessageId) return;
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = selectedAgentResourceId();
	controllerState.steeringMessageId = messageId;
	renderChatComposer();
	try {
		await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/messages/${encodeURIComponent(messageId)}/steer`, { method: "POST" });
		await refreshResourceMessageStatus(workspaceId, resourceId);
		if (workspaceId === controllerState.activeWorkspaceId && resourceId === selectedAgentResourceId()) {
			publishViewModels();
			toast("Message inserted into the current turn.");
		}
	} catch (error) {
		try { await refreshResourceMessageStatus(workspaceId, resourceId); } catch (_) {}
		throw error;
	} finally {
		if (controllerState.steeringMessageId === messageId) {
			controllerState.steeringMessageId = "";
			renderChatComposer();
		}
	}
}
async function reloadResourceForSelection(): Promise<void> {
	flushAgentDraft();
	agentOperations.reset();
	clearAgentDraftMemory();
	controllerState.messageStatus = null;
	controllerState.messageStatusKey = "";
	controllerState.messageStatusRequestVersion++;
	controllerState.stopNotice = null;
	await refreshResourceMessageStatus();
}
function resetAgentState(): void {
	flushAgentDraft();
	discardAgentUploadDialog();
	controllerState.agent.optionsOpen = false;
	controllerState.agent.historyOpen = false;
	clearAgentDraftMemory();
	agentOperations.reset();
	controllerState.messageStatus = null;
	controllerState.messageStatusKey = "";
	controllerState.messageStatusRequestVersion++;
	controllerState.steeringMessageId = "";
	controllerState.stopNotice = null;
	controllerState.agent.toolGroupOpen.clear();
	controllerState.agent.approvalDrafts.clear();
	controllerState.agent.renderDeferredForSelection = false;
	clearAgentRenderTimer();
}
function handleSvelteAgentEvent(workspaceId: string, resourceId: string, event: AgentEvent): void {
	if (workspaceId !== controllerState.activeWorkspaceId || resourceId !== selectedAgentResourceId() || !event) return;
	const runtime = findResource(resourceId)?.runtime || controllerState.messageStatus?.generation;
	if ([
		"turn.completed",
		"turn.failed",
		"turn.cancelled"
	].includes(event.type)) observeCompletionEvent(event, runtime?.generationId ? {
		id: runtime.generationId,
		resourceId,
		generationId: runtime.generationId,
		completionState: event.type === "turn.failed" ? "failed" : event.type === "turn.cancelled" ? "cancelled" : "completed"
	} : null);
	if ([
		"turn.completed",
		"turn.failed",
		"turn.cancelled",
		"session.state",
		"approval.requested",
		"approval.resolved"
	].includes(event.type)) refreshResourceMessageStatus().then(publishViewModels).catch((err) => console.warn("agent refresh failed", err));
}
function clearAgentRenderTimer(): void {
	if (controllerState.agent.renderTimer) window.clearTimeout(controllerState.agent.renderTimer);
	controllerState.agent.renderTimer = null;
}
function agentConfigSummary(agent: AgentConfig | null | undefined): string {
	if (!agent) return "";
	const parts = [providerName(agent.providerId)];
	if (agent.options?.model) parts.push(agent.options.model);
	return parts.filter(Boolean).join(" · ");
}
function providerName(providerId: string | undefined): string {
	return (controllerState.config?.agentHubProviders || settingsController.providers()).find((item) => item.id === providerId)?.name || providerId || "Provider";
}
function chatTimelineHasActiveSelection(log: HTMLElement): boolean {
	const selection = window.getSelection?.();
	if (!selection || selection.isCollapsed || selection.rangeCount === 0) return false;
	return selection.getRangeAt(0).intersectsNode(log);
}
function renderChatPanel(_options: RenderOptions = {}): void {
	renderChatComposer();
	const resourceId = selectedAgentResourceId();
	const status = controllerState.messageStatusKey === `${controllerState.activeWorkspaceId}:${resourceId}` ? controllerState.messageStatus : null;
	const configured = (controllerState.config?.agents || []).find((agent) => agent.id === status?.resolvedAgent);
	const agent = configured || selectedAgentConfig();
	const runtime = findResource(resourceId)?.runtime;
	const submitting = agentOperations.isSending(resourceMutationKey(controllerState.activeWorkspaceId, resourceId));
	publisher.renderAgentPanelHeader({
		identity: `${controllerState.activeWorkspaceId}:${resourceId}`,
		workspaceId: controllerState.activeWorkspaceId,
		resourceId,
		status,
		submitting,
		agentName: agentDisplayName(agent),
		modelSummary: agentConfigSummary(agent),
		turnNumber: Number(status?.generation?.turnNumber) || Number(runtime?.turnNumber) || 0,
		turnStartedAt: String(runtime?.turnStartedAt || "")
	});
	publisher.renderEventTimeline({
		identity: `${controllerState.activeWorkspaceId}:${resourceId}`,
		workspaceId: controllerState.activeWorkspaceId,
		resourceId,
		status,
		submitting,
		agentName: agentDisplayName(agent),
		resolveResourceTitle,
		onNavigate: (targetResourceId: string) => selectResource(targetResourceId).catch((err) => toast(errorMessage(err))),
		project: projectConversationEvents,
		onEvent: handleSvelteAgentEvent,
		onNotice: () => {},
		onApproval: resolveResourceApproval,
		onToast: toast
	});
}
function resourceMutationKey(workspaceId: string, resourceId: string): string {
	return `${workspaceId || "workspace"}:${resourceId || "resource"}`;
}
let agentBindingSavingFor = "";
function renderChatComposer(_options: RenderOptions = {}): void {
	controllerState.agent.skipChatDraftSync = false;
	const resourceId = selectedAgentResourceId();
	if (controllerState.activeWorkspaceId && resourceId) restoreAgentDraftForResource(resourceId);
	const stopTurnPending = agentOperations.active("turn-stop") && agentOperations.key("turn-stop") === resourceId;
	const endGenerationPending = agentOperations.active("generation-end") && agentOperations.key("generation-end") === resourceId;
	const messageStatus = controllerState.messageStatusKey === `${controllerState.activeWorkspaceId}:${resourceId}` ? controllerState.messageStatus : null;
	const workspaceId = controllerState.activeWorkspaceId;
	const stopNoticeKey = `${workspaceId}:${resourceId}`;
	const canEndTurn = Boolean(stopTurnPending || ["running", "waiting_approval"].includes(String(messageStatus?.session?.state || "")));
	publisher.renderComposer({
		identity: `${controllerState.activeWorkspaceId}:${resourceId}:${controllerState.agent.chatDraftKey || ""}`,
		workspaceId: controllerState.activeWorkspaceId,
		resourceId,
		draft: controllerState.agent.chatDraft || "",
		draftKey: controllerState.agent.chatDraftKey || "",
		draftResetVersion: controllerState.agent.chatDraftResetVersion || 0,
		unavailableReason: !messageStatus ? "Loading work status." : !messageStatus.acceptsMessages ? (messageStatus.archived ? "This resource is archived." : messageStatus.configError || "This resource cannot accept messages.") : "",
		sending: agentOperations.isSending(resourceMutationKey(controllerState.activeWorkspaceId, resourceId)),
		canEndTurn,
		endingTurn: stopTurnPending,
		canEndGeneration: Boolean(messageStatus?.acceptsMessages && messageStatus?.generation?.generationId && !canEndTurn),
		endingGeneration: Boolean(endGenerationPending || messageStatus?.generation?.replacementPending),
		stopNotice: controllerState.stopNotice?.key === stopNoticeKey ? controllerState.stopNotice.text : "",
		waitingMessages: messageStatus?.waitingMessages || [],
		canSteerWaiting: Boolean(messageStatus?.canSteerWaiting),
		steeringMessageId: controllerState.steeringMessageId,
		agentBinding: resourceId === "workspace"
			? controllerState.tree?.agentBinding || { kind: "profile", name: "default" }
			: findResource(resourceId)?.agentBinding || { kind: "profile", name: "default" },
		agentProfiles: (controllerState.config?.agentProfiles || []).map((profile) => ({ key: profile.key, description: profile.description, agentName: profile.agentName })),
		agents: svelteAgentOptions(),
		bindingSaving: agentBindingSavingFor === resourceId,
		onDraft: (text, draftContext) => updateAgentDraftFromSvelte(text, draftContext),
		onSend: submitChatInput,
		onOpenUpload: openAgentUploadDialog,
		onEndTurn: () => stopAgentTurn().catch((err) => toast(err.message)),
		onEndGeneration: () => endAgentGeneration().catch((err) => toast(err.message)),
		onDismissStopNotice: dismissStopNotice,
		onSteerWaiting: steerWaitingMessage,
		onSaveAgentBinding: async (binding) => {
			if (resourceId !== selectedAgentResourceId()) return;
			agentBindingSavingFor = resourceId;
			renderChatComposer();
			try {
				await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/agent-binding`, {
					method: "PUT", body: JSON.stringify(binding)
				});
				await loadTree({ updateURL: false });
				if (resourceId !== "workspace") await loadDetail(resourceId, { force: true });
				publishViewModels();
				toast("Resource agent binding saved.");
			} catch (err) {
				toast(errorMessage(err));
			} finally {
				agentBindingSavingFor = "";
				renderChatComposer();
			}
		}
	});
}
function agentDisplayName(agent: AgentConfig | null | undefined): string {
	return agent?.name || agent?.id || "Agent";
}
function renderSettingsModal(): void {
	settingsController.render();
}
function updateAgentDraftFromSvelte(text: string, context: ComposerContext): void {
	if (!context || context.workspaceId !== controllerState.activeWorkspaceId || context.resourceId !== selectedAgentResourceId() || context.draftKey !== controllerState.agent.chatDraftKey) return;
	updateAgentDraft(text);
}
function openAgentUploadDialog(): void {
	const resourceId = selectedAgentResourceId();
	if (!resourceId || controllerState.messageStatus?.archived) {
		toast("Select an active resource before uploading files.");
		return;
	}
	const input = elementById<HTMLInputElement>("chatInput");
	if (input) updateAgentDraft(input.value);
	controllerState.modalEnter = "upload";
	controllerState.uploadDialog = {
		open: true,
		identity: ++uploadDialogIdentity,
		resourceId,
		items: [],
		nextId: 1
	};
	renderAgentUploadDialog();
}
function closeAgentUploadDialog(paths: string[] = [], context: UploadContext = {}): void {
	if (!controllerState.uploadDialog.open) return;
	const sameResource = controllerState.uploadDialog.resourceId === selectedAgentResourceId();
	const sameWorkspace = !context.workspaceId || context.workspaceId === controllerState.activeWorkspaceId;
	const shouldSkipDraftSync = paths.length > 0 && sameWorkspace && sameResource;
	if (shouldSkipDraftSync) {
		updateAgentDraft(appendUploadedPaths(controllerState.agent.chatDraft, paths));
		controllerState.agent.chatDraftResetVersion++;
	}
	discardAgentUploadDialog();
	const composer = elementById("chatComposer");
	if (composer) delete composer.dataset.composerKey;
	renderChatComposer({ skipDraftSync: shouldSkipDraftSync });
	elementById("chatInput")?.focus({ preventScroll: true });
}
function discardAgentUploadDialog(): void {
	controllerState.uploadDialog = {
		open: false,
		identity: ++uploadDialogIdentity,
		resourceId: "",
		items: [],
		nextId: 1
	};
	renderAgentUploadDialog();
}
function appendUploadedPaths(draft: string, paths: string[]): string {
	const block = paths.filter(Boolean).join("\n");
	if (!block) return draft;
	if (!draft) return block;
	return `${draft}${draft.endsWith("\n") ? "" : "\n"}${block}`;
}
function renderAgentUploadDialog(): void {
	const dialog = controllerState.uploadDialog;
	publisher.renderUploadDialog({
		open: Boolean(dialog.open),
		identity: `${dialog.identity || 0}:${controllerState.activeWorkspaceId}:${dialog.resourceId || ""}`,
		workspaceId: controllerState.activeWorkspaceId,
		resourceId: dialog.resourceId || "",
		onDone: closeAgentUploadDialog
	});
}
async function stopAgentTurn(): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = selectedAgentResourceId();
	const generationId = controllerState.messageStatus?.generation?.generationId || "";
	const operation = agentOperations.begin("turn-stop", resourceId);
	if (!operation) return;
	try {
		const query = generationId ? `?generationId=${encodeURIComponent(generationId)}` : "";
		const response = await api<{
			status?: string;
			taskState?: string;
			taskStateError?: string;
			pendingSteerPolicy?: string;
			cancelledPendingSteerCount?: number;
			pendingSteerCancellationError?: string;
		}>(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/turn/end${query}`, { method: "POST" });
		const cancelledCount = Math.max(0, Number(response.cancelledPendingSteerCount || 0));
		let notice = response.taskState === "paused" ? "Task paused. " : "";
		notice += cancelledCount === 1
			? "Turn stopped. 1 pending steer was cancelled and will not affect the next turn."
			: cancelledCount > 1
				? `Turn stopped. ${cancelledCount} pending steers were cancelled and will not affect the next turn.`
				: "Turn stopped. No pending steer remained; any steer already delivered to this turn was not changed.";
		if (response.pendingSteerCancellationError) notice += ` Pending steer cancellation needs attention: ${response.pendingSteerCancellationError}`;
		if (response.taskStateError) notice += ` Task pause needs attention: ${response.taskStateError}`;
		controllerState.stopNotice = { key: `${workspaceId}:${resourceId}`, text: notice };
		await Promise.all([refreshResourceMessageStatus(workspaceId, resourceId), refreshTreeAfterResourceMutation()]);
		publishViewModels();
	} finally {
		agentOperations.finish(operation);
	}
}

async function endAgentGeneration(): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = selectedAgentResourceId();
	const generationId = controllerState.messageStatus?.generation?.generationId || "";
	if (!workspaceId || !resourceId || !generationId) return;
	if (!window.confirm("End this generation? Its AgentHub session will be stopped and archived. Your next message will start a new generation.")) return;
	const operation = agentOperations.begin("generation-end", resourceId);
	if (!operation) return;
	try {
		await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/generation/end?generationId=${encodeURIComponent(generationId)}`, { method: "POST" });
		await Promise.all([refreshResourceMessageStatus(workspaceId, resourceId), refreshTreeAfterResourceMutation()]);
		publishViewModels();
		toast("Generation is ending. Your next message will start a new generation.");
	} finally {
		agentOperations.finish(operation);
	}
}
async function resolveResourceApproval(generationId: string, requestId: string, reply: Parameters<EventTimelineModel["onApproval"]>[2]): Promise<void> {
	const workspaceId = controllerState.activeWorkspaceId;
	const resourceId = selectedAgentResourceId();
	await api(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/approval?generationId=${encodeURIComponent(generationId)}`, {
		method: "POST", body: JSON.stringify({ requestId, ...reply })
	});
	await refreshResourceMessageStatus(workspaceId, resourceId);
	publishViewModels();
}
async function submitChatInput(rawText: string, context: ComposerContext): Promise<{ accepted: boolean; clear: boolean }> {
	if (!rawText.trim() || context.workspaceId !== controllerState.activeWorkspaceId || context.resourceId !== selectedAgentResourceId() || context.draftKey !== controllerState.agent.chatDraftKey) return { accepted: false, clear: false };
	const key = resourceMutationKey(context.workspaceId, context.resourceId);
	if (!agentOperations.startSending(key)) return { accepted: false, clear: false };
	const version = controllerState.agent.chatDraftVersion;
	try {
		await api(`/api/workspaces/${encodeURIComponent(context.workspaceId)}/resources/${encodeURIComponent(context.resourceId)}/messages`, {
			method: "POST", body: JSON.stringify({ text: rawText, role: "user", sender: { name: currentUserName() } })
		});
		const accepted = true;
		const clear = clearResourceDraftAfterAccepted({ workspaceId: context.workspaceId, resourceId: context.resourceId, key: context.draftKey, text: rawText, version });
		if (clear) controllerState.agent.chatDraftResetVersion++;
		if (clear && controllerState.stopNotice?.key === `${context.workspaceId}:${context.resourceId}`) controllerState.stopNotice = null;
		// The message is durably accepted once the POST returns. Resolve the
		// send immediately so the composer clears its submitting state, and
		// refresh status/tree in the background: those requests share the
		// resource's server-side job queue and can lag behind long-running
		// reconciliation work (for example first-turn setup on large sessions).
		void Promise.all([refreshResourceMessageStatus(context.workspaceId, context.resourceId), refreshTreeAfterResourceMutation()])
			.then(publishViewModels)
			.catch((err) => console.warn("post-send refresh failed", err));
		publishViewModels();
		return { accepted, clear };
	} finally {
		agentOperations.stopSending(key);
	}
}
function selectedAgentResourceId(): string {
	if (controllerState.selectedId === "workspace") return "workspace";
	return findResource(controllerState.selectedId)?.id || "";
}
function relativeTime(value: string | undefined): string {
	if (!value) return "unknown";
	const time = new Date(value).getTime();
	if (!Number.isFinite(time)) return value;
	const seconds = Math.max(0, Math.round((Date.now() - time) / 1e3));
	if (seconds < 60) return `${seconds}s ago`;
	const minutes = Math.round(seconds / 60);
	if (minutes < 60) return `${minutes}m ago`;
	const hours = Math.round(minutes / 60);
	if (hours < 24) return `${hours}h ago`;
	return `${Math.round(hours / 24)}d ago`;
}
function showProjectForm(): void {
	openCreateDialog("project");
}
function showTaskForm(projectId: string): void {
	openCreateDialog("task", projectId);
}
function openCreateDialog(type: "project" | "task", projectId = ""): void {
	createDialogController.open(type === "task" ? "task" : "project", projectId);
}
function closeCreateDialog(): void {
	createDialogController.close();
}
function renderCreateDialog(): void {
	createDialogController.render();
}
async function archiveResource(resourceId: string): Promise<void> {
	const kind = findResource(resourceId)?.type === "task" ? "task" : "project";
	const title = resolveResourceTitle(resourceId) || resourceId;
	const confirmed = await confirmDialog({
		title: `Archive ${kind}`,
		message: `Archive ${kind} "${title}"? This ends its open working state and stops its agent.`,
		confirmLabel: "Archive",
		danger: true
	});
	if (!confirmed) return;
	const redirectTarget = archiveRedirectTarget(resourceId, controllerState.projectOrder, controllerState.taskOrder);
	const result = await api<ArchiveResponse>(`/api/workspaces/${controllerState.activeWorkspaceId}/archive`, {
		method: "POST",
		body: JSON.stringify({ resourceId })
	});
	const warnings = result.warnings || [];
	toast(warnings.length > 0
		? [`Archived.`, ...warnings.map((warning) => `Warning: ${warning.message}`)].join("\n")
		: "Archived.");
	removeArchivedResourceFromTree(resourceId);
	await selectResource(redirectTarget);
}
// removeArchivedResourceFromTree drops a just-archived resource (and, for
// projects, its tasks) from the local tree so the sidebar only updates the
// affected nodes instead of reloading the whole tree.
function removeArchivedResourceFromTree(resourceId: string): void {
	const tree = controllerState.tree;
	const removedIds = new Set<string>([resourceId]);
	if (tree) {
		const projects = tree.projects || [];
		const projectIndex = projects.findIndex((project) => project.id === resourceId);
		if (projectIndex >= 0) {
			const [removed] = projects.splice(projectIndex, 1);
			for (const task of removed.children || []) removedIds.add(task.id);
		} else {
			for (const project of projects) {
				const taskIndex = (project.children || []).findIndex((task) => task.id === resourceId);
				if (taskIndex < 0) continue;
				project.children!.splice(taskIndex, 1);
				break;
			}
		}
		if (tree.activity) {
			for (const key of Object.keys(tree.activity) as Array<keyof typeof tree.activity>) {
				tree.activity[key] = tree.activity[key].filter((item) => !removedIds.has(item.id));
			}
		}
	}
	controllerState.projectOrder = controllerState.projectOrder.filter((id) => !removedIds.has(id));
	for (const id of removedIds) controllerState.expandedProjects.delete(id);
	for (const [projectId, order] of Object.entries(controllerState.taskOrder || {})) {
		if (removedIds.has(projectId)) delete controllerState.taskOrder[projectId];
		else if (order.some((id) => removedIds.has(id))) controllerState.taskOrder[projectId] = order.filter((id) => !removedIds.has(id));
	}
	const removedFolders = controllerState.folders.filter((folder) => removedIds.has(folder.projectId));
	if (removedFolders.length) {
		const removedFolderIds = new Set(removedFolders.map((folder) => folder.id));
		controllerState.folders = controllerState.folders.filter((folder) => !removedFolderIds.has(folder.id));
		for (const folderId of removedFolderIds) delete controllerState.folderOrder[folderId];
	}
	for (const [folderId, order] of Object.entries(controllerState.folderOrder || {})) {
		if (order.some((id) => removedIds.has(id))) controllerState.folderOrder[folderId] = order.filter((id) => !removedIds.has(id));
	}
	for (const id of removedIds) clearUnreadForResource(id);
}
function findResource(id: string): ResourceRecord | null {
	if (!controllerState.tree) return null;
	if (controllerState.tree.scheduler?.id === id) return controllerState.tree.scheduler;
	for (const project of controllerState.tree.projects) {
		if (project.id === id) return project;
		for (const task of project.children || []) if (task.id === id) return task;
	}
	return null;
}
function findTreeResource(id: string): ResourceRecord | null {
	if (!controllerState.tree) return null;
	if (id === "workspace") return controllerState.tree.workspace || null;
	return findResource(id);
}
function resolveResourceTitle(id: string): string | null {
	if (id === "workspace") return workspaceName();
	const resource = findResource(id);
	return resource ? String(resource.title || resource.id).trim() || resource.id : null;
}
function ensureValidSelection(): boolean {
	if (controllerState.selectedId === "workspace" || findResource(controllerState.selectedId)) return false;
	controllerState.selectedId = "workspace";
	return true;
}
function parentProject(id: string): ResourceRecord | null {
	if (!controllerState.tree) return null;
	for (const project of controllerState.tree.projects) {
		if (project.id === id) return project;
		if ((project.children || []).some((task) => task.id === id)) return project;
	}
	return null;
}
function isProjectExpanded(id: string): boolean {
	return controllerState.expandedProjects.has(id);
}
function ensureSelectedProjectExpanded(persist = false): void {
	const parent = parentProject(controllerState.selectedId);
	if (!parent || parent.id === controllerState.selectedId || controllerState.expandedProjects.has(parent.id)) return;
	controllerState.expandedProjects.add(parent.id);
	if (persist) saveUIState().catch((err) => toast(err.message));
}
function parseRoute(pathname = window.location.pathname): ReturnType<typeof routeController.parse> {
	return routeController.parse(pathname);
}
function workspaceExists(id: string | undefined): boolean {
	return Boolean(id && (controllerState.config?.workspaces || []).some((workspace) => workspace.id === id));
}
function syncURL(options: { replace?: boolean } = {}): void {
	routeController.project(controllerState.activeWorkspaceId, controllerState.selectedId, options);
}
function workspaceName(): string {
	return (controllerState.config?.workspaces || []).find((w) => w.id === controllerState.activeWorkspaceId)?.name || "Workspace";
}
function applyAgentConfig(): void {
	const agents = enabledAgentConfigs();
	const defaultAgentName = defaultChatAgentName();
	if (!agents.some((agent) => agent.id === controllerState.agent.agentName)) controllerState.agent.agentName = defaultAgentName;
}
function selectedAgentConfig(): AgentConfig | null {
	const agents = enabledAgentConfigs();
	const agentName = controllerState.agent.agentName || defaultChatAgentName();
	return agents.find((agent) => agent.id === agentName) || agents[0] || null;
}
function enabledAgentConfigs(): AgentConfig[] {
	return (controllerState.config?.agents || []).filter((agent) => agent.available !== false);
}
function defaultChatAgentName(): string {
	const agents = enabledAgentConfigs();
	const configured = configuredAgentProfileName(controllerState.config?.agentProfiles, "default") || configuredAgentProfileName(settingsController.profiles(), "default");
	if (configured) return configured;
	return agents[0]?.id || "";
}
function configuredAgentProfileName(profiles: AgentProfile[] | undefined, key: string): string {
	const normalizedKey = String(key || "").trim().toLowerCase();
	const profile = (profiles || []).find((item) => String(item.key || "").trim().toLowerCase() === normalizedKey);
	return String(profile?.agentName || "").trim();
}
async function openSettings(tab: SettingsModel["initialTab"] = "system"): Promise<void> {
	return settingsController.open(tab);
}
function closeSettings(dirty = false): void {
	void settingsController.close(dirty);
}
async function refreshSettings(): Promise<void> {
	await settingsController.refresh();
}
function configWithAgentHubCatalog(base: WorkspaceConfig, agentHub: AgentHubData): WorkspaceConfig {
	return settingsController.withAgentHubCatalog(base, agentHub);
}
function sameJSON(a: unknown, b: unknown): boolean {
	return JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
}
let toastRevision = 0;
function toast(message: unknown): void {
	publisher.renderToast({ message: String(message || ""), revision: ++toastRevision });
}
function optionalAssetLoaded(asset: string): void {
	if (asset === "markdown" && window.marked && window.DOMPurify) {
		renderDetails();
	}
	if (asset === "diff") renderDetails();
}
window.puaAssetLoaded = optionalAssetLoaded;
function initPaneResize(): void {
	paneLayoutController.initialize();
	themeController.initialize();
}
function setPaneSize(name: keyof AppShellModel["paneSizes"], value: number): void {
	paneLayoutController.previewPane(name, value);
}
function savePaneSize(name: keyof AppShellModel["paneSizes"]): void {
	paneLayoutController.commitPane(name);
}
function syncPaneViewport(): void {
	paneLayoutController.syncViewport();
}
function setMobileSidebar(open: boolean): void {
	paneLayoutController.setMobileSidebar(open);
}
function installControllerListeners(): void {
	lifecycle?.listen(document, "selectionchange", () => {
	if (!controllerState.agent.renderDeferredForSelection) return;
	const log = elementById("chatTimeline");
	if (log && chatTimelineHasActiveSelection(log)) return;
	controllerState.agent.renderDeferredForSelection = false;
	renderChatPanel();
	});
	lifecycle?.listen(document, "keydown", (event) => {
	if (event.key === "Escape" && controllerState.diff) closeDiff();
	else if (event.key === "Escape" && (controllerState.agent.optionsOpen || controllerState.agent.historyOpen)) {
		controllerState.agent.optionsOpen = false;
		controllerState.agent.historyOpen = false;
		renderChatComposer();
	}
	});
	lifecycle?.listen(document, "click", (event) => {
	const target = event.target instanceof Element ? event.target : null;
	const breadcrumbButton = target?.closest<HTMLElement>("[data-breadcrumb-resource]");
	if (breadcrumbButton) {
		openBreadcrumbResource(breadcrumbButton.dataset.breadcrumbResource || "workspace").catch((err) => toast(errorMessage(err)));
		return;
	}
	const outsideAgentPanelMenu = (controllerState.agent.optionsOpen || controllerState.agent.historyOpen) && target && !target.closest(".chat-composer");
	if (outsideAgentPanelMenu) {
		controllerState.agent.optionsOpen = false;
		controllerState.agent.historyOpen = false;
		renderChatComposer();
	}
	});
	lifecycle?.listen(window, "beforeunload", flushAgentDraftOnPageLeave);
	lifecycle?.listen(document, "visibilitychange", () => {
		if (document.hidden || document.visibilityState === "hidden") flushAgentDraftOnPageLeave();
	});
}
let appBooted = false;
export function startPUAApp(nextPublisher: PUAViewPublisher): void {
	publisher = nextPublisher;
	if (appBooted) {
		publishAllViewModels();
		return;
	}
	appBooted = true;
	const scope = new ResourceScope();
	lifecycle = scope;
	notificationController = createNotificationController({
		scope,
		selectedResourceId: () => controllerState.selectedId,
		resourceProjections: () => resourceNotificationProjections(),
		findResource,
		selectResource,
		notificationsSettingsVisible: () => settingsController.isOpenTab("notifications"),
		renderSettings: renderSettingsModal,
		flushDraft: flushAgentDraftOnPageLeave
	});
	userSettingsController = createUserSettingsController(scope, () => {
		if (!controllerState.activeWorkspaceId) return;
		void loadWorkspaceContext().catch((err) => toast(errorMessage(err)));
	});
	installControllerListeners();
	initPaneResize();
	notificationController.install();
	renderAppShell();
	load().catch((err) => {
		controllerState.navigationLoading = false;
		controllerState.navigationError = err.message;
		toast(err.message);
		publishViewModels();
	});
	startAutoRefresh();
}
function flushAgentDraftOnPageLeave(): void {
	flushAgentDraft();
}
export function stopPUAApp(): void {
	if (!appBooted) return;
	flushAgentDraftOnPageLeave();
	appBooted = false;
	notificationController?.dispose();
	notificationController = null;
	userSettingsController = null;
	agentOperations.reset();
	schedulerMutationController.invalidate();
	clearAgentRenderTimer();
	createDialogController.dispose();
	lifecycle?.dispose();
	lifecycle = null;
	controllerState.autoRefreshTimer = null;
}
async function handleHistoryNavigation(pathname: string): Promise<void> {
	const route = parseRoute(pathname);
	if (!workspaceExists(route.workspaceId)) {
		syncURL({ replace: true });
		return;
	}
	const workspaceChanged = controllerState.activeWorkspaceId !== route.workspaceId;
	const previousSelectedId = controllerState.selectedId;
	flushAgentDraft();
	controllerState.navigationVersion++;
	schedulerMutationController.invalidate();
	controllerState.autoRefreshVersion++;
	controllerState.treeRequestVersion++;
	controllerState.detailRequestVersion++;
	controllerState.workspaceAgentsRequestVersion++;
	controllerState.diffRequestVersion++;
	const navigationVersion = controllerState.navigationVersion;
	controllerState.activeWorkspaceId = route.workspaceId || "";
	controllerState.selectedId = route.resourceId || "workspace";
	if (!workspaceChanged && previousSelectedId !== controllerState.selectedId && controllerState.selectedId !== "workspace") {
		resourceDetailController.reset(controllerState.selectedId);
		delete controllerState.details[controllerState.selectedId];
	}
	controllerState.diff = null;
	if (workspaceChanged) {
		controllerState.tree = null;
		controllerState.navigationLoading = true;
		controllerState.navigationError = "";
		closeCreateDialog();
		initializeNotificationState(controllerState.activeWorkspaceId);
	}
	if (workspaceChanged) resetAgentState();
	renderWorkspaceSelect();
	if (workspaceChanged) {
		controllerState.currentUserName = "";
		controllerState.userGate = { mode: "", suggestedUserName: "", missingUserName: "" };
		if (!await resolveWorkspaceIdentity(route.workspaceId || "")) return;
		if (!await loadUIState(route.workspaceId || "", navigationVersion)) return;
		if (!route.resourceId && controllerState.lastResourceId) controllerState.selectedId = controllerState.lastResourceId;
		await loadTree({ updateURL: false });
		if (isCurrentWorkspaceView(route.workspaceId || "", navigationVersion)) syncURL({ replace: true });
	} else {
		const selectionCorrected = ensureValidSelection();
		if (controllerState.selectedId === "workspace") await loadWorkspaceAgents();
		else {
			ensureSelectedProjectExpanded(false);
			await loadDetail(controllerState.selectedId);
		}
		if (!isCurrentWorkspaceView(route.workspaceId || "", navigationVersion)) return;
		if (previousSelectedId !== controllerState.selectedId) await reloadResourceForSelection();
		publishViewModels();
		if (selectionCorrected) syncURL({ replace: true });
	}
}
