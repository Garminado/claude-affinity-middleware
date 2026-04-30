# Claude 渠道亲和性中间件

> 让 new-api 把同一会话的连续多轮 Claude `/v1/messages` 请求稳定路由到同一个上游渠道。

## 一、它解决什么问题

new-api 在不做特殊处理时,会把同一客户的连续两次 `/v1/messages` 请求按权重/随机分配到不同的 Claude 上游渠道。这会触发以下 Claude/Bedrock 严重错误,以及 prompt cache 大面积失效:

| 跨渠道行为 | 上游表现 |
|------|------|
| `thinking.signature` 跨渠道 | **400** `Invalid signature in thinking block`(签名按 provider 加密,跨渠道无效) |
| `tool_use.id` ↔ `tool_result.tool_use_id` 跨渠道 | **400** `unexpected tool_use_id in tool_result` |
| `cache_control` 跨渠道 | 不会报错,但 prompt cache 命中率几乎归零 |

new-api 内置的"渠道亲和性"机制只支持三种 key 来源:`gjson`、`context_int`、`context_string`,其中没有"对 body 做 hash 后取 hex"的能力,光从 body 取单字段(如 `model`)做亲和键粒度太粗 —— 会把同一 model 的所有用户塌到同一个渠道。

本中间件填上的就是这个缺口:**解析 body → 判断是否需要亲和绑定 → 算出稳定 hash → 写入 gin Context 一个固定 key**。后续 new-api 自带的渠道选择逻辑用 `context_string` 来源读这个 key,自动按一致性哈希落桶到同一渠道。

## 二、工作原理

```
POST /v1/messages
        │
        ▼
┌─────────────────────────────┐
│ 1. 路径过滤                  │  非 POST 或非 /messages → 直接放行
│    (HasSuffix "/messages")  │
└─────────────────────────────┘
        │
        ▼
┌─────────────────────────────┐
│ 2. 取 body 字节              │  common.GetBodyStorage(c)
│    (走 new-api 的 BodyStorage,│  下游 UnmarshalBodyReusable 自动复用
│     不消耗 Reader)            │
└─────────────────────────────┘
        │
        ▼
┌─────────────────────────────┐
│ 3. Inspect 触发判断          │  thinking / tool / cache_control
│    + 字段规范化              │  system/tools/first_user
│    + inner SHA256            │
└─────────────────────────────┘
        │
        ▼ 命中(否则直接 c.Next() 走原有随机/权重)
┌─────────────────────────────┐
│ 4. token_id salt             │  finalKey = SHA256(secret=token_id ‖
│    再做一次 SHA256            │                    inner=innerKey)
└─────────────────────────────┘
        │
        ▼
┌─────────────────────────────┐
│ 5. c.Set("claude_affinity_   │  下一中间件 Distribute 通过
│           key", finalKey)    │  context_string 读取此 key 做亲和路由
└─────────────────────────────┘
```

### 触发条件

| 档位 | 触发(任一即命中) | 跨渠道后果 | hash 输入 |
|------|------|------|------|
| **strict** | 顶层 `thinking.type` 启用 / 顶层 `tools[]` 非空 / `messages` 内出现 `thinking`、`tool_use`、`tool_result`、`cache_control` 块 | 400 错误 | tier + model + system + tools + first_user |
| **loose** | 仅 `system` 或 `tools` 顶层带 `cache_control`(且无任何 strict 触发) | 仅 cache miss | tier + model + system + tools(**不含 first_user**,使多用户共享同一前缀塌缩到同一渠道复用 cache) |
| **none** | 普通 chat,无任何触发 | 无 | 不写 context key,走原本权重路由 |

完整决策流程和字段规则见 [AFFINITY.md](AFFINITY.md)。

### Salt 二次 hash

inner hash 只与请求 body 内容相关。如果两个用户发完全相同的 body,他们的 inner hash 也相同,会被路由到同一渠道 —— 这破坏了用户隔离。

中间件读取 `TokenAuth` 中间件已写入 gin Context 的 `token_id`(见 `middleware/auth.go` 中 `c.Set("token_id", token.Id)`),作为 secret 对 inner hash 再做一次 SHA256:

```
finalKey = SHA256("secret" ‖ \x00 ‖ token_id_str ‖ \x01 ‖ "inner" ‖ \x00 ‖ innerKey ‖ \x01)
```

这样:
- 同一 token 多轮请求 → finalKey 相同 → 同一渠道(达成会话亲和)
- 不同 token 相同 body → finalKey 不同 → 路由分散(达成 token 隔离)
- token_id 是 int,不会泄漏 API key 明文到日志

## 三、接入方式(在你的 new-api fork 中启用)

### 步骤 1:复制中间件文件

把 `middleware/claude_affinity.go` 复制到你的 new-api 项目的 `middleware/` 目录下。**不需要修改任何字段**,文件是自包含的(除依赖项目自身的 `common`、`logger` 包外,只用 `gjson` 和标准库)。

### 步骤 2:在路由注册中间件

打开 `router/relay-router.go`,找到 `httpRouter` 这个 group 的定义(在 `relayV1Router.Group("")` 之后),在 `httpRouter.Use(middleware.Distribute())` **之前**插入一行:

```go
{
    //http router
    httpRouter := relayV1Router.Group("")
    httpRouter.Use(middleware.ClaudeAffinityHash())  // 新增:必须先于 Distribute
    httpRouter.Use(middleware.Distribute())

    // claude related routes
    httpRouter.POST("/messages", func(c *gin.Context) {
        controller.Relay(c, types.RelayFormatClaude)
    })
    // ... 其他路由保持原样
}
```

**为什么必须在 `Distribute()` 之前**:gin 的中间件链顺序是先注册先执行。Distribute 是渠道选择中间件,会读 context_string 做亲和路由。如果中间件在 Distribute 之后才执行,Distribute 那时 context 里还没有 key,会退化成随机选渠道,整个改造失效。

**为什么挂在 group 级别而不是 `/messages` 路由级别**:gin 中 group middleware 严格先于 route middleware,如果挂在路由级别会更晚执行 —— 错过 Distribute 的读取窗口。group 级 + 中间件内部 path 过滤,是唯一正确组合。

### 步骤 3:在 new-api 后台配置渠道亲和性规则

后台 → "渠道亲和性" → 新建规则:

| 字段 | 取值 |
|------|------|
| 来源类型 | `context_string` |
| Key | `claude_affinity_key` |
| 作用模型 | 所有 Claude 模型(claude-3-5-sonnet / claude-3-7-sonnet / claude-opus-4 等) |
| 作用分组 | 按需配置 |

Key 字符串必须**严格等于** `claude_affinity_key`(中间件内部硬编码)。

### 步骤 4:重新编译启动

```bash
go build ./...
./new-api --log-dir /var/log/new-api  # 启用文件日志,便于运维查证
```

## 四、验证

### 编译验证

```bash
go build ./middleware/... ./router/...
go vet ./middleware/... ./router/...
```

### 命中验证

```bash
# 触发 strict tier(thinking 启用)
curl -X POST http://localhost:3000/v1/messages \
  -H "Authorization: Bearer <你的 sk-xxx>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [{"role": "user", "content": "Hi"}]
  }'
```

预期日志(同时输出到 stdout 和 `<log-dir>/oneapi-<ts>.log`):

```text
[INFO] 2026/04/30 - 14:23:12 | abc-req-id | [claude_affinity] hit tier=strict triggers=[thinking] key=4a8f9c1d2e3b6071 salted=true token_id=42 model=claude-3-7-sonnet-20250219 body_bytes=4827 elapsed=14.2µs
```

连续发 3 次相同请求,所有 `key=` 字段应完全相同,且 new-api 选中的 channel_id 也应保持一致。

### 未触发验证

```bash
# 普通 chat,不带 thinking/tools/cache_control
curl -X POST http://localhost:3000/v1/messages \
  -d '{"model":"claude-3-5-sonnet-20241022","max_tokens":100,
       "messages":[{"role":"user","content":"hello"}]}'
```

预期:**没有** `[claude_affinity] hit` 日志,Distribute 走原有权重/随机选择。

### OpenAI 格式不受影响验证

```bash
curl -X POST http://localhost:3000/v1/chat/completions \
  -d '{"model":"gpt-4","messages":[...]}'
```

预期:**没有** `[claude_affinity]` 日志(中间件因 path 不以 `/messages` 结尾立即放行,不读 body)。

## 五、配置常量速查

中间件不暴露任何外部配置,所有参数硬编码在 `middleware/claude_affinity.go` 文件顶部:

| 常量 | 值 | 含义 |
|------|------|------|
| `claudeAffinityContextKey` | `"claude_affinity_key"` | 写入 gin Context 的 key,后台配置必须填同样字符串 |
| `claudeMessagesPathSuffix` | `"/messages"` | 路径过滤后缀 |
| `claudeAffinityLogPrefix` | `"[claude_affinity]"` | 日志前缀 |
| `claudeAffinityKeyLogLen` | `16` | 日志中 key 打印的最大字符数(SHA256 hex 共 64 字符) |
| `tokenIdContextKey` | `"token_id"` | 用于 salt 的 secret 来源 key,对齐 TokenAuth 写入约定 |
| `defaultClaudeAffinityTrigger.OnThinking` | `true` | thinking 触发器开关 |
| `defaultClaudeAffinityTrigger.OnTool` | `true` | tool 触发器开关 |
| `defaultClaudeAffinityTrigger.OnCache` | `true` | cache_control 触发器开关 |

如需调整(例如临时禁用 cache 触发),改源码常量后重新编译即可。

## 六、强约束(本中间件的演进规则)

> **以下规则是本中间件的设计金线,任何修改前必须读完。违反会导致线上故障或破坏改造的最小侵入原则。**

### 约束 1:不引入任何当前项目尚未使用的依赖

中间件唯一的"非标准库"依赖是 `github.com/tidwall/gjson`,而该包已经被 new-api 多处使用(如 `service/channel_affinity.go`),所以不算新增依赖。

**禁止**引入 `lumberjack`、`zap`、`logrus`、`xxhash`、`fnv`、`murmur` 等任何 new-api 主项目尚未使用的库。日志一律走 `logger.LogInfo/LogWarn/LogError/LogDebug`(自动落地到 `gin.DefaultWriter`,而 `logger.SetupLogger()` 已把它替换为 `MultiWriter(stdout, <LogDir>/oneapi-<ts>.log)`)。

### 约束 2:不增加任何其他文件的改动

整个改造的代码侵入面是:

1. 新增 `middleware/claude_affinity.go`(本中间件)
2. 修改 `router/relay-router.go` 一行(注册中间件)

**这是上限。** 任何想"顺便清理一下"、"顺便重构一下"、"加个常量到 constant 包"、"加个工具到 common 包"的诱惑,都必须拒绝。如果未来有需要扩展功能,优先考虑:
- 先在中间件内部实现(本地常量、本地辅助函数)
- 严重无法回避时,**必须先与维护者确认 3 次以上才能扩散到其他文件**

### 约束 3:hash 计算逻辑不可修改

`Inspect` / `hasCacheControl` / `normalizedSystem` / `canonicalTools` / `firstUserText` / `computeKey` / `writeKV` 这 7 个函数是 hash 行为的权威实现,跨版本必须严格稳定。修改它们会让线上正在跑的会话 hash 漂移 → 同一会话被路由到不同渠道 → **重新触发本中间件本要解决的 400 错误**。

如果必须改(例如新增触发类型),改动前必须:
1. 同步更新 [AFFINITY.md](AFFINITY.md) 对应表格
2. 增加单元测试覆盖新行为
3. 评估对线上已有会话的兼容性(通常需要灰度 + 双 key 过渡)

### 约束 4:saltAffinityKey 的 secret 必须是 token_id

不要换成 `token_key`(sk-xxx 明文)、`user_id`、IP 等其他来源:

- `token_key` 是 API key 字符串,**进 hash 输入会经由日志/调试场景泄漏明文风险**
- `user_id` 是用户级,同一用户多个 token 会被塌到同一组渠道(失去 token 级隔离)
- IP 不稳定(用户切换网络就丢亲和)

`token_id` 是 int 类型的数据库主键,稳定、唯一、零信息泄漏,且 TokenAuth 在中间件链更早位置已经写入 gin Context(`c.Set("token_id", token.Id)`),零额外读取成本。

### 约束 5:中间件必须在 Distribute 之前注册

见"接入方式步骤 2"的解释。如果未来 new-api 引入新的渠道选择中间件,本中间件必须保证**先于所有读取 `claude_affinity_key` 的中间件**执行。

### 约束 6:任何错误都必须透传

中间件设计原则是"永不阻断业务请求"。无论是 body 过大、JSON 非法、panic、还是 token_id 拿不到,都必须 `c.Next()` 让请求走原本流程。绝不调用 `c.Abort()` 或返回 4xx/5xx。

### 约束 7:body 字节零修改

只通过 `common.GetBodyStorage(c)` 读字节;绝不修改 body、绝不替换 `c.Request.Body`。下游 controller 的 `UnmarshalBodyReusable` 会自动复用已缓冲的存储,中间件不参与 body 重写。

## 七、FAQ

**Q1:为什么不把整个 messages 数组都做 hash?**

多轮对话的 messages 每次都在变(追加新消息),整体 hash 不稳定 → 每轮都换渠道 → 立刻 400。中间件只取**会话期间永不变化的前缀**:`model + system + tools + first_user`。

**Q2:为什么 messages 内的 cache_control 判定为 strict 而不是 loose?**

如果按 loose 处理,系统/工具都为空时 hash 退化为 `(tier+model)` 近常量,所有此类请求会塌到同一个渠道,既毁负载均衡又拿不到 cache 收益(空前缀根本无东西可缓存)。messages 内部的 cache_control 必然包含会变的 message 历史,跨用户根本不可能复用,只有"同一会话多轮的增量缓存"才有意义 —— 这本来就是 strict 的语义。

**Q3:Claude Code CLI 已经会自动加 `metadata.user_id`,本中间件还有用吗?**

中间件检测到 `metadata.user_id` 时会**短路**(skip_reason=metadata_user_id),不写 context key,把亲和决策权交给上游 —— 上游应自行按 user_id 路由。这意味着:Claude Code CLI 用户的请求**不会**被本中间件干预,完全无副作用。

**Q4:渠道列表变化(增删)会导致亲和键失效吗?**

亲和键本身稳定不变。但 new-api 内部用一致性哈希把 key 落桶到具体渠道,渠道列表变化会让一部分 key 重新映射到新渠道。这是一致性哈希的固有行为,不是本中间件的问题。

**Q5:如何临时关闭这个中间件?**

最简单的方法:把 `router/relay-router.go` 里那一行 `httpRouter.Use(middleware.ClaudeAffinityHash())` 注释掉,重新编译。中间件不会有任何残留状态,关闭即生效。

**Q6:可以同时给 OpenAI 路由开亲和性吗?**

不能直接用本中间件 —— OpenAI 协议的 body 结构(`/v1/chat/completions`)与 Claude 不同,没有 `thinking`、`tool_use`、`cache_control` 这些字段,触发判断会全部 miss。要给 OpenAI 做亲和需要另写一个针对 OpenAI 协议的解析器。

## 八、相关文档

- [CLAUDE.md](./docs/claude-affinity-middleware/CLAUDE.md) — AI 助手项目上下文(改动前必读)
- [AFFINITY.md](./docs/claude-affinity-middleware/AFFINITY.md) — 触发条件与 hash 字段的完整穷举规约
- [TESTING.md](./docs/claude-affinity-middleware/TESTING.md) — REST API 测试用例集(覆盖所有决策分支 + 端到端验证)

## 九、其他
- [Linux.do](https://linux.do/)
