import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

const root = resolve(import.meta.dirname, "../..");
const components = [
  "AgentPanelHeader",
  "ActivityGroup",
  "AppearanceSettingsPanel",
  "ActivityPanel",
  "ApprovalCard",
  "AppShell",
  "AgentHubSettingsPanel",
  "ChatComposer",
  "ConfirmDialog",
  "CreateDialog",
  "ProjectCreateForm",
  "TaskWizard",
  "TemplateFieldGroup",
  "TemplatePicker",
  "DetailPanel",
  "DiffModal",
  "DoctorDialog",
  "EventTimeline",
  "FileBrowser",
  "FilePreviewFullscreen",
  "FilePreviewModal",
  "HistoryTimeline",
  "LifecycleNotice",
  "MarkdownDocument",
  "MarkdownEditor",
  "MobileToolbar",
  "NotificationSettingsPanel",
  "PaneResizeHandle",
  "ProfilesSettingsPanel",
  "ProjectTree",
  "SchedulerPanel",
  "SettingsModal",
  "SettingsNavigation",
  "StatusPresentation",
  "ThinkingBlock",
  "TimelineMessage",
  "TimelineNotice",
  "Toast",
  "ToolGroup",
  "ToolItem",
  "UnknownEvent",
  "UploadDialog",
  "WorkspaceSettingsPanel",
  "WorkspaceSwitcher",
] as const;

const owners: Record<(typeof components)[number], string> = {
  AgentPanelHeader: "agent-panel-header",
  ActivityGroup: "event-timeline",
  AppearanceSettingsPanel: "appearance-settings-panel",
  ActivityPanel: "attention-list",
  ApprovalCard: "event-timeline",
  AppShell: "app-shell",
  AgentHubSettingsPanel: "agenthub-settings-panel",
  ChatComposer: "chat-composer",
  ConfirmDialog: "confirm-dialog",
  CreateDialog: "create-dialog",
  ProjectCreateForm: "project-create-form",
  TaskWizard: "task-wizard",
  TemplateFieldGroup: "template-field-group",
  TemplatePicker: "template-picker",
  DetailPanel: "detail-panel",
  DiffModal: "diff-modal",
  DoctorDialog: "doctor-dialog",
  EventTimeline: "event-timeline",
  FileBrowser: "file-browser",
  FilePreviewFullscreen: "file-preview-fullscreen",
  FilePreviewModal: "file-preview-modal",
  HistoryTimeline: "history-timeline",
  LifecycleNotice: "event-timeline",
  MarkdownDocument: "markdown-document",
  MarkdownEditor: "markdown-editor",
  MobileToolbar: "mobile-toolbar",
  NotificationSettingsPanel: "notification-settings-panel",
  PaneResizeHandle: "pane-resize-handle",
  ProfilesSettingsPanel: "profiles-settings-panel",
  ProjectTree: "project-tree",
  SchedulerPanel: "scheduler-panel",
  SettingsModal: "settings",
  SettingsNavigation: "settings-navigation",
  StatusPresentation: "status-presentation",
  ThinkingBlock: "event-timeline",
  TimelineMessage: "event-timeline",
  TimelineNotice: "event-timeline",
  Toast: "toast",
  ToolGroup: "event-timeline",
  ToolItem: "event-timeline",
  UnknownEvent: "event-timeline",
  UploadDialog: "upload-dialog",
  WorkspaceSettingsPanel: "workspace-settings-panel",
  WorkspaceSwitcher: "workspace-switcher",
};

function read(relativePath: string): string {
  return readFileSync(resolve(root, relativePath), "utf8");
}

function zIndexes(relativePath: string): number[] {
  return [...read(relativePath).matchAll(/z-index:\s*(\d+)/g)].map((match) => Number(match[1]));
}

function selectorHeaders(css: string): string[] {
  const source = css.replaceAll(/\/\*[\s\S]*?\*\//g, "");
  const headers: string[] = [];
  const stack: Array<"at" | "keyframes" | "rule" | "keyframe-step"> = [];
  let buffer = "";
  for (const character of source) {
    if (character === "{") {
      const header = buffer.trim().replaceAll(/\s+/g, " ");
      buffer = "";
      if (header.startsWith("@")) stack.push(header.includes("keyframes") ? "keyframes" : "at");
      else if (stack.at(-1) === "keyframes") stack.push("keyframe-step");
      else {
        headers.push(header);
        stack.push("rule");
      }
    } else if (character === "}") {
      stack.pop();
      buffer = "";
    } else if (character === ";") buffer = "";
    else buffer += character;
  }
  return headers.filter(Boolean);
}

describe("CSS ownership", () => {
  it("defines every design token referenced by the stylesheets", () => {
    const defined = new Set<string>();
    for (const file of ["src/styles/tokens.css", "src/styles/themes-slate.css", "src/styles/themes-riso.css"]) {
      for (const match of read(file).matchAll(/(--[a-z0-9-]+)\s*:/gi)) defined.add(match[1]);
    }
    // Tokens applied at runtime by controllers (pane sizes, font scales,
    // mobile viewport metrics) rather than by stylesheets.
    const runtimeTokens = new Set([
      "--sidebar-width", "--chat-width", "--sidebar-attention-height",
      "--sidebar-font-scale", "--details-font-scale", "--chat-font-scale",
      "--app-viewport-height", "--app-viewport-offset-top", "--app-viewport-offset-left",
      // Component-local variables set inline by FileBrowser rows.
      "--depth"
    ]);
    const sources = [
      ...components.map((name) => `src/components/${name === "ActivityPanel" ? "AttentionList" : name}.css`),
      "src/styles/base.css", "src/styles/primitives.css", "src/styles/rich-content.css", "src/styles/themes-riso.css"
    ];
    const missing = new Map<string, string[]>();
    for (const file of sources) {
      for (const match of read(file).matchAll(/var\((--[a-z0-9-]+)/gi)) {
        const token = match[1];
        if (defined.has(token) || runtimeTokens.has(token)) continue;
        if (!missing.has(token)) missing.set(token, []);
        missing.get(token)!.push(file);
      }
    }
    expect([...missing.entries()].map(([token, files]) => `${token} used by ${[...new Set(files)].join(", ")}`)).toEqual([]);
  });

  it("keeps the global entry limited to documented shared layers", () => {
    expect(read("src/app.css").trim()).toBe([
      "/* Global CSS entry: tokens, browser defaults, and deliberately shared primitives only. */",
      '@import "./styles/tokens.css";',
      '@import "./styles/base.css";',
      '@import "./styles/primitives.css";',
      '@import "./styles/rich-content.css";',
      '@import "./styles/themes-slate.css";',
      '@import "./styles/themes-riso.css";',
    ].join("\n"));
    expect(selectorHeaders(read("src/styles/base.css"))).not.toEqual(expect.arrayContaining([expect.stringMatching(/\.[a-z]/)]));
  });

	  it.each(components)("keeps %s selectors inside its component boundary", (component) => {
	    const owner = owners[component];
	    const componentSource = read(`src/components/${component}.svelte`);
	    const cssName = component === "ActivityPanel" ? "AttentionList" : component;
	    const css = read(`src/components/${cssName}.css`);
	    expect(componentSource).toContain(`import "./${cssName}.css";`);
    for (const header of selectorHeaders(css)) {
      for (const selector of header.split(",")) {
        const normalized = selector.trim();
        const paneBodyState = component === "PaneResizeHandle" && /^body\.resizing(?:-[xy])?$/.test(normalized);
        expect(paneBodyState || normalized.includes(`[data-component-owner="${owner}"]`), normalized).toBe(true);
      }
    }
  });

  it("limits generated rich HTML rules to the sanitized markdown wrapper", () => {
    for (const header of selectorHeaders(read("src/styles/rich-content.css"))) {
      for (const selector of header.split(",")) expect(selector.trim()).toMatch(/^\.markdown-rendered(?:\W|$)/);
    }
  });

  it("keeps shared settings panel rules inside explicit panel roots", () => {
    for (const header of selectorHeaders(read("src/components/SettingsPanel.css"))) {
      for (const selector of header.split(",")) expect(selector.trim()).toContain("[data-component-owner][data-settings-panel]");
    }
  });

  it("keeps file previews above application navigation and below higher-priority dialogs", () => {
    const filePreview = Math.max(...zIndexes("src/components/FilePreviewModal.css"));
    const navigation = Math.max(...zIndexes("src/components/AppShell.css"), ...zIndexes("src/components/MobileToolbar.css"));
    const higherPriorityDialogs = ["CreateDialog", "UploadDialog", "SettingsModal", "ConfirmDialog", "DoctorDialog"]
      .map((component) => Math.max(...zIndexes(`src/components/${component}.css`)));

    expect(filePreview).toBeGreaterThan(navigation);
    expect(filePreview).toBeLessThan(Math.min(...higherPriorityDialogs));
  });

  it("keeps file preview header actions at their intrinsic width on narrow layouts", () => {
    const css = read("src/components/FilePreviewModal.css");
    const selector = ':where([data-component-owner="file-preview-modal"]) .file-modal-actions > .secondary-button';
    const start = css.indexOf(selector);
    expect(start).toBeGreaterThanOrEqual(0);
    expect(css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1)).toContain("width: auto;");
  });

  it("keeps Wiki file preview actions at a 44px mobile touch size", () => {
    const previewCss = read("src/components/FilePreviewModal.css");
    const previewMobileStart = previewCss.indexOf("@media (max-width:980px)");
    expect(previewMobileStart).toBeGreaterThanOrEqual(0);
    const previewMobileCss = previewCss.slice(previewMobileStart);
    expect(previewMobileCss).toContain(".file-modal-actions > .secondary-button {\n    min-height: 44px;");
    expect(previewMobileCss).toContain(".file-modal-actions > .icon-button {\n    flex: 0 0 44px;\n    width: 44px;\n    height: 44px;");

    const browserCss = read("src/components/FileBrowser.css");
    const browserMobileStart = browserCss.indexOf("@media (max-width: 980px)");
    expect(browserMobileStart).toBeGreaterThanOrEqual(0);
    expect(browserCss.slice(browserMobileStart)).toContain(".artifact-download {\n    flex: 0 0 44px;\n    width: 44px;\n    height: 44px;");
  });

  it("keeps the System Settings close control at the mobile touch target size", () => {
    const css = read("src/components/SettingsModal.css");
    const selector = ':where([data-component-owner="settings"]) .settings-close';
    const start = css.indexOf(selector);
    expect(start, selector).toBeGreaterThanOrEqual(0);
    const rule = css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    expect(rule).toContain("width: 44px;");
    expect(rule).toContain("height: 44px;");
  });

  it("keeps the Workspace problems close control touch-sized without moving list scrolling", () => {
    const css = read("src/components/DoctorDialog.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const close = body(':where([data-component-owner="doctor-dialog"]) .doctor-close {');
    expect(close).toContain("flex: 0 0 44px;");
    expect(close).toContain("width: 44px;");
    expect(close).toContain("height: 44px;");
    expect(close).toContain("margin: -5px;");

    const content = body(':where([data-component-owner="doctor-dialog"]) .doctor-content');
    expect(content).toContain("overflow-y: auto;");
  });

  it("keeps the mobile navigation trigger at a 44px touch size without growing the toolbar row", () => {
    const css = read("src/components/MobileToolbar.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const toolbar = body(':where([data-component-owner="mobile-toolbar"]).mobile-toolbar {');
    expect(toolbar).toContain("grid-template-columns: 44px minmax(0, 1fr) 44px;");
    expect(toolbar).toContain("padding: 4px 10px;");
    expect(toolbar).toContain("padding-top: calc(4px + env(safe-area-inset-top, 0px));");

    const trigger = body(':where([data-component-owner="mobile-toolbar"]) .mobile-icon-button');
    expect(trigger).toContain("width: 44px;");
    expect(trigger).toContain("height: 44px;");
  });

  it("keeps the mobile navigation drawer brand actions at a 44px touch size", () => {
    const css = read("src/components/AppShell.css");
    const body = (selector: string) => {
      const start = css.lastIndexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const mobileStart = css.indexOf("@media (max-width:980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);
    for (const selector of [
      ':where([data-component-owner="app-shell"]) .brand-settings {',
      ':where([data-component-owner="app-shell"]) .brand-doctor {',
    ]) {
      expect(css.lastIndexOf(selector), selector).toBeGreaterThan(mobileStart);
    }

    const settings = body(':where([data-component-owner="app-shell"]) .brand-settings {');
    expect(settings).toContain("flex: 0 0 44px;");
    expect(settings).toContain("width: 44px;");
    expect(settings).toContain("height: 44px;");

    const doctor = body(':where([data-component-owner="app-shell"]) .brand-doctor {');
    expect(doctor).toContain("min-width: 44px;");
    expect(doctor).toContain("height: 44px;");
  });

  it("keeps mobile Projects title actions at a 44px touch size", () => {
    const css = read("src/components/ProjectTree.css");
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);

    const body = (selector: string) => {
      const start = css.lastIndexOf(selector);
      expect(start, selector).toBeGreaterThan(mobileStart);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const title = body(':where([data-component-owner="project-tree"]) .section-title {');
    expect(title).toContain("min-height: 44px;");

    const actions = body(':where([data-component-owner="project-tree"]) .section-title button {');
    expect(actions).toContain("min-width: 44px;");
    expect(actions).toContain("height: 44px;");
  });

  it("keeps resource detail tabs at a mobile touch target size", () => {
    const css = read("src/components/DetailPanel.css");
    const selector = ':where([data-component-owner="detail-panel"]) .details-tab';
    const start = css.indexOf(`${selector} {`);
    expect(start, selector).toBeGreaterThanOrEqual(0);
    const rule = css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);

    // The four Project detail tabs must remain easy to tap at the narrowest
    // supported viewport without changing their horizontal layout.
    expect(rule).toContain("min-height: 44px;");
  });

  it("keeps WorkspaceSwitcher controls at a 44px mobile touch size without growing its header", () => {
    const css = read("src/components/WorkspaceSwitcher.css");
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);
    const mobileCss = css.slice(mobileStart);

    expect(mobileCss).toContain(
      ':where([data-component-owner="workspace-switcher"]) .workspace-switcher-head {\n    padding: 1px 6px;\n  }',
    );
    expect(mobileCss).toContain(
      ':where([data-component-owner="workspace-switcher"]) .workspace-open,\n  :where([data-component-owner="workspace-switcher"]) .workspace-switcher-menu-button {\n    min-height: 44px;\n  }',
    );
    expect(mobileCss).toContain(
      ':where([data-component-owner="workspace-switcher"]) .workspace-switcher-menu-button {\n    height: 44px;\n  }',
    );
  });

  it("keeps mobile file-browser rows at a 44px touch size", () => {
    const css = read("src/components/FileBrowser.css");
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);
    expect(css.slice(mobileStart)).toContain(
      ':where([data-component-owner="file-browser"]) .artifact-row {\n    min-height: 44px;\n  }',
    );
  });

  it("keeps mobile ProjectTree resource rows at a 44px touch size", () => {
    const css = read("src/components/ProjectTree.css");
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);
    expect(css.slice(mobileStart)).toContain(
      ':where([data-component-owner="project-tree"]) .tree-item {\n    min-height: 44px;\n  }',
    );
  });

  it("truncates overlong workspace names instead of overflowing the settings row", () => {
    const css = read("src/components/WorkspaceSettingsPanel.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    // The name itself ellipsizes like the path does.
    const name = body(':where([data-component-owner="workspace-settings-panel"]) .settings-row-main strong');
    expect(name).toContain("overflow: hidden;");
    expect(name).toContain("text-overflow: ellipsis;");
    expect(name).toContain("white-space: nowrap;");

    // The row is a grid item, so it also needs min-width: 0 to shrink below its
    // min-content instead of pushing the actions out of the viewport.
    const row = body(':where([data-component-owner="workspace-settings-panel"]) .settings-list-row');
    expect(row).toContain("min-width: 0;");
  });

  it("keeps the chat composer send button inside very narrow viewports", () => {
    const body = (path: string, selector: string) => {
      const css = read(path);
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    // At ~220px the agent binding label (e.g. "default (current: codex)") is
    // wider than the composer card; every flex box on the chain from the
    // composer bar to the label must allow shrinking so the label ellipsizes
    // instead of pushing the send button past the right edge.
    const options = body("src/components/ChatComposer.css", ':where([data-component-owner="chat-composer"]) .chat-composer-options');
    expect(options).toContain("min-width: 0;");

    const binding = body("src/components/ChatComposer.css", ':where([data-component-owner="chat-composer"]) .chat-agent-binding');
    expect(binding).toContain("flex: 0 1 auto;");
    expect(binding).toContain("min-width: 0;");

    const button = body("src/components/AgentBindingSelector.css", ':where([data-component-owner="agent-binding-selector"]) .agent-binding-button');
    expect(button).toContain("flex: 0 1 auto;");
    expect(button).toContain("min-width: 0;");
    expect(button).toContain("max-width: 100%;");

    const label = body("src/components/AgentBindingSelector.css", ':where([data-component-owner="agent-binding-selector"]) .agent-binding-label');
    expect(label).toContain("min-width: 0;");
    expect(label).toContain("overflow: hidden;");
    expect(label).toContain("text-overflow: ellipsis;");
    expect(label).toContain("white-space: nowrap;");
  });

  it("keeps composer actions at a mobile touch size without growing the bar", () => {
    const css = read("src/components/ChatComposer.css");
    const touchSelector = ':where([data-component-owner="chat-composer"]) .chat-send-button,';
    const touchStart = css.indexOf(touchSelector);
    expect(touchStart, touchSelector).toBeGreaterThanOrEqual(0);
    const touchRule = css.slice(css.indexOf("{", touchStart), css.indexOf("}", touchStart) + 1);
    expect(touchRule).toContain("width: 44px;");
    expect(touchRule).toContain("height: 44px;");
    expect(touchRule).toContain("min-width: 44px;");
    expect(touchRule).toContain("flex: 0 0 44px;");
    expect(touchRule).toContain("margin: -6px;");

    const adjacentSelector = ':where([data-component-owner="chat-composer"]) .chat-composer-action + .chat-send-button';
    const adjacentStart = css.indexOf(adjacentSelector);
    expect(adjacentStart, adjacentSelector).toBeGreaterThanOrEqual(0);
    const adjacentRule = css.slice(css.indexOf("{", adjacentStart), css.indexOf("}", adjacentStart) + 1);
    expect(adjacentRule).toContain("margin-left: 6px;");
  });

  it("keeps the composer binding trigger at a mobile touch size and menu anchor", () => {
    const css = read("src/components/ChatComposer.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const root = body(':where([data-component-owner="chat-composer"]) .chat-agent-binding .agent-binding');
    expect(root).toContain("height: 44px;");
    expect(root).toContain("margin: -6px 0;");

    const button = body(':where([data-component-owner="chat-composer"]) .chat-agent-binding .agent-binding-button');
    expect(button).toContain("height: 44px;");
    expect(button).toContain("min-height: 44px;");
  });

  it("keeps Project detail actions at a 44px touch size without narrow-layout overflow", () => {
    const detailCss = read("src/components/DetailPanel.css");
    const detailStart = detailCss.indexOf(':where([data-component-owner="detail-panel"]) .details-actions button {');
    expect(detailStart).toBeGreaterThanOrEqual(0);
    const detailRule = detailCss.slice(detailCss.indexOf("{", detailStart), detailCss.indexOf("}", detailStart) + 1);
    expect(detailRule).toContain("min-height: 44px;");

    const markdownCss = read("src/components/MarkdownDocument.css");
    const markdownSelector = ':where([data-component-owner="markdown-document"]) .markdown-document-actions .secondary-button';
    const markdownStart = markdownCss.indexOf(markdownSelector);
    expect(markdownStart).toBeGreaterThanOrEqual(0);
    const markdownRule = markdownCss.slice(markdownCss.indexOf("{", markdownStart), markdownCss.indexOf("}", markdownStart) + 1);
    expect(markdownRule).toContain("min-height: 44px;");

    const narrowRuleStart = markdownCss.lastIndexOf(`${markdownSelector} {`);
    expect(narrowRuleStart).toBeGreaterThanOrEqual(0);
    const narrowRule = markdownCss.slice(markdownCss.indexOf("{", narrowRuleStart), markdownCss.indexOf("}", narrowRuleStart) + 1);
    expect(narrowRule).toContain("width: auto;");
  });

  it("keeps Workspace user actions at a 44px mobile touch size", () => {
    const css = read("src/components/ResourceSettingsPanel.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const deleteAction = body(':where([data-component-owner="resource-settings-panel"]) .resource-settings-user-actions .secondary-button');
    const saveAction = body(':where([data-component-owner="resource-settings-panel"]) .resource-settings-user-save');
    expect(deleteAction).toContain("min-height: 44px;");
    expect(saveAction).toContain("min-height: 44px;");

    // Delete remains intrinsic-width on narrow layouts, while Save preference
    // can continue using the card width without changing either button's role.
    const narrowDelete = body('.resource-settings-user-actions .secondary-button');
    expect(narrowDelete).toContain("width: auto;");
  });

  it("keeps Workspace Agent binding selectors at a 44px mobile touch size", () => {
    const css = read("src/components/ResourceSettingsPanel.css");
    const selector = ':where([data-component-owner="resource-settings-panel"]) .resource-settings-agent-bindings .agent-binding-button';
    const start = css.indexOf(selector);
    expect(start, selector).toBeGreaterThanOrEqual(0);
    const rule = css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    expect(rule).toContain("min-height: 44px;");

    // The shared selector keeps its width constraints so the taller mobile
    // hit target cannot introduce horizontal overflow in the settings card.
    const selectorCss = read("src/components/AgentBindingSelector.css");
    const buttonSelector = ':where([data-component-owner="agent-binding-selector"]) .agent-binding-button';
    const buttonStart = selectorCss.indexOf(buttonSelector);
    expect(buttonStart, buttonSelector).toBeGreaterThanOrEqual(0);
    const buttonRule = selectorCss.slice(selectorCss.indexOf("{", buttonStart), selectorCss.indexOf("}", buttonStart) + 1);
    expect(buttonRule).toContain("max-width: 100%;");
  });

  it("keeps the create dialog close control at a mobile touch size without changing the header footprint", () => {
    const css = read("src/components/CreateDialog.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    const title = body(':where([data-component-owner="create-dialog"]) .create-dialog-header > div');
    const close = body(':where([data-component-owner="create-dialog"]) .create-dialog-header > .icon-button');
    expect(title).toContain("min-width: 0;");
    expect(close).toContain("flex: 0 0 44px;");
    expect(close).toContain("width: 44px;");
    expect(close).toContain("height: 44px;");
    expect(close).toContain("margin: -7px;");
  });

  it("keeps Project Settings General actions at a mobile touch size", () => {
    const css = read("src/components/ResourceSettingsPanel.css");
    const nameSelector = ':where([data-component-owner="resource-settings-panel"]) .resource-settings-name-row .secondary-button';
    const descriptionSelector = ':where([data-component-owner="resource-settings-panel"]) .resource-settings-desc-row .secondary-button';
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);

    for (const selector of [nameSelector, descriptionSelector]) {
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThan(mobileStart);
      const rule = css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
      expect(rule).toContain("min-height: 44px;");
    }
  });

  it("keeps the Workspace Generation lifecycle Save action at a mobile touch size", () => {
    const css = read("src/components/ResourceSettingsPanel.css");
    const selector = ':where([data-component-owner="resource-settings-panel"]) .resource-settings-policy-controls .secondary-button';
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    const start = css.indexOf(selector);
    expect(mobileStart).toBeGreaterThanOrEqual(0);
    expect(start, selector).toBeGreaterThan(mobileStart);

    const rule = css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    expect(rule).toContain("min-height: 44px;");

    for (const fieldSelector of [
      ':where([data-component-owner="resource-settings-panel"]) .resource-settings-policy-controls label',
      ':where([data-component-owner="resource-settings-panel"]) .resource-settings-policy-controls input[type="number"]',
    ]) {
      const fieldStart = css.indexOf(fieldSelector);
      expect(fieldStart, fieldSelector).toBeGreaterThan(mobileStart);
      const fieldRule = css.slice(css.indexOf("{", fieldStart), css.indexOf("}", fieldStart) + 1);
      expect(fieldRule).toContain("min-height: 44px;");
    }
  });

  it("keeps Scheduler Settings controls at a mobile touch size", () => {
    const css = read("src/components/ResourceSettingsPanel.css");
    const mobileStart = css.indexOf("@media (max-width: 980px)");
    expect(mobileStart).toBeGreaterThanOrEqual(0);

    const selectors = [
      ':where([data-component-owner="resource-settings-panel"]) .resource-settings-scheduler-agent .agent-binding-button',
    ];
    for (const selector of selectors) {
      const start = css.indexOf(selector, mobileStart);
      expect(start, selector).toBeGreaterThan(mobileStart);
      const rule = css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
      expect(rule).toContain("min-height: 44px;");
    }
  });

  it("wraps overlong chat message tokens without stranding trailing characters", () => {
    // overflow-wrap:anywhere breaks a token at the exact overflow point and
    // can leave a single trailing character on its own line; break-word only
    // splits tokens that cannot fit on a line of their own.
    const body = (selector: string) => {
      const css = read("src/components/TimelineMessage.css");
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };
    expect(body('.agent-message-bubble>p')).toContain("overflow-wrap: break-word;");
    expect(body(':where([data-component-owner="event-timeline"]) .agent-message-content {')).toContain("overflow-wrap: break-word;");
  });

  it("keeps chat surfaces on the chat region tokens", () => {
    const body = (path: string, selector: string) => {
      const css = read(path);
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    // Message bubbles take their frame, rail and fill from the region
    // tokens so a theme can restyle the conversation column from its own
    // stylesheet without touching component CSS.
    const bubble = body("src/components/TimelineMessage.css", ':where([data-component-owner="event-timeline"]) .agent-message-bubble {');
    expect(bubble).toContain("var(--chat-bubble-border-width)");
    expect(bubble).toContain("var(--chat-rail-width)");
    expect(bubble).toContain("var(--chat-bubble-bg)");

    const userBubble = body("src/components/TimelineMessage.css", ':where([data-component-owner="event-timeline"]) .agent-message-row.user .agent-message-bubble {');
    expect(userBubble).toContain("var(--chat-user-bg)");
    expect(userBubble).toContain("var(--chat-role-user)");

    const composer = body("src/components/ChatComposer.css", ':where([data-component-owner="chat-composer"]) .chat-input {');
    expect(composer).toContain("var(--chat-composer-border-color)");
    expect(composer).toContain("var(--chat-composer-radius)");

    const approval = body("src/components/ApprovalCard.css", ':where([data-component-owner="event-timeline"]) .agent-event.approval {');
    expect(approval).toContain("var(--chat-event-rail-approval)");

    const notice = body("src/components/TimelineNotice.css", ':where([data-component-owner="event-timeline"]) .timeline-notice {');
    expect(notice).toContain("var(--chat-event-rail-width)");

    const toolOutput = body("src/components/ToolItem.css", ':where([data-component-owner="event-timeline"]) .agent-tool-item pre {');
    expect(toolOutput).toContain("var(--chat-output-bg)");
    expect(toolOutput).toContain("var(--chat-output-text)");
  });

  it("keeps sidebar text and status colors on sidebar region tokens", () => {
    const body = (path: string, selector: string) => {
      const css = read(path);
      const start = css.indexOf(selector);
      expect(start, selector).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };

    // The sidebar is light in the default theme: text must come from the
    // sidebar region tokens, never from line tokens (invisible on light
    // surfaces) or --panel (white).
    expect(body("src/components/AppShell.css", ':where([data-component-owner="app-shell"]) .brand-copy strong {')).toContain("color: var(--sidebar-text);");
    expect(body("src/components/AttentionList.css", ':where([data-component-owner="attention-list"]) .inbox-text {')).toContain("color: var(--sidebar-text);");
    expect(body("src/components/AttentionList.css", ':where([data-component-owner="attention-list"]) .inbox-row {')).toContain("color: var(--sidebar-text);");
    const schedulerSmall = body("src/components/SchedulerNav.css", '[data-component-owner="scheduler-nav"] small {');
    expect(schedulerSmall).toContain("color: var(--sidebar-muted);");

    // Tree state markers use the sidebar status accents so dark sidebars
    // can raise them without touching the light themes.
    const blocked = body("src/components/StatusPresentation.css", ':where([data-component-owner="status-presentation"]) .task-state-blocked {');
    expect(blocked).toContain("color: var(--sidebar-attention);");
    const waiting = body("src/components/StatusPresentation.css", ':where([data-component-owner="status-presentation"]) .task-state-waiting,');
    expect(waiting).toContain("color: var(--sidebar-attention-soft);");
  });

  it("marks nested component roots with the same owner used by their CSS", () => {
	    for (const component of ["AgentPanelHeader", "ActivityGroup", "AppearanceSettingsPanel", "ActivityPanel", "AgentHubSettingsPanel", "ApprovalCard", "DiffModal", "DoctorDialog", "FileBrowser", "FilePreviewModal", "HistoryTimeline", "LifecycleNotice", "MarkdownDocument", "MobileToolbar", "NotificationSettingsPanel", "PaneResizeHandle", "ProfilesSettingsPanel", "ProjectCreateForm", "ProjectTree", "SettingsNavigation", "StatusPresentation", "TaskWizard", "TemplateFieldGroup", "TemplatePicker", "ThinkingBlock", "TimelineMessage", "TimelineNotice", "ToolGroup", "ToolItem", "UnknownEvent", "WorkspaceSettingsPanel", "WorkspaceSwitcher"] as const) {
      expect(read(`src/components/${component}.svelte`)).toContain(`data-component-owner="${owners[component]}"`);
    }
  });

  it("keeps WorkspaceSwitcher controls below the component root boundary", () => {
    const css = read("src/components/WorkspaceSwitcher.css");
    expect(css).toContain(':where([data-component-owner="workspace-switcher"]) .workspace-switcher-menu-button');
    expect(css).not.toContain(':where([data-component-owner="workspace-switcher"]).workspace-switcher-menu-button');
    expect(css).toContain(':where([data-component-owner="workspace-switcher"]) .workspace-open');
    expect(css).not.toContain(':where([data-component-owner="workspace-switcher"]).workspace-open');
  });

  it("keeps the selected Activity row background while hovered", () => {
    const css = read("src/components/AttentionList.css");
    const body = (selector: string) => {
      const start = css.indexOf(selector);
      expect(start).toBeGreaterThanOrEqual(0);
      return css.slice(css.indexOf("{", start), css.indexOf("}", start) + 1);
    };
    const backgroundOf = (selector: string) => {
      const match = body(selector).match(/background:\s*([^;]+);/);
      expect(match, selector).not.toBeNull();
      return match![1].trim();
    };
    // Hovering the open resource must not wash out its selected state.
    expect(backgroundOf('.activity-row.selected:hover')).toBe(backgroundOf('.activity-row.selected'));
    // Non-selected rows keep the dedicated hover tint.
    expect(backgroundOf('.activity-row:hover')).toBe("var(--sidebar-row)");
  });
});
