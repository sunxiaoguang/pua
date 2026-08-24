# PUA 内部应用 API

`internal/app` 是 CLI 与 `pua serve` 共用、且唯一实现 Workspace 文件系统业务规则的应用层。已有 root 的服务调用方使用 `OpenWorkspace`；需要从调用者提供的目录寻找所属 Workspace 的 CLI 适配器使用 `OpenWorkspaceFrom`：

```go
workspace, err := app.OpenWorkspace(root)
if err != nil {
	return err
}

project, err := workspace.CreateProjectWithInput(app.CreateProjectInput{
	Description: "Example",
	Slug: "example",
})
```

`Workspace` 保存规范化后的 root，可安全地在并发请求之间复用。API 不读取或修改进程 cwd；`OpenWorkspaceFrom` 的起点同样由调用方显式传入。应用层不启动 `pua` 子进程，也不向 stdout/stderr 写入协议或用户输出。返回值使用 `Project`、`Task`、`WorkspaceTree`、`ResourceDetailView` 等类型；失败时使用 `APIError`，可通过 `errors.As` 或 `app.IsKind` 检查操作类别。

主要入口：

- `OpenWorkspace`、`OpenWorkspaceFrom`、`Initialize`、`InitializeWithOptions`、`Migrate`：打开、发现、初始化或迁移 Workspace；
- `NormalizeProjectID`、`NormalizeTaskName`、`NormalizeTaskID`、`InferProjectID`、`InferTaskID`：集中实现 CLI 使用的资源选择规则；
- `Tree`、`Resource`、`Projects`、`Tasks`：读取 Workspace 资源视图；
- `EnsureScheduler`、`Scheduler`、`AddSchedule`、`UpdateSchedule`、`PauseSchedule`、`ResumeSchedule`、`RemoveSchedule`：创建、读取和原子修改 schema v2 Scheduler 定义；
- `Templates`、`Template`、`RenderTemplate`、`ValidateTemplateContent`、`CreateTemplate`、`MigrateTemplates`：模板发现、结构化校验、确定性渲染、脚手架和 V1 内容迁移；
- `PreviewTask`：无副作用地计算最终标题、Markdown、最终 Agent binding 与模板 digest；
- `CreateProject`、`CreateProjectWithInput`、`CreateTask`、`ArchiveResource`：资源生命周期。`ArchiveResource` 以可恢复目录移动为唯一提交点；Project 归档级联移动完整子树，Git 预检、开放子任务和移动后 worktree 修复问题通过 `ArchiveResult.Warnings` 返回，不会修改源码或阻断移动；
- `ResourceAgentBinding`、`SetResourceAgentBinding`、`SetResourceAgentDefaults`、`SetProjectTaskDefault`：读取或修改 Workspace、Project、Task 的 Agent/Profile binding，并保持新建 Task 的显式值、Project 默认值和 Workspace 默认值的继承顺序；
- `GenerationDiagnostics`、`GenerationDiagnostic`：从 `<control-dir>/runtime/resources/<resource-key>/` 的 resource-scoped generation store 派生只读 generation 诊断；不创建、修改或访问 AgentHub Session；
- `Repositories`、`CloneRepository` 及 Task repository 方法：仓库数据。资源对话历史由 `pua serve` 的 Resource History API 提供；旧资源的 `log.jsonl` 仅由 `pua migrate` 迁移为 `artifacts/legacy-log.md`。

跨进程写入使用 Workspace mutation lock。模板任务在同一 mutation lock 中重新读取并渲染；可选 digest 不匹配会在分配任务编号和创建 staging 目录前失败。CLI、HTTP handler 和 Web 界面只负责适配输入输出，不解析 YAML、替换占位符或自行读写资源 schema。

Workspace、Project 和 Task 的新建接口把 ID 分配、全部资源文件一起放在 mutation lock 的同一提交边界内。Project/Task 先写同文件系统 staging 目录，再原子 rename；Workspace 初始化使用 `.pua/initializing.json` 作为可恢复标记。旧 resource JSON 中的 creator/createdBy 字段只由一次性迁移清理，正常 API 不读取或写入这些字段。

Scheduler 是固定 ID `scheduler` 的 Workspace 特殊资源，工作目录为 `scheduler/`。初始化和迁移只创建缺失文件、校验冲突并刷新 `AGENTS.md` 的 PUA managed block，不覆盖 `scheduler.md`。`scheduler.json` 使用严格 schema v2 和格式化 JSON；所有修改与其他 Workspace 写入共享 mutation lock，并通过同目录临时文件、fsync、rename 和目录 fsync 原子提交。应用层校验 tagged-union trigger、最短间隔、六字段 cron、IANA 时区、开放目标和 revision CAS；它不保存运行游标，也不解释 guard。v1 迁移原样保留定义并标记 `needs_compilation`。
