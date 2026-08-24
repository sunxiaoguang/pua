import type { AgentOption } from "./common";
import type { ResourceAgentBindingModel, ResourceAgentProfileModel } from "./detail";

export interface UploadDialogModel {
  open: boolean;
  identity: string;
  workspaceId: string;
  resourceId: string;
  onDone: (paths: string[], context: { workspaceId: string; resourceId: string }) => void;
}

export interface ComposerModel {
  identity: string;
  workspaceId: string;
  resourceId: string;
  draft: string;
  draftKey: string;
  draftResetVersion: number;
  unavailableReason: string;
  sending: boolean;
  canEndTurn: boolean;
  endingTurn: boolean;
  canEndGeneration: boolean;
  endingGeneration: boolean;
  stopNotice: string;
  waitingMessages: WaitingMessage[];
  canSteerWaiting: boolean;
  steeringMessageId: string;
  agentBinding: ResourceAgentBindingModel;
  agentProfiles: ResourceAgentProfileModel[];
  agents: AgentOption[];
  bindingSaving: boolean;
  onDraft: (text: string, context: ComposerContext) => void;
  onSend: (text: string, context: ComposerContext) => Promise<{ accepted: boolean; clear: boolean }>;
  onOpenUpload: () => void;
  onEndTurn: () => void;
  onEndGeneration: () => void;
  onDismissStopNotice: () => void;
  onSteerWaiting: (messageId: string) => Promise<void>;
  onSaveAgentBinding: (binding: ResourceAgentBindingModel) => Promise<void>;
}

export interface WaitingMessage {
  messageId: string;
  text: string;
  status: string;
  acceptedAt: string;
  requestedMode: string;
  actualMode: string;
  canPromote: boolean;
}

export interface ResourceMessageStatus {
  resourceId: string;
  sessionState: "idle" | "working" | "attention_required" | "unavailable" | "archived";
  canSteerWaiting: boolean;
  exists?: boolean;
  archived?: boolean;
  acceptsMessages?: boolean;
  resolvedAgent?: string;
  resolvedProfile?: string;
  configError?: string;
  lastError?: string;
  generation?: ResourceGenerationStatus;
  session?: ResourceSessionStatus;
  waitingMessages: WaitingMessage[];
  messages?: { waiting?: number; delivering?: number; interrupting?: number; delivered?: number; cancelled?: number; undeliverable?: number; deliveryUnknown?: number };
}

export interface ComposerContext {
  workspaceId: string;
  resourceId: string;
  draftKey: string;
}

export interface ResourceGenerationStatus {
  generation: number;
  generationId: string;
  status: string;
  completionState?: string;
  completionHasFinalReply?: boolean;
  turnNumber?: number;
  replacementPending?: boolean;
  resumable?: boolean;
  idleSuspended?: boolean;
  resumeUnavailable?: boolean;
  agentHubSessionId?: string;
}

export interface ResourceSessionStatus {
  id?: string;
  state?: string;
  currentTurnId?: string;
  inputCapabilities?: { prompt?: boolean; steer?: boolean };
}

export interface AgentEvent {
  id: number;
  semanticId?: string;
  semanticIndex?: number;
  type: string;
  time?: string;
  startTime?: string;
  sessionId?: string;
  turnId?: string;
  data?: Record<string, unknown> & { text?: string; append?: boolean; state?: string };
}

export interface AgentSemanticFrame {
  schema: "agenthub.semantic-events.v1";
  cursor: number;
  mode: "replace" | "append";
  source: { eventId: number; type: string; sessionId: string; turnId?: string; time?: string; startTime?: string };
  events: Array<{
    id: string;
    sourceEventId: number;
    index: number;
    type: string;
    time?: string;
    startTime?: string;
    sessionId?: string;
    turnId?: string;
    data?: Record<string, unknown> & { text?: string; state?: string };
  }>;
}

export interface AgentTurnItem {
  type: string;
  role?: string;
  sender?: { name?: string; id?: string; sessionId?: string };
  steer?: boolean;
  text?: string;
  payload?: unknown;
  startEventId: number;
  endEventId: number;
  startedAt?: string;
  endedAt?: string;
  durationMs?: number;
  count?: number;
  thinkingCount?: number;
  reasoningUpdateCount?: number;
  toolCallCount?: number;
  data?: Record<string, unknown>;
}

export interface ResourceHistoryGeneration {
  generation: number;
  generationId: string;
  title: string;
  binding?: { kind?: string; name?: string };
  resolvedProfile?: string;
  agentName?: string;
  provider?: string;
  providerId?: string;
  model?: string;
  status: string;
  createdAt: string;
  updatedAt: string;
  replacementPending?: boolean;
}

export interface ResourceHistoryGap {
  code: string;
  message: string;
  retryable: boolean;
}

export interface ResourceHistoryTurnSummary {
  reference: string;
  turnId: string;
  status: string;
  closed: boolean;
  startedAt: string;
  endedAt?: string;
  durationMs: number;
  triggerPreview?: string;
  // triggerRole carries the turn opener's provenance ("user", "system",
  // "agent"); the chat timeline collapses closed non-user Turns by default.
  triggerRole?: string;
  triggerSender?: { name?: string; id?: string; sessionId?: string };
  finalReplyPreview?: string;
  eventCount: number;
  toolEventCount: number;
  startEventId: number;
  lastEventId: number;
  endEventId?: number;
  generation: ResourceHistoryGeneration;
}

export interface ResourceHistoryTurnDetail {
  turn: ResourceHistoryTurnSummary;
  items: AgentTurnItem[];
  latestEventId: number;
}

export interface ResourceHistorySegment {
  generation: ResourceHistoryGeneration;
  turns: ResourceHistoryTurnSummary[];
  gap?: ResourceHistoryGap;
}

export interface ResourceHistoryPage {
  resourceId: string;
  segments: ResourceHistorySegment[];
  page: { limit: number; nextCursor?: string; hasMore: boolean };
}

export interface ConversationBlock {
  kind: "turn" | "gap";
  key: string;
  generation: ResourceHistoryGeneration;
  turn?: ResourceHistoryTurnSummary;
  items?: TimelineItem[];
  events?: AgentEvent[];
  loading?: boolean;
  error?: string;
  gap?: ResourceHistoryGap;
}

export interface AgentNotice {
  source?: string;
  type?: string;
  data?: Record<string, unknown> & {
    level?: string;
    method?: string;
    kind?: string;
    lifecycle?: string;
    resourceId?: string;
    text?: string;
  };
}

export interface TimelineItem {
  kind: string;
  key?: string | number;
  generationId?: string;
  role?: string;
  text?: string;
  time?: string;
  startTime?: string;
  active?: boolean;
  steer?: boolean;
  sender?: { name?: string; id?: string; sessionId?: string };
  payload?: unknown;
  tone?: string;
  type?: string;
  preview?: string;
  count?: number;
  calls?: Array<Record<string, unknown> & { key?: string | number; callId?: string; name?: string; summary?: string; status?: string; output?: string; error?: string; method?: string; rawPreview?: string }>;
  items?: TimelineItem[];
  thinkingCount?: number;
  reasoningUpdateCount?: number;
  toolCallCount?: number;
  approvalId?: string;
  title?: string;
  detail?: string;
  question?: string;
  options?: Array<{ optionId: string; name?: string; kind?: string }>;
  status?: string;
  decision?: string;
  reply?: string;
  compact?: boolean;
  rangeStartEventId?: number;
  rangeEndEventId?: number;
  // turnFinal marks the last assistant message of a turn; earlier mid-turn
  // progress updates get turnFinal=false so the UI can mute their rail.
  turnFinal?: boolean;
  // agentStart marks the head of an uninterrupted run of agent activity
  // (reasoning, tool calls, approvals) that opens before the turn's first
  // assistant message. The run head renders a header with the agent's name
  // and the run's start time so the label attaches to the first event even
  // when reasoning or tool calls precede the first progress update.
  // Assistant messages always render their own meta row and never carry
  // this annotation.
  agentStart?: boolean;
}

export interface ChatContextSnapshot {
  identity: string;
  workspaceId: string;
  resourceId: string;
  generationId: string;
  blocks: ConversationBlock[];
  notices: AgentNotice[];
  hasMoreBefore: boolean;
  loading: boolean;
  loadingOlder: boolean;
  loaded: boolean;
  error: string;
}

export interface AgentPanelHeaderModel {
  identity: string;
  workspaceId: string;
  resourceId: string;
  status: ResourceMessageStatus | null;
  /** True while a user-initiated message submission is in flight. */
  submitting: boolean;
  agentName: string;
  /** "Provider · model" summary of the resolved agent, may be empty. */
  modelSummary: string;
  turnNumber: number;
  /** ISO timestamp of the running turn, empty when unknown. */
  turnStartedAt: string;
}

export interface EventTimelineModel {
  identity: string;
  workspaceId: string;
  resourceId: string;
  status: ResourceMessageStatus | null;
  submitting: boolean;
  agentName: string;
  resolveResourceTitle: (resourceId: string) => string | null;
  onNavigate: (resourceId: string) => void;
  project: (events: AgentEvent[]) => TimelineItem[];
  onEvent: (workspaceId: string, resourceId: string, event: AgentEvent) => void;
  onNotice: (workspaceId: string, resourceId: string, notice: AgentNotice) => void;
  onApproval: (generationId: string, approvalId: string, reply: { decision?: string; optionId?: string; text?: string }) => Promise<void>;
  onToast: (message: string) => void;
}
