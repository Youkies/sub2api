# Claude 修改审查记录

审查时间：2026-06-11

审查范围：

- 基线：`e34ad2b1`
- 当前：`HEAD` (`a2144c04`)
- 变更文件：
  - `backend/internal/pkg/antigravity/request_transformer.go`
  - `backend/internal/service/antigravity_credits_overages.go`
  - `backend/internal/service/antigravity_gateway_service.go`
- 未跟踪文件：
  - `.ai_memory/0_archive_context.md`
  - `.ai_memory/1_project_context.md`
  - `.ai_memory/2_active_task.md`
  - `.ai_memory/3_work_log.md`

## 结论

当前修改不建议直接合并或继续推送。主要问题是 `service` 单元测试失败并出现 panic，且新的 `ambiguous_transient` 分支可能把上游瞬时压力误标记为 AI Credits 耗尽。

## Findings

### P1: service 单元测试失败并出现 panic

命令：

```powershell
cd backend
go test -tags unit ./internal/service
```

结果：失败。

关键现象：

- `TestAntigravityRetryLoop_NoURLFallback_UsesConfiguredBaseURL` 收到 `account 1 model claude-sonnet-4-5 rate limited, need switch`。
- `TestHandleUpstreamError_429_ModelRateLimit` 期望 `SwitchError`，实际 `nil`。
- `TestShouldTriggerAntigravitySmartRetry` 中 7s / 15s 旧阈值语义与新的 60s 阈值冲突。
- `TestHandleSmartRetry_503_LongDelay_NoSingleAccountRetry_StillSwitches` 进入 smart retry 后调用 nil `httpUpstream`，在 `backend/internal/service/antigravity_gateway_service.go:316` panic。

判断：

- 阈值从 7s 改到 60s 可能是有意行为，但测试没有同步更新。
- panic 不是单纯断言变更，说明测试路径或实现缺少 nil/stub 防护。
- 新阈值还让部分 unit test 出现 39s 真实等待，测试会明显变慢。

建议：

- 明确 60s 是否是最终产品语义。
- 更新受影响测试用例中的 delay 和期望。
- 为 `handleSmartRetry` 的测试补齐 `httpUpstream` stub，或让相关分支在缺失 upstream 时返回可控错误。
- 避免 unit test 真实 sleep 39s，可注入 clock/sleeper 或改用短 delay 场景。

### P1: ambiguous_transient 可能误标记 AI Credits 耗尽

相关位置：

- `backend/internal/service/antigravity_gateway_service.go:209`
- `backend/internal/service/antigravity_credits_overages.go:58`
- `backend/internal/service/antigravity_credits_overages.go:207`

问题：

`Resource has been exhausted` 被归类为 `antigravity429AmbiguousTransient` 后，会允许尝试 credits retry。若 credits retry 仍返回同一句 `resource has been exhausted`，`shouldMarkCreditsExhausted` 会把 `AICredits` 写入 2h 冷却。

这会把“上游/节点瞬时压力”误判成“积分真的耗尽”，导致后续本可用的 credits 路径被跳过。

建议：

- 不要仅凭裸 `Resource has been exhausted` 标记 credits exhausted。
- 只在明确 credits 关键词或结构化 credits 错误时写入 `AICredits`。
- 给 ambiguous credits retry 增加独立测试：第一次裸 RESOURCE_EXHAUSTED，credits retry 仍裸 RESOURCE_EXHAUSTED，不应写 `AICredits`。

### P2: ambiguous_transient 写入 requestedModel，可能绕过最终模型限流

相关位置：

- `backend/internal/service/antigravity_gateway_service.go:430`

问题：

`ambiguous_transient` 分支用 `p.requestedModel` 写模型限流。但 Antigravity 调度和预检查通常依赖最终映射模型，例如请求 `claude-opus-4-6` 可能映射到 `claude-sonnet-4-5`。

如果写入请求模型而不是最终模型，后续调度可能无法命中刚写入的冷却 key。

建议：

- 使用 `resolveFinalAntigravityModelKey(p.ctx, p.account, p.requestedModel)` 获取最终 key。
- 若解析失败，再 fallback 到 `resolveAntigravityModelKey(p.requestedModel)`。
- `SwitchError.RateLimitedModel` 也应考虑返回实际写入的 key，避免上层和日志含义不一致。

### P2/P3: TransformOptions 变成死配置

相关位置：

- `backend/internal/pkg/antigravity/request_transformer.go:47`
- `backend/internal/pkg/antigravity/request_transformer.go:53`
- `backend/internal/pkg/antigravity/request_transformer.go:249`
- `backend/internal/pkg/antigravity/request_transformer.go:282`

问题：

`buildSystemInstruction` 现在把 `TransformOptions` 和 tools 都丢弃了，导致 `EnableIdentityPatch`、`IdentityPatch`、`EnableMCPXML`、`mcpXMLProtocol`、`hasMCPTools` 在 Claude -> Gemini 转换链路上不再生效。

如果纯透传是最终决定，这些配置和管理端开关应标记废弃或移除；否则当前行为会让用户以为开关仍有效。

建议：

- 决定是否彻底删除 identity/MCP XML 注入。
- 如果保留配置，需要恢复对应逻辑或至少让配置名、文档、UI 与真实行为一致。

### P3: .ai_memory 未忽略

当前 `git status --short -uall` 只有 `.ai_memory/` 四个未跟踪文件。`.gitignore` 没有忽略 `.ai_memory`。

建议：

- 如果这是本地工作记忆，不应提交，加入 `.gitignore`。
- 如果这是项目约定的一部分，需要明确是否纳入版本控制。

## 验证记录

通过：

```powershell
cd backend
go test ./internal/pkg/antigravity
```

失败：

```powershell
cd backend
go test -tags unit ./internal/service
```

额外检查：

```powershell
git diff --check e34ad2b1..HEAD
```

结果：通过，无 whitespace error。

## 建议修复顺序

1. 先修 `service` 单测 panic 和 60s threshold 对应测试语义。
2. 修 `ambiguous_transient` 与 `AICredits` 的误标记风险。
3. 修 ambiguous 分支写入 requested model key 的问题。
4. 决定 `TransformOptions` 的去留，并同步代码、测试、UI/文档。
5. 处理 `.ai_memory/` 是否忽略或提交。
