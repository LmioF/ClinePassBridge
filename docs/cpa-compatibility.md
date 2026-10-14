# CPA 宿主兼容

ClinePassBridge 保持 JSON ABI v1，旧 CPA 仍可加载插件。默认开启的「重试兼容」可把指定 Fireworks/LiteLLM 响应截断返回为干净的 502，供原版 DSH 等客户端采用已有重试策略。

完整的请求级免冷却、流式空闲截流、网络回调分类和模型冷却范围显示，还需要宿主支持以下扩展：

| 能力 | 宿主扩展 |
| --- | --- |
| 输出前后的请求级错误 | RPC 错误分类、`host.stream.close.error_details` / `host.stream.emit.error_details` |
| 流式空闲截流 | `host.http.stream_close` 主动结束上游，宿主识别空闲超时的请求级 502 并免账号和模型冷却 |
| 网络故障、取消和截止时间 | `host.http.do_stream` / `host.http.stream_read` 的可选 `error_details` |
| 模型冷却范围 | `host.auth.list` 返回 `model_states_available` / `model_states` |

升级插件不会自动升级 CPA。旧宿主缺少这些扩展时，账号冷却仍取决于旧策略；日志中的 `legacy_host_unknown` 表示宿主未确认结构化流错误支持。

## 配套补丁

Release 提供 `clinepassbridge_0.3.2_cpa_compatibility.zip`，包含本文和 `cpa-v8.0.13-stream-errors.patch`。仓库中的补丁位于 `compatibility/`。

「流式卡住时提前截流」从 v0.3.2 起提供。空闲超时免冷却需要本版配套宿主源码补丁；v0.3.1 兼容包尚未识别该错误类别。

补丁基于官方 CLIProxyAPI **v8.0.13**，准确提交为 `d7914afdedca7af95ee974a42453dc49fc1388ce`，包含所需宿主源码和回归测试。它不包含本地独立的 payload 定制。其他 CPA 版本需先检查补丁适用性，本次未验证其他版本。

在单独的源码目录应用并构建（Go 1.26，Linux 动态库需 CGO / GCC）：

```bash
git clone https://github.com/router-for-me/CLIProxyAPI.git
cd CLIProxyAPI
git checkout d7914afdedca7af95ee974a42453dc49fc1388ce
# 将下载包解压后的 patch 放在此目录，再执行：
git apply --check cpa-v8.0.13-stream-errors.patch
git apply cpa-v8.0.13-stream-errors.patch
go test ./internal/pluginhost ./sdk/pluginapi
CGO_ENABLED=1 go build -trimpath -o CLIProxyAPI ./cmd/server
```

将构建后的 CPA 与对应版本的插件一起升级，沿用已有配置和持久化目录。部署前备份 CPA 二进制、插件动态库与数据；回滚时恢复对应宿主和插件。已有独立定制应在自己的源码分支保留并处理补丁冲突。

## 行为边界

- 「流式卡住时提前截流」默认开启，首段有效输出后连续空闲 10 秒即结束上游；思考、正文和工具增量重置计时，heartbeat、role 和空帧不重置。它对所有上游 SSE 请求生效（含非流式聚合），原生非流式不适用，不缩短首字等待，也不预知 LiteLLM 失败。
- 空闲超时以 502 错误终止并免账号和模型冷却。下游响应前可返回 HTTP 502；SSE 已开始时 HTTP 200 保持，仅发送错误帧结束流，不补正常完成帧。完整设置见 [进阶配置](configuration.md#流式卡住时提前截流)。
- 指定 Fireworks/LiteLLM 响应截断、本地取消/截止时间、已确认的网络故障默认免账号和模型冷却。真实 HTTP 限流、认证和额度错误保持各自处理。
- 上游已有错误 HTTP 状态时，即使读取错误正文时断网，也保留该状态。旧回调只有文字时不会推断网络豁免。
- 明确配置的「停止并冷却」「继续并冷却」规则仍优先。
- 重试由客户端执行，插件结束失败请求，不重放已开始的输出，不把失败流伪装成成功。
