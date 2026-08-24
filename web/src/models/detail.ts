import type { TaskTemplate } from "./create";
import type { AgentOption } from "./common";
import type { GenerationPolicyRecord, SchedulerConfigRecord, SchedulerRevision, StallWatchdogPolicyRecord, WorkspaceUser } from "./workspace";

export interface ResourceAgentBindingModel {
  kind: "profile" | "agent";
  name: string;
}

export interface ResourceAgentProfileModel {
  key: string;
  description?: string;
  agentName?: string;
}

export interface ResourceFileModel {
  name: string;
  path?: string;
  content: string;
  contentHash?: string;
}

export interface FileTreeModel {
  name: string;
  path: string;
  type: "file" | "directory" | string;
  size?: number;
  modified?: string;
  children?: FileTreeModel[];
}

export interface ResourceRepoModel {
  name?: string;
  worktreePath?: string;
  branch?: string;
  targetBranch?: string;
}

export interface ResourceDetailModel {
  id: string;
  type: "scheduler" | "project" | "task";
  title: string;
  description?: string;
  path: string;
  archived?: boolean;
  agentBinding?: ResourceAgentBindingModel;
  taskDefault?: ResourceAgentBindingModel;
  files?: ResourceFileModel[];
  artifacts?: FileTreeModel[];
  repos?: ResourceRepoModel[];
  templates?: TaskTemplate[];
  template?: { name: string; schemaVersion?: number; digest?: string } | null;
  scheduler?: SchedulerConfigRecord;
}

export interface FilePreviewModel {
  path: string;
  name?: string;
  size?: number;
  content?: string;
  contentHash?: string;
  truncated?: boolean;
  binary?: boolean;
  image?: boolean;
  mimeType?: string;
}

export interface DiffPreviewModel {
  path: string;
  name?: string;
  branch?: string;
  base?: string;
  diff?: string;
  hasChanges?: boolean;
}

export interface WorkspaceAgentsModel extends Partial<FilePreviewModel> {
  path: string;
  content?: string;
  error?: string;
}

interface SchedulerSaveFields {
  description: string;
  condition: string;
  target: string;
}

export type SchedulerSaveInput = SchedulerSaveFields & (
  | { scheduleId?: undefined; expectedRevision?: undefined }
  | { scheduleId: string; expectedRevision: SchedulerRevision }
);

export interface SchedulerMutationCallbacks {
  validateTarget: (target: string) => string;
  save: (input: SchedulerSaveInput) => Promise<boolean>;
  setPaused: (scheduleId: string, paused: boolean) => Promise<boolean>;
  remove: (scheduleId: string) => Promise<boolean>;
}

export interface DetailPanelModel {
  identity: string;
  workspaceId: string;
  workspaceName: string;
  resourceId: string;
  resourceType: "workspace" | "scheduler" | "project" | "task" | "";
  resourceTitle: string;
  parent?: { id: string; title: string } | null;
  loading: boolean;
  detail: ResourceDetailModel | null;
  wiki: { exists?: boolean; error?: string; entries?: FileTreeModel[] } | null;
  workspaceAgents: WorkspaceAgentsModel | null;
  workspaceDefaults: { project: ResourceAgentBindingModel; task: ResourceAgentBindingModel };
  workspaceUsers: WorkspaceUser[];
  currentUserName: string;
  generationPolicy: GenerationPolicyRecord;
  stallWatchdogPolicy: StallWatchdogPolicyRecord;
  agentBinding: ResourceAgentBindingModel;
  agentProfiles: ResourceAgentProfileModel[];
  agents: AgentOption[];
  resolveResourceTitle: (resourceId: string) => string | null;
  onNavigate: (resourceId: string) => void;
  onCreateTask: (projectId: string) => void;
  onArchive: (resourceId: string) => void;
  onSaveWorkspaceAgents: (content: string, expectedContentHash: string) => Promise<WorkspaceAgentsModel>;
  onSaveMarkdownFile: (path: string, content: string, expectedContentHash: string) => Promise<FilePreviewModel>;
  onDeleteArtifact: (path: string) => Promise<void>;
  onSaveAgentBinding: (binding: ResourceAgentBindingModel) => Promise<void>;
  onRenameResource: (title: string) => Promise<void>;
  onSaveDescription: (description: string) => Promise<void>;
  onSaveWorkspaceDefaults: (defaults: { project: ResourceAgentBindingModel; task: ResourceAgentBindingModel }) => Promise<void>;
  onSaveWorkspaceUserPreference: (name: string, preference: string) => Promise<void>;
  onSwitchWorkspaceUser: (name: string) => Promise<void>;
  onAddWorkspaceUser: (name: string) => Promise<void>;
  onDeleteWorkspaceUser: (name: string) => Promise<void>;
  onSaveGenerationPolicy: (policy: GenerationPolicyRecord) => Promise<void>;
  onSaveStallWatchdogPolicy: (policy: StallWatchdogPolicyRecord) => Promise<void>;
  onSaveTaskDefault: (projectId: string, binding: ResourceAgentBindingModel | null) => Promise<void>;
  schedulerActions?: SchedulerMutationCallbacks;
  onToast: (message: string) => void;
}
