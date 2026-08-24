import type { TaskTemplate } from "./create";
import type { FileTreeModel, ResourceFileModel, ResourceRepoModel } from "./detail";

export interface ResourceRecord {
  id: string;
  type?: string;
  title?: string;
  path?: string;
  description?: string;
  state?: "not_started" | "in_progress" | "waiting" | "blocked" | "paused" | "completed" | "error";
  stateNote?: string;
  stateUpdatedAt?: string;
  archived?: boolean;
  agentBinding?: { kind: "profile" | "agent"; name: string };
  taskDefault?: { kind: "profile" | "agent"; name: string };
  updatedAt?: string;
  children?: ResourceRecord[];
  files?: ResourceFileModel[];
  artifacts?: FileTreeModel[];
  repos?: ResourceRepoModel[];
  templates?: TaskTemplate[];
  template?: { name: string; schemaVersion?: number; digest?: string } | null;
  wiki?: { exists?: boolean; error?: string; entries?: FileTreeModel[] };
  scheduler?: SchedulerConfigRecord;
  runtime?: ResourceRuntime;
  userState?: ResourceUserState;
  latestTurnNumber?: number;
  latestTurnAt?: string;
  latestAgentName?: string;
  unreadCount?: number;
}

export interface ResourceUserState {
  readTurnNumber?: number;
}

export interface ResourceRuntime {
  generation?: number;
  generationId?: string;
  status?: string;
  sessionState?: "idle" | "working" | "attention_required" | "unavailable" | "archived";
  agentName?: string;
  updatedAt?: string;
  lastOutputAt?: string;
  completionMarker?: string;
  completionState?: string;
  completionAt?: string;
  replacementPending?: boolean;
  resumable?: boolean;
  idleSuspended?: boolean;
  resumeUnavailable?: boolean;
  turnNumber?: number;
  activeTurn?: boolean;
  turnStartedAt?: string;
}

export type SchedulerRevision = string;

const maximumSchedulerRevision: SchedulerRevision = "18446744073709551615";

export function isSchedulerRevision(value: unknown): value is SchedulerRevision {
  if (typeof value !== "string" || !/^[1-9][0-9]*$/.test(value)) return false;
  return value.length < maximumSchedulerRevision.length
    || (value.length === maximumSchedulerRevision.length && value <= maximumSchedulerRevision);
}

export interface ScheduleRecord {
  id: string;
  revision: SchedulerRevision;
  description: string;
  condition: string;
  guard?: string;
  target: string;
  state: "active" | "paused" | "completed" | "needs_compilation";
  trigger?:
    | { type: "at"; at: string }
    | { type: "interval"; everySeconds: number; anchorAt: string }
    | { type: "cron"; cron: string; timeZone: string };
  createdAt: string;
  updatedAt: string;
  effectiveState: string;
  nextRunAt?: string;
  lastOccurrenceAt?: string;
  lastOutcome?: string;
  lastError?: string;
}

export interface SchedulerConfigRecord {
  schemaVersion: number;
  agentBinding: { kind: "profile" | "agent"; name: string };
  schedules: ScheduleRecord[];
  nextWakeAt?: string;
}

export interface ResourceAgentDefaultsRecord {
  project: { kind: "profile" | "agent"; name: string };
  task: { kind: "profile" | "agent"; name: string };
}

export interface GenerationPolicyRecord {
  enabled: boolean;
  maxTurns: number;
  maxAccumulatedTurnMinutes: number;
}

export interface StallWatchdogPolicyRecord {
  enabled: boolean;
  timeoutMinutes: number;
}

export interface WorkspaceTree {
  agentBinding?: { kind: "profile" | "agent"; name: string };
  resourceDefaults?: ResourceAgentDefaultsRecord;
  generationPolicy?: GenerationPolicyRecord;
  stallWatchdogPolicy?: StallWatchdogPolicyRecord;
  workspace?: ResourceRecord;
  scheduler?: ResourceRecord;
  projects: ResourceRecord[];
  activity?: ResourceActivityLists;
  wiki?: { exists?: boolean; error?: string; entries?: FileTreeModel[] };
}

export interface ResourceActivityLists {
  running: ResourceRecord[];
  unread: ResourceRecord[];
  problems: ResourceRecord[];
}

export interface WorkspaceFileRecord {
  path: string;
  name?: string;
  section?: string;
  size?: number;
  content?: string;
  contentHash?: string;
  truncated?: boolean;
  binary?: boolean;
  image?: boolean;
  mimeType?: string;
  loading?: boolean;
  error?: string;
}

export interface DiffRecord {
  path?: string;
  name?: string;
  branch?: string;
  base?: string;
  diff?: string;
  hasChanges?: boolean;
}

export interface AgentConfig {
  id: string;
  name?: string;
  available?: boolean;
  providerId?: string;
  options?: { model?: string };
}

export interface AgentProfile {
  key: string;
  description: string;
  agentName: string;
}

export interface WorkspaceConfig {
  activeId?: string;
  workspaces: Array<{ id: string; instanceId?: string; name: string; path: string; icon?: string }>;
  suggestedUserName?: string;
  agents: AgentConfig[];
  agentProfiles: AgentProfile[];
  agentHubProviders?: Array<{ id: string; name?: string }>;
  // Content hash of the serve configuration; polled via /api/settings/revision
  // to detect settings changes made by other tabs or clients.
  revision?: string;
}

export interface WorkspaceUser {
  version: number;
  name: string;
  preference: string;
}
