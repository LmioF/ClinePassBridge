# CPA v8.0.13 可选本地 429 退避补丁

普通用户可以直接使用 ClinePassBridge 插件，宿主补丁为可选项。官方 CPA 未实现这里的回调时，插件仍会对自己生成的本地 429 等待 5 秒，观察插件 deadline 和停止信号；客户端取消观察和 CPA 选号前的 429 延迟由本补丁提供。

## 适用版本与范围

- 官方 CPA `v8.0.13`，提交 `d7914afdedca7af95ee974a42453dc49fc1388ce`。补丁改动的既有源码与该提交逐文件 blob 核对一致。
- 限定唯一提供者 `cline-pass`。多提供者路由、其他提供者、普通上游错误、真实上游 429 均保持原返回时机。
- 仅延迟真正的本地 **429**：`modelCooldownError`，以及明确带有 429 状态的本地 `auth_not_found` / `auth_unavailable`。原本为 503 或未指定状态的“无可用凭据”保持原行为，遵循“仅本地 429”的范围。
- 利用 CPA 现有 `upstreamExecutionAttemptError` 标记排除真正执行过上游的错误，判断发生在原标记被移除之前。历史冷却错误附带的上游原因不代表本次请求已访问上游。
- 在执行的最终返回边界等一次 5 秒，此时选号和执行的局部资源已释放；流式调用尚未返回 headers/StreamResult。不会添加自动重试，也不会在选每个账号时重复等待。CPA 原有重试/冷却等待设置仍按原逻辑执行。
- 保留原错误对象、状态、正文与 `Retry-After`，不把 `Retry-After` 改成 5 秒。context 取消或 deadline 到达时及时结束等待。
- 补丁不依赖 Resin，也不涉及任何代理类型或出口配置。

## 保留现有 payload 定制

`local-429-delay.patch` 仅包含退避、调度回调和测试；不会修改 `internal/pluginhost/adapters_executors.go` 或 payload 规则文件。生成与编译用的独立副本已在基线中保留原有线上 payload 规则补丁。原参考目录没有原位修改。

只应用本文件即可兼容官方 CPA；已有 payload 定制的用户继续保留该定制。不要用旧版整目录或二进制覆盖当前宿主。

## 应用与验证

先在 CPA 的干净分支或独立 worktree 中操作，替换下面的源码路径：

```sh
CPA_CHECKOUT=/path/to/CLIProxyAPI
DELAY_PATCH=/path/to/ClinePassBridge/compat/cpa-v8.0.13/local-429-delay.patch
git -C "$CPA_CHECKOUT" apply --check "$DELAY_PATCH"
git -C "$CPA_CHECKOUT" apply "$DELAY_PATCH"
cd "$CPA_CHECKOUT"
go test ./sdk/cliproxy/auth ./internal/pluginhost -run '^(TestClineLocalDelay|TestHostSchedulingCapabilities|TestHostRequestContext)' -count=1
go build -o ./cli-proxy-api ./cmd/server
```

PowerShell 示例：

```powershell
$cpaCheckout = 'C:\src\CLIProxyAPI'
$delayPatch = 'C:\src\ClinePassBridge\compat\cpa-v8.0.13\local-429-delay.patch'
git -C $cpaCheckout apply --check $delayPatch
git -C $cpaCheckout apply $delayPatch
go -C $cpaCheckout test ./sdk/cliproxy/auth ./internal/pluginhost -run '^(TestClineLocalDelay|TestHostSchedulingCapabilities|TestHostRequestContext)' -count=1
go -C $cpaCheckout build -o (Join-Path $cpaCheckout 'cli-proxy-api.exe') ./cmd/server
```

测试用可注入 timer 推进 5 秒，覆盖作用域、真实上游 429、原 Retry-After、取消、deadline、执行/流式/计数最终边界，以及 callback 的插件/实例归属检查。调度能力和请求 context 测试使用 `decodeRPCEnvelope` 校验完整 ABI 信封，避免裸 JSON 在函数测试中通过、动态插件实际解码失败。Windows 纯 Go 宿主编译不代表 Linux 动态插件加载验证；部署前应按目标平台构建宿主和插件。

回滚源码：在仅包含本补丁改动的副本中执行 `git -C $cpaCheckout apply -R $delayPatch`。这是开发交付，不含推送、部署或生产重启。

## 可选回调契约

以下示例为完整 ABI 响应，宿主使用 `marshalRPCResult(...)` 生成 `ok/result` 信封；插件先检查 `ok`，再读取 `result`。直接返回裸 JSON 会使插件将 callback 判定为失败，能力标志回落为 false，取消观察也随之降级。

`host.scheduling.capabilities`，入参 `{}`，成功返回：

```json
{"ok":true,"result":{"local_delay_supported":true,"request_context_supported":true,"local_delay_ms":5000}}
```

`local_delay_supported` 表示 CPA 选号前本地 429 退避补丁已具备。插件 `(*Service).LocalBackoffCapabilities()` 会在 250ms 内返回探测结果；宿主未实现、报错、超时或插件停止时，宿主能力默认 false。探测失败不阻断业务。UI 将 `HostSelectionDelay` 映射到 `host.local_delay_supported`，将 `RequestCancelObservable` 用于显示取消观察能力。

`host.request.context`，入参 `{"host_callback_id":"当前请求的回调ID"}`，成功返回：

```json
{"ok":true,"result":{"supported":true,"state":"active","deadline":"2026-10-12T10:00:00Z"}}
```

`deadline` 无宿主 deadline 时省略；`state` 为 `active` / `canceled` / `deadline_exceeded` / `closed`。宿主核对 callback 的插件和加载实例，不发网络请求，不占用 HTTP operation 或 stream。插件每 100ms 查询一次，至多保留一个进行中的查询。没有该回调的官方宿主仅支持插件 deadline/停止观察；UI 应明确展示这项差异。

## 插件整合契约

- 已在 `Service.Handle` 的 `executor.execute` / `executor.execute_stream` 最终错误出口调用 `s.applyLocal429Backoff(r, err)`。主代理合并后保留这一层，避免在 executor 的内部失败分支另加等待。
- 错误必须具有 `StatusCode() == 429`，并且具有 `Local429() bool` 返回 true；兼容现有 `upstreamError.Local == true`。quota、egress、team 新错误使用这两种契约之一，不按错误文案猜测本地来源。
- 不更改 `executor.go` / `upstream_error.go` / `types.go`。等待在执行器返回、lease 释放之后发生。
- `ExecutorRequest.deadline` 在 `Handle` 开始时捕获；宿主查询返回更早的 deadline 时采用更早值。等待遵守停止信号，并作为 active 工作参与 quiesce/shutdown。
- `LocalBackoffCapabilities()` 返回 `PluginDelayMS`（固定 5000）、`RequestCancelObservable` 和 `HostSelectionDelay`。调用点由主代理在管理/UI 响应中接入。
- 插件执行日志在 executor 返回时已生成，现有日志耗时不包含 RPC 出口的这次等待；延迟后的 HTTP 时机以客户端或定向测试为准。

## 本次验证

- 插件：`go test ./internal/bridge -run '^TestLocalBackoff' -count=1` 通过，覆盖本地 quota/egress/team 标记、真实上游错误不等、一次 5 秒、取消、deadline、停止、挂起 callback、官方宿主降级与流式/非流式 Handle 钩子。
- 宿主：上述两个纯 Go 包的定向测试通过；本地 Windows `go build ./cmd/server` 通过，编译副本包含既有 payload 规则定制。
- 官方 `v8.0.13` 干净副本的 `git apply --check`、实际应用及相同定向测试均通过；测试不依赖 payload 定制。

最初修订的 callback 测试只解码裸 JSON，遗漏了 ABI 信封校验。当前修订修复了三处成功返回，并让调度能力及 active/canceled/deadline_exceeded/closed 状态测试通过 `decodeRPCEnvelope` 验证响应；动态插件与宿主的完整加载 smoke 仍需在部署验收中执行。
