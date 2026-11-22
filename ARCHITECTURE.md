# 这两个网关到底在「反代」什么

先把词说清楚：**反代 = 协议适配层**。客户端只会说 OpenAI / Anthropic 那套 HTTP+SSE；
上游产品只会说自己内部那套（鉴权、会话、回合、额度）。网关的工作是把两边的**动作**和**信息**逐项对上，
而不是「把请求转一手」。

```
客户端（OpenAI/Anthropic 形状）
      │  messages[] / tools[] / stream:true
      ▼
   ┌──────────── 网关 ────────────┐
   │ 请求映射 → 上游调用 → 事件流 │
   │ 响应映射 ← usage 归一 ← 错误分类 │
   └──────────────────────────────┘
      │  上游产品私有协议
      ▼
   上游（CodeBuddy / Capy）
```

## WorkBuddy2API 反代的是什么

**上游**：腾讯 CodeBuddy 的聊天后端（`internal/upstream`）。它本身已经是一条
**带 SSE 增量的 chat 接口**，只是形状是产品私有的：模型别名、会话 id、prompt 模式、
图片、工具调用、积分扣减、限流业务码（6000/6020/11102/11115/11158/14017…）。

**对外暴露**（`internal/server/handler.go` 路由表）：

| 面 | 端点 |
| --- | --- |
| OpenAI | `POST /v1/chat/completions`、`POST /v1/responses`、`GET /v1/responses`(426 回落)、`GET /v1/models`、`POST /v2/chat/busy` |
| Anthropic | `POST /v1/messages`、`POST /messages`、`POST /v1/messages/count_tokens` |
| 媒体 | `POST /v1/images/generations|edits`、`POST /v1/videos*`、`GET /v1/videos/{id}`、`POST /v1/3d/*` |
| 工具面 | `POST /mcp`（JSON-RPC）、`POST /v1/search|fetch|tools/search|tools/fetch` |
| 观测 | `GET /status`、`GET /v1/capacity`、`GET /healthz`、`GET /metrics`、`/panel/`、`POST /admin/tasks/{name}/run` |

**中间流过的信息**（这才是「反代」的真本事）：

1. **请求**：messages/tools/images → 上游 payload；模型别名解析；会话/提示词模式选择；压缩与头信息。
2. **响应**：上游 SSE → 客户端协议的 SSE/JSON；`tool_calls` 按 index 合并；截断（没有 `[DONE]`）标成 `finish_reason="length"`。
3. **用量**：`prompt_tokens / completion_tokens / cached_tokens`（多种别名归一）、积分扣减、缓存命中率。
4. **失败语义**：错误分类驱动账号冷却——硬额度(402)、软限流(429)、会话失效(401)、模型无权(11158)、WAF(403)、prompt 超长(11115)…；「首帧否决」= 还没给客户端写字节就失败，可以换号重试。
5. **流式计时**：TTFB、tokens/s，全部由 SSE 增量直接测出来。

## Capy2API 反代的是什么（我们的上游完全不同）

**上游**：Capy 网页端的 **agent 线程 API**——不是 chat 接口：

| 动作 | 上游调用 |
| --- | --- |
| 建一次对话 | `POST /app/orgs/{org}/jams`（设备/云端）或 `POST /app/orgs/{org}/jams/{jam}/side-threads`（复用于线程） |
| 发用户消息 | `POST /app/orgs/{org}/jams/{jam}/message` |
| 拿结果 | `GET /app/orgs/{org}/jams/{jam}/events`（只在 `assistant committed` 时给完整块） |
| 拿增量 | `wss://realtime-production.cloud.capy.ai/realtime`（`llm-event`：text-delta / tool-input-delta / step-finish） |
| 收拾 | `POST /app/orgs/{org}/jams/delete` |

所以这里的「反代」是**把 agent 的回合翻译成 chat completion**：

| 客户端概念 | Capy 侧对应物 | 我们的处理 |
| --- | --- | --- |
| `messages[]` | 一条 `message` 文本 | 折成 transcript / 工具协议 JSON 塞进首条消息 |
| `tools[]` + `tool_calls` | 上游没有 OpenAI 工具通道 | 文本协议 `capy2api.tools.v1`，模型只输出决策，客户端执行 |
| `stream:true` | committed 才有正文 | 等待期 keepalive，落地后分片吐字；增量只用于 TTFT/测速 |
| `usage` | 事件的 `usage` + `contextTokens` | 归一成 OpenAI/Anthropic 字段（含 cached_tokens） |
| 上下文/缓存 | `contextTokens{system,tools,mcp,...}` | 透出 `capy.context_tokens`，解释 43k 输入从哪来 |
| 账号/额度 | Clerk 会话 + 组织 billing | 账号池、冷却、402 `insufficient_credits` |
| 并发 | 每请求一条子线程 | 全局并发上限 + 线程用完即删 |

## 两边差在哪（决定还需要补什么）

| 能力 | WorkBuddy | Capy（我们） | 状态 |
| --- | --- | --- | --- |
| 上游原生增量 | 有（SSE） | 有（realtime WS，需订阅） | 已接：TTFT / gen / tokens-per-second |
| 工具调用 | 上游原生 `tool_calls` | 无，文本协议模拟 | 已实现（严格校验，客户端执行） |
| 多轮上下文复用 | 会话键 → 粘性账号 + prompt_cache_key | 会话键 → 复用同一条上游线程（只发增量） | **已支持**（见 README「对话与缓存」） |
| 图片输入 | 支持 | 附件上传（`/app/orgs/{org}/files` → PUT → `message.files`），模型直接看见 | **已支持**（三种协议） |
| `count_tokens` | 本地估算 | —— | **本次补齐** |
| `GET /v1/responses` 426 | 有 | —— | **本次补齐** |
| `/status` `/v1/capacity` `/v2/chat/busy` | 有 | —— | **本次补齐** |
| Anthropic usage 的 cache 字段 | 有 | 只有 input/output | **本次补齐** |
| 首字直接下发给客户端 | 有（边收边转） | 只用于测速 | 未做（需处理增量与最终文本对账） |
| 截断语义 | 无 `[DONE]` → `length` | 超时即报错 | 部分（超时是明错，不会静默截断） |
| 媒体/搜索/MCP | 有 | 无 | 不做（上游没有对应能力） |

一句话：**WorkBuddy 是在「同一个 chat 协议的两个方言」之间翻译；我们是在「agent 回合协议」和 chat 协议之间翻译**——
所以缺的从来不是「转发」，而是把回合里的信息（增量、usage、缓存、工具、错误）逐项对上。
