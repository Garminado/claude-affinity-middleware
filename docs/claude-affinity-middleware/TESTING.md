# 中间件功能测试用例

> 用 REST API 请求(curl)穷举测试中间件每条决策分支的实际行为。配合 [AFFINITY.md](AFFINITY.md) 的决策图阅读 —— 每个用例编号都直接对应决策图的代号(P0、E1、S0-S4、A1-A6、B1)。

## 一、准备

### 1.1 启动 new-api(开启文件日志)

```bash
./new-api --log-dir ./logs
```

`logger.SetupLogger()` 会把 `gin.DefaultWriter/DefaultErrorWriter` 包成 `MultiWriter(stdout, ./logs/oneapi-<timestamp>.log)`,所有 `[claude_affinity]` 日志会自动落盘。

### 1.2 导出环境变量

```bash
export BASE_URL='http://localhost:3000'
export TOKEN='sk-your-first-user-token'    # 用户 A 的 token
export TOKEN2='sk-your-second-user-token'  # 用户 B 的 token,用于 salt 隔离测试
export LOG_FILE='./logs/oneapi-*.log'
```

### 1.3 准备一个统一的日志观测命令

每个用例发完 curl 后,用以下命令查看本次请求产生的中间件日志:

```bash
# 查看最新一次中间件活动
tail -n 200 $LOG_FILE | grep '\[claude_affinity\]' | tail -5

# 查看所有命中
grep '\[claude_affinity\] hit' $LOG_FILE

# 查看所有跳过(需要先启用 DEBUG: 启动加 --debug 或环境变量 DEBUG_ENABLED=true)
grep '\[claude_affinity\] miss\|\[claude_affinity\] skip' $LOG_FILE
```

### 1.4 测试通过的统一标准

每个用例后看以下三处:

1. **日志命中标记**:`[claude_affinity] hit ...` / `miss ...` / `skip ...` 与"预期日志"匹配
2. **Context key**:命中用例下,后台日志中 Distribute 选中的 channel_id 在多次相同请求间应保持一致
3. **HTTP 响应**:中间件不会改变响应体,200/4xx 由上游决定;关注的是日志和路由行为

---

## 二、路径过滤(P0)

### 用例 P0-1:非 POST 方法不进入解析

**目的**:验证中间件对 GET / OPTIONS / DELETE 等非 POST 方法直接放行,不读 body 不算 hash。

```bash
curl -X GET "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN"
```

**预期日志**(开启 DEBUG 后可见):
```
[DEBUG] ... [claude_affinity] skip path=/v1/messages method=GET reason=not_messages_endpoint
```

**预期行为**:中间件 1 行 path 比较后立即 `c.Next()`,不调用 `GetBodyStorage`。

---

### 用例 P0-2:非 /messages 路径不进入解析(OpenAI 格式)

**目的**:验证中间件挂在共享 group 上,但只对 `/messages` 后缀生效,不影响 OpenAI 协议路由。

```bash
curl -X POST "$BASE_URL/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**(开启 DEBUG 后可见):
```
[DEBUG] ... [claude_affinity] skip path=/v1/chat/completions method=POST reason=not_messages_endpoint
```

**预期行为**:**没有** `[claude_affinity] hit` 或 `miss` 日志(连 body 都没读)。

---

### 用例 P0-3:其他不以 /messages 结尾的 Claude 风格路径

**目的**:验证 `/v1/responses`、`/v1/embeddings` 等 group 内其他路由也跳过中间件。

```bash
curl -X POST "$BASE_URL/v1/embeddings" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"text-embedding-3-small","input":"hello"}'
```

**预期日志**:同 P0-2,DEBUG 级 `skip ... reason=not_messages_endpoint`。

---

## 三、跳过场景(E1 / S0-S4)

> 这些用例都进入了中间件主体逻辑(POST /messages),但因各种原因不写入 `claude_affinity_key`。Distribute 后续读 context_string 拿到空字符串,自动回退到原本的随机/权重渠道选择策略。

### 用例 S0:非法 JSON

**目的**:验证 body 不是合法 JSON 时安全跳过,不阻断业务请求。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d 'this is not json {{ broken'
```

**预期日志**(DEBUG):
```
[DEBUG] ... [claude_affinity] miss reason=invalid_json model= body_bytes=26 elapsed=...
```

**预期行为**:`c.Next()` 透传给上游。上游会自己返回 4xx(不是中间件 4xx)。

---

### 用例 S1:metadata.user_id 短路(Claude Code CLI 行为)

**目的**:验证客户端已带用户标识时,中间件把亲和决策权交给上游,不写 context key。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "metadata": {"user_id": "alice@example.com"},
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**(DEBUG):
```
[DEBUG] ... [claude_affinity] miss reason=metadata_user_id model=claude-3-5-sonnet-20241022 body_bytes=... elapsed=...
```

**关键点**:即使 body 同时带了 `thinking`(本应触发 strict),`metadata.user_id` 优先级**最高**,直接短路。这是 Claude Code CLI 用户的预期行为。

---

### 用例 S1-边界:metadata.user_id 是空字符串也短路

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "metadata": {"user_id": ""},
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**:同 S1,`miss reason=metadata_user_id`。

**关键点**:`metadata.user_id` **存在即短路**,不区分空/非空。

---

### 用例 S1-边界:metadata 但无 user_id 字段不短路

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "metadata": {"trace_id": "xxx"},
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**:**命中** strict(因为 thinking 触发,且 metadata 里没有 user_id):
```
[INFO] ... [claude_affinity] hit tier=strict triggers=[thinking] key=... salted=true token_id=...
```

---

### 用例 S2:普通 chat,无任何触发

**目的**:验证最常见的"无 thinking / 无 tools / 无 cache_control"请求安全跳过。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "messages": [{"role":"user","content":"hello world"}]
  }'
```

**预期日志**(DEBUG):
```
[DEBUG] ... [claude_affinity] miss reason=no_trigger_matched model=claude-3-5-sonnet-20241022 body_bytes=... elapsed=...
```

**预期行为**:Distribute 走原本的权重/随机选择(连续多次发同样请求,channel_id 应当分散到多个渠道)。

---

### 用例 S3:strict 触发但首条 user 无 text(image-only)

**目的**:验证 strict 命中但第一条 user 消息没有任何文本(仅 image / tool_result)时跳过。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [{
      "role": "user",
      "content": [
        {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw..."}}
      ]
    }]
  }'
```

**预期日志**(DEBUG):
```
[DEBUG] ... [claude_affinity] miss reason=no_first_user_text model=claude-3-5-sonnet-20241022 body_bytes=... elapsed=...
```

**预期行为**:走原有权重路由。strict tier 强制要求 first_user 有可哈希的文本,否则降级跳过。

---

### 用例 S4:loose 触发但前缀全空

**目的**:验证仅 system/tools 顶层 cache_control 但规范化后两者都为空字符串时,防退化保护跳过。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "system": [{"type": "text", "text": "", "cache_control": {"type": "ephemeral"}}],
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**(DEBUG):
```
[DEBUG] ... [claude_affinity] miss reason=loose_skipped_empty_prefix model=claude-3-5-sonnet-20241022 body_bytes=... elapsed=...
```

**预期行为**:走原有权重路由。**关键不变量**:防止"hash 退化为常量,所有此类请求挤到同一个渠道"。

---

### 用例 E1a:body 过大(超过 128MiB)

**目的**:验证 body 超过 `constant.MaxRequestBodyMB` 时安全跳过,不阻断请求。

```bash
# 生成一个 130 MiB 的伪造 body(全是 'A' 字符,刻意撞 128MB 上限)
python3 -c "print('{\"model\":\"x\",\"messages\":[{\"role\":\"user\",\"content\":\"' + 'A'*(130*1024*1024) + '\"}]}')" > /tmp/big.json

curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/big.json
```

**预期日志**(WARN,生产也输出):
```
[WARN]  ... [claude_affinity] get_body_storage_failed path=/v1/messages content_length=136314880 err=request body exceeds 128 MB
```

**预期行为**:`c.Next()` 透传。中间件**绝不**返回 413(超限即透传是金线 6 的具体落地)。后续由 new-api 的其他逻辑或上游决定如何处理大 body。

---

## 四、Strict tier 命中(A1-A6)

> 这些用例下,`claude_affinity_key` 必被写入 context,且经过 token_id salt 后写入,日志含 `salted=true`。
> **同一会话**(message 第一条 user 不变、system/tools 不变)的连续多轮请求 → finalKey **完全一致** → 命中同一个 channel。

### 用例 A1:顶层 thinking 启用

**目的**:验证扩展思考请求被识别为 strict。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [{"role":"user","content":"What is 2+2?"}]
  }'
```

**预期日志**:
```
[INFO]  ... [claude_affinity] hit tier=strict triggers=[thinking] key=<16字符> salted=true token_id=<int> model=claude-3-7-sonnet-20250219 body_bytes=... elapsed=...
```

---

### 用例 A1-边界:thinking.type=disabled 不触发

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "disabled"},
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**:`miss reason=no_trigger_matched`。`disabled` 是 Claude 协议的关闭值,不应触发 strict。

---

### 用例 A2:顶层 tools[] 非空

**目的**:验证带工具定义的请求被识别为 strict。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 1024,
    "tools": [{
      "name": "get_weather",
      "description": "Get current weather",
      "input_schema": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
    }],
    "messages": [{"role":"user","content":"weather in Beijing"}]
  }'
```

**预期日志**:
```
[INFO]  ... [claude_affinity] hit tier=strict triggers=[tool] key=<16字符> salted=true ...
```

---

### 用例 A3:messages 内有 thinking 块(模拟第二轮)

**目的**:验证客户端把上一轮 assistant 的 thinking 块带回时被识别为 strict。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [
      {"role": "user", "content": "What is 2+2?"},
      {"role": "assistant", "content": [
        {"type": "thinking", "thinking": "Let me compute...", "signature": "abc123"},
        {"type": "text", "text": "4"}
      ]},
      {"role": "user", "content": "Now multiply by 3"}
    ]
  }'
```

**预期日志**:`hit tier=strict triggers=[thinking]`(thinking 在顶层 + messages 内都有,合并为单一 trigger)

**关键不变量**:这一轮的 finalKey 必须与 A1 用例的 finalKey **相同**(因为 `model + system + tools + first_user` 都没变,first_user 仍是 "What is 2+2?")。

---

### 用例 A4:messages 内有 tool_use / tool_result 块

**目的**:验证多轮工具调用的会话被识别为 strict。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 1024,
    "tools": [{
      "name": "get_weather",
      "description": "Get current weather",
      "input_schema": {"type":"object","properties":{"city":{"type":"string"}}}
    }],
    "messages": [
      {"role": "user", "content": "weather in Beijing"},
      {"role": "assistant", "content": [
        {"type": "tool_use", "id": "toolu_01ABC", "name": "get_weather", "input": {"city":"Beijing"}}
      ]},
      {"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": "toolu_01ABC", "content": "Sunny, 22°C"}
      ]}
    ]
  }'
```

**预期日志**:`hit tier=strict triggers=[tool]`

**关键不变量**:与 A2 用例(只有顶层 tools[])finalKey **应该相同**(model/tools/first_user 都不变)。这就是"同一会话多轮稳定"的核心保证。

---

### 用例 A5:messages 内 content 块带 cache_control

**目的**:验证 messages 内挂 cache_control 走 strict(**不是** loose,这是金线不变量)。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 1024,
    "messages": [{
      "role": "user",
      "content": [
        {"type": "text", "text": "Long context here...", "cache_control": {"type":"ephemeral"}}
      ]
    }]
  }'
```

**预期日志**:
```
[INFO]  ... [claude_affinity] hit tier=strict triggers=[cache] key=... salted=true ...
```

**关键不变量**:`tier=strict`,**绝不**是 `loose`。这是 [AFFINITY.md](AFFINITY.md) "关键不变量 5" 锁定的金线。

---

### 用例 A6:tools 数组某个 tool 带 cache_control

**目的**:验证 tools 上挂 cache_control 必然伴随 A2(tools 非空),整体走 strict。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 1024,
    "tools": [{
      "name": "get_weather",
      "description": "Get weather",
      "input_schema": {"type":"object"},
      "cache_control": {"type": "ephemeral"}
    }],
    "messages": [{"role":"user","content":"weather?"}]
  }'
```

**预期日志**:`hit tier=strict triggers=[tool cache]`(同时触发 A2 和 A6,两个 trigger)

**关键不变量**:此用例的 finalKey 应该与 **不带 cache_control 的相同 tools/system/first_user 请求**(等同于纯 A2 用例)产出**完全相同**的 finalKey —— 因为 `cache_control` 字段在 hash 计算中被剥掉,不进 inner hash。

---

## 五、Loose tier 命中(B1)

### 用例 B1:仅 system 顶层带 cache_control

**目的**:验证只有 system cache 时走 loose,使多用户共享同一 system 复用 cache。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 1024,
    "system": [
      {"type": "text", "text": "You are a helpful assistant.", "cache_control": {"type":"ephemeral"}}
    ],
    "messages": [{"role":"user","content":"hello"}]
  }'
```

**预期日志**:
```
[INFO]  ... [claude_affinity] hit tier=loose triggers=[cache] key=... salted=true token_id=... model=...
```

**关键不变量**:loose hash 输入**不含** first_user。如下用例验证此点。

---

### 用例 B1-跨用户文本:不同 first_user 的 loose 应得到相同 inner hash

**目的**:验证 loose tier 下,共享同一 model+system+tools 的不同会话 → 同一 inner hash → (经 token_id salt 后)同 token 落同一渠道。

**步骤**:用同一 TOKEN 发以下两个请求(注意 first_user 文本不同):

```bash
# 请求 1
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-sonnet-20241022","max_tokens":100,
       "system":[{"type":"text","text":"Helper.","cache_control":{"type":"ephemeral"}}],
       "messages":[{"role":"user","content":"question 1"}]}'

# 请求 2(只改 first_user)
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"model":"claude-3-5-sonnet-20241022","max_tokens":100,
       "system":[{"type":"text","text":"Helper.","cache_control":{"type":"ephemeral"}}],
       "messages":[{"role":"user","content":"question 2 totally different"}]}'
```

**预期日志**:两次请求的 `key=` 字段值**完全相同**(64 字符 SHA256 hex 一样,前 16 字符也一样)。

```bash
# 验证命令
grep '\[claude_affinity\] hit tier=loose' $LOG_FILE | tail -2 | awk '{for(i=1;i<=NF;i++) if($i~/^key=/) print $i}'
# 应输出两行相同的 key=xxxxxxxxxxxxxxxx
```

---

## 六、优先级与稳定性

### 用例 PRIO-1:strict 吸收 loose

**目的**:同时存在 A1(thinking) + B1(system cache) 时,最终 tier 必然是 strict。

```bash
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "system": [{"type":"text","text":"Helper","cache_control":{"type":"ephemeral"}}],
    "messages": [{"role":"user","content":"Hi"}]
  }'
```

**预期日志**:`hit tier=strict triggers=[thinking cache]`(`triggers` 列出全部命中的触发器,但 `tier` 必然是 strict)

---

### 用例 STAB-1:多轮 hash 完全相同(同一会话)

**目的**:验证同一会话连续 3 轮 / 5 轮请求的 finalKey **逐字节相同** —— 这是中间件存在的核心价值。

**步骤**:模拟 Claude Code CLI 的真实多轮对话(去掉 metadata.user_id 让中间件介入),保持 first_user 不变,assistant 回复 + 下一轮 user 追加:

```bash
# 第 1 轮:仅 user
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [{"role":"user","content":"What is 2+2?"}]
  }'

# 第 2 轮:user → assistant(thinking+text) → user
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [
      {"role":"user","content":"What is 2+2?"},
      {"role":"assistant","content":[
        {"type":"thinking","thinking":"compute","signature":"sig1"},
        {"type":"text","text":"4"}
      ]},
      {"role":"user","content":"now times 3"}
    ]
  }'

# 第 3 轮:再追加一轮
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-7-sonnet-20250219",
    "max_tokens": 1024,
    "thinking": {"type": "enabled", "budget_tokens": 1024},
    "messages": [
      {"role":"user","content":"What is 2+2?"},
      {"role":"assistant","content":[{"type":"thinking","thinking":"c1","signature":"s1"},{"type":"text","text":"4"}]},
      {"role":"user","content":"now times 3"},
      {"role":"assistant","content":[{"type":"thinking","thinking":"c2","signature":"s2"},{"type":"text","text":"12"}]},
      {"role":"user","content":"square it"}
    ]
  }'
```

**预期日志**:三次请求的 `key=` 字段**完全相同**(因为 model + system + tools + first_user="What is 2+2?" 始终不变)。

```bash
# 验证命令
grep '\[claude_affinity\] hit' $LOG_FILE | tail -3 | awk -F'key=' '{print $2}' | awk '{print $1}'
# 应输出三行相同的 16 字符 hex
```

**关键不变量**:这是中间件存在的根本目的 —— 同会话多轮 hash 稳定 → 同会话多轮路由到同一渠道 → thinking.signature / tool_use_id 跨轮有效。

---

### 用例 STAB-2:tools 顺序不影响 hash

**目的**:验证 `canonicalTools` 按 name 排序的不变性。

```bash
# 请求 A:tools 顺序 [get_weather, get_time]
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-5-sonnet-20241022","max_tokens":100,
    "tools":[
      {"name":"get_weather","description":"weather","input_schema":{"type":"object"}},
      {"name":"get_time","description":"time","input_schema":{"type":"object"}}
    ],
    "messages":[{"role":"user","content":"hi"}]
  }'

# 请求 B:tools 顺序反过来 [get_time, get_weather]
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-5-sonnet-20241022","max_tokens":100,
    "tools":[
      {"name":"get_time","description":"time","input_schema":{"type":"object"}},
      {"name":"get_weather","description":"weather","input_schema":{"type":"object"}}
    ],
    "messages":[{"role":"user","content":"hi"}]
  }'
```

**预期**:两次的 `key=` 字段**完全相同**。

---

### 用例 STAB-3:加/移除 cache_control 不改 hash

**目的**:验证 cache_control 字段被规范化剥掉,只作为触发标识不进 inner hash。

```bash
# 请求 A:tools 不带 cache_control
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-5-sonnet-20241022","max_tokens":100,
    "tools":[{"name":"t","description":"d","input_schema":{"type":"object"}}],
    "messages":[{"role":"user","content":"hi"}]
  }'

# 请求 B:tools 上加 cache_control
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-5-sonnet-20241022","max_tokens":100,
    "tools":[{"name":"t","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
    "messages":[{"role":"user","content":"hi"}]
  }'
```

**预期**:两次的 `key=` 字段**完全相同**(triggers 不同,A 是 `[tool]`,B 是 `[tool cache]`,但 hash 输入剥掉 cache_control 后等价)。

---

### 用例 STAB-4:不同 first_user 文本 → strict 必须分散

**目的**:验证 strict tier 对不同会话产出不同 hash(用户隔离 + 会话隔离)。

```bash
# 请求 A
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-7-sonnet-20250219","max_tokens":100,
    "thinking":{"type":"enabled","budget_tokens":1024},
    "messages":[{"role":"user","content":"question A"}]
  }'

# 请求 B(只改 first_user)
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-7-sonnet-20250219","max_tokens":100,
    "thinking":{"type":"enabled","budget_tokens":1024},
    "messages":[{"role":"user","content":"question B"}]
  }'
```

**预期**:两次的 `key=` 字段**完全不同**。

---

## 七、Salt 层验证(token 隔离)

### 用例 SALT-1:同 token + 同 body → finalKey 相同

**目的**:验证同一 token 多次相同请求的 finalKey 一致(基础亲和)。

```bash
# 同样的请求连发 3 次
for i in 1 2 3; do
  curl -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{
      "model":"claude-3-7-sonnet-20250219","max_tokens":100,
      "thinking":{"type":"enabled","budget_tokens":1024},
      "messages":[{"role":"user","content":"hi"}]
    }'
done
```

**预期**:三次日志的 `key=` 字段全相同,`token_id=<同一int>` 全相同,`salted=true`。

---

### 用例 SALT-2:不同 token + 同 body → finalKey 不同

**目的**:验证 token_id salt 真的实现了跨 token 隔离 —— 即使 body 完全一样,不同 token 的 finalKey 必须分散。

```bash
# 用户 A 发请求
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-7-sonnet-20250219","max_tokens":100,
    "thinking":{"type":"enabled","budget_tokens":1024},
    "messages":[{"role":"user","content":"identical body"}]
  }'

# 用户 B 发完全相同的 body(仅换 token)
curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN2" -H "Content-Type: application/json" \
  -d '{
    "model":"claude-3-7-sonnet-20250219","max_tokens":100,
    "thinking":{"type":"enabled","budget_tokens":1024},
    "messages":[{"role":"user","content":"identical body"}]
  }'
```

**预期**:
- 两次日志的 `token_id=` 不同(分别是用户 A 和 B 的 token id)
- 两次日志的 `key=` 字段**完全不同**(salt 起效)
- 两次都 `salted=true`

```bash
# 验证命令
grep '\[claude_affinity\] hit' $LOG_FILE | tail -2
# 应看到 token_id=42 ... key=abc...  和 token_id=99 ... key=xyz...
```

---

### 用例 SALT-3:salted=false 降级路径(未带 token / token_id=0)

**目的**:理论上 TokenAuth 不会让无效 token 走到中间件,这里仅为完整性。如果你的部署有 noauth 路径或 TokenAuth 配置异常,可以观察到此降级。

**预期日志**(若发生):
```
[INFO] ... [claude_affinity] hit tier=... triggers=... key=... salted=false token_id=0 model=...
```

`key` 此时是 inner hash 直接的值(SHA256 64 字符 hex)。

---

## 八、端到端渠道一致性验证

### 用例 E2E-1:同会话多轮命中同一 channel_id

**目的**:综合验证"中间件 → context_string → Distribute → 同一 channel"的完整链路。

**前提**:
- 你的 new-api 至少有 ≥2 个 Claude 渠道(channel_id 不同)
- 在后台已配置渠道亲和性规则(类型 `context_string`,Key `claude_affinity_key`,作用模型包含 `claude-3-7-sonnet-20250219`)

**步骤**:

```bash
# 连发 5 次相同的 strict 请求
for i in 1 2 3 4 5; do
  curl -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{
      "model":"claude-3-7-sonnet-20250219","max_tokens":50,
      "thinking":{"type":"enabled","budget_tokens":1024},
      "messages":[{"role":"user","content":"E2E test"}]
    }'
  sleep 1
done
```

**预期**:在 new-api 后台 → 日志 / 调用记录中,5 次请求的 channel_id 字段**完全相同**。

```bash
# 通过 new-api 自身的请求日志验证(grep request id 串联)
grep '<request-id>' $LOG_FILE | grep -E 'channel_id|claude_affinity'
```

---

### 用例 E2E-2:对照组 — 未命中请求会分散到多个渠道

**目的**:对比验证"S2 普通 chat 不命中"时确实会随机/权重分配。

```bash
# 连发 10 次普通 chat
for i in $(seq 1 10); do
  curl -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{
      "model":"claude-3-5-sonnet-20241022","max_tokens":50,
      "messages":[{"role":"user","content":"hello round '"$i"'"}]
    }'
  sleep 1
done
```

**预期**:在后台日志看到 10 次请求的 channel_id 分布在多个渠道(具体分布按你配置的权重),证明中间件**只**对触发条件命中的请求生效,不影响普通流量。

---

### 用例 E2E-3:不同 token + 同 body → 落不同渠道

**目的**:综合验证 SALT-2 的端到端效果。

```bash
# 用户 A 连发 5 次
for i in 1 2 3 4 5; do
  curl -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{
      "model":"claude-3-7-sonnet-20250219","max_tokens":50,
      "thinking":{"type":"enabled","budget_tokens":1024},
      "messages":[{"role":"user","content":"shared body"}]
    }'
done

# 用户 B 连发 5 次相同 body
for i in 1 2 3 4 5; do
  curl -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN2" -H "Content-Type: application/json" \
    -d '{
      "model":"claude-3-7-sonnet-20250219","max_tokens":50,
      "thinking":{"type":"enabled","budget_tokens":1024},
      "messages":[{"role":"user","content":"shared body"}]
    }'
done
```

**预期**:
- 用户 A 的 5 次落同一个 channel_id(假设 channel_3)
- 用户 B 的 5 次落另一个 channel_id(假设 channel_7),与用户 A 不同
- 不同 token 在不同渠道并行运行,不会互相挤占同一渠道

---

## 九、压力 / 边界场景

### 用例 STRESS-1:body 接近 128MiB 上限不阻断

**目的**:验证 body 接近但未超上限时正常计算 hash。

```bash
# 生成一个 ~120MiB 的 body
python3 -c "import json; print(json.dumps({'model':'claude-3-5-sonnet-20241022','max_tokens':100,'thinking':{'type':'enabled','budget_tokens':1024},'messages':[{'role':'user','content':'A'*(120*1024*1024)}]}))" > /tmp/big120.json

curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  --data-binary @/tmp/big120.json
```

**预期日志**:命中 strict(thinking 触发),`body_bytes=~125829120`,`elapsed=` 几十毫秒(SHA256 计算大 body 需时间)。

---

### 用例 STRESS-2:超长 first_user 文本

**目的**:验证 first_user 几兆文本也能稳定 hash。

```bash
python3 -c "import json; print(json.dumps({'model':'claude-3-5-sonnet-20241022','max_tokens':100,'thinking':{'type':'enabled','budget_tokens':1024},'messages':[{'role':'user','content':'A'*(2*1024*1024)}]}))" > /tmp/big2m.json

curl -X POST "$BASE_URL/v1/messages" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  --data-binary @/tmp/big2m.json
```

**预期**:命中 strict,`elapsed` ~10-30ms(2MiB SHA256)。两次相同请求的 `key=` 必须仍然相同。

---

### 用例 STRESS-3:并发同会话请求

**目的**:验证并发情况下 hash 计算无竞态(纯函数,理论上不可能竞态,但验证 BodyStorage 共享路径不会出问题)。

```bash
# 10 个并发相同请求
for i in $(seq 1 10); do
  (curl -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{
      "model":"claude-3-7-sonnet-20250219","max_tokens":50,
      "thinking":{"type":"enabled","budget_tokens":1024},
      "messages":[{"role":"user","content":"concurrent"}]
    }' &)
done
wait
```

**预期**:10 次日志的 `key=` 全相同,`channel_id` 全相同。任何 panic 日志都是 bug 信号。

---

## 十、覆盖矩阵

下表标记每个 AFFINITY.md 决策分支是否已被本文档某用例覆盖:

| 分支 | 用例 | 覆盖 |
|------|------|------|
| P0 路径过滤(非 POST) | P0-1 | ✓ |
| P0 路径过滤(非 /messages) | P0-2 / P0-3 | ✓ |
| E1a body 过大 | E1a / STRESS-1 | ✓ |
| E1b body 读取失败 | (依赖底层 IO 错误,运维侧观察) | 半覆盖 |
| E2 panic 恢复 | (依赖人为构造异常,日志侧观察) | 半覆盖 |
| S0 invalid_json | S0 | ✓ |
| S1 metadata.user_id 短路 | S1 / S1-边界(空) / S1-边界(无 user_id) | ✓ |
| S2 普通 chat | S2 / E2E-2 | ✓ |
| S3 first_user 无 text | S3 | ✓ |
| S4 loose 前缀全空 | S4 | ✓ |
| A1 顶层 thinking | A1 / A1-边界 / STAB-1 | ✓ |
| A2 顶层 tools[] | A2 / STAB-2 / STAB-3 | ✓ |
| A3 messages 内 thinking 块 | A3 / STAB-1 第 2 轮 | ✓ |
| A4 messages 内 tool_use/tool_result | A4 | ✓ |
| A5 messages 内 cache_control(strict 锁定) | A5 | ✓ |
| A6 tools[*].cache_control | A6 | ✓ |
| B1 system cache loose | B1 / B1-跨用户 | ✓ |
| 多触发并存 strict 优先 | PRIO-1 | ✓ |
| 多轮稳定性 | STAB-1 | ✓ |
| tools 排序不变性 | STAB-2 | ✓ |
| cache_control 字段不进 hash | STAB-3 | ✓ |
| 不同会话分散 | STAB-4 | ✓ |
| 同 token salt | SALT-1 | ✓ |
| 跨 token 隔离 | SALT-2 / E2E-3 | ✓ |
| 降级 salted=false | SALT-3 | 半覆盖(需特殊配置触发) |
| 端到端渠道一致 | E2E-1 / E2E-3 | ✓ |
| 大 body | STRESS-1 / STRESS-2 | ✓ |
| 并发 | STRESS-3 | ✓ |

---

## 十一、回归脚本骨架(可选)

如果希望把上述用例做成可重复的 CI 脚本,可以参考下面的骨架(纯 bash,不引入额外依赖):

```bash
#!/usr/bin/env bash
set -e

BASE_URL="${BASE_URL:-http://localhost:3000}"
TOKEN="${TOKEN:?need TOKEN}"
LOG_FILE="${LOG_FILE:-./logs/oneapi-$(date +%Y%m%d)*.log}"

# 抓取最近一次 [claude_affinity] 日志的 key 字段
last_key() {
  grep '\[claude_affinity\] hit' $LOG_FILE | tail -1 | awk -F'key=' '{print $2}' | awk '{print $1}'
}

# 用例 STAB-1 自动化
echo "=== STAB-1: 多轮 hash 稳定 ==="
for body in \
  '{"model":"claude-3-7-sonnet-20250219","max_tokens":50,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}' \
  '{"model":"claude-3-7-sonnet-20250219","max_tokens":50,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"hello back"}]},{"role":"user","content":"r2"}]}'
do
  curl -s -X POST "$BASE_URL/v1/messages" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$body" > /dev/null
  sleep 0.3
done

# 取最近两次的 key
keys=$(grep '\[claude_affinity\] hit' $LOG_FILE | tail -2 | awk -F'key=' '{print $2}' | awk '{print $1}')
k1=$(echo "$keys" | head -1)
k2=$(echo "$keys" | tail -1)

if [ "$k1" = "$k2" ]; then
  echo "PASS: STAB-1 keys identical: $k1"
else
  echo "FAIL: STAB-1 keys differ: $k1 vs $k2"
  exit 1
fi
```

按需要扩展到所有用例。
