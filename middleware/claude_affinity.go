// Package middleware 中的 ClaudeAffinityHash 中间件:
//
// 解析 POST /v1/messages 请求 body,识别其是否需要"按会话粘到同一上游 Claude
// 渠道"(否则 thinking.signature / tool_use_id 跨渠道就会触发 Bedrock 等上游的
// 400 错误,prompt cache 也会全部 miss),命中则计算一个稳定的 SHA256 hex 写入
// gin.Context 的 "claude_affinity_key" 这个固定 key。
//
// 配合 new-api 渠道亲和性配置使用:
//
//	类型: context_string
//	Key:  claude_affinity_key
//
// 中间件不写 key 时(未命中触发条件 / metadata.user_id 短路 / 错误),Distribute
// 会读到空字符串,自然回退到原本的随机/权重渠道选择策略。
//
// 第一部分(常量、类型、Inspect/hasCacheControl/normalizedSystem/canonicalTools/
// firstUserText/computeKey/writeKV)是 hash 行为的权威实现,行为规约见
// docs/claude-affinity-middleware/AFFINITY.md。这部分代码任何修改都会让线上
// 已经在跑的会话 hash 漂移,导致连续多轮请求被路由到不同渠道。改动前请先读
// docs/claude-affinity-middleware/CLAUDE.md "改动金线"章节。
package middleware

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// =============================================================================
// 配置(全部硬编码,不依赖 constant 包)
// =============================================================================

const (
	// claudeAffinityContextKey 是写入 gin.Context 的 key 名。
	// 在 new-api UI 配置渠道亲和性规则时:
	//   类型: context_string
	//   Key:  claude_affinity_key
	// 必须与此处保持一致;改动会让所有线上配置失效。
	claudeAffinityContextKey = "claude_affinity_key"

	// claudeMessagesPathSuffix 用于路径过滤,只处理 Claude Messages 协议。
	// httpRouter group 下还有 /v1/chat/completions 等多种路由,这些跳过即可。
	claudeMessagesPathSuffix = "/messages"

	// claudeAffinityLogPrefix 是日志输出前缀,便于运维 grep。
	claudeAffinityLogPrefix = "[claude_affinity]"

	// claudeAffinityKeyLogLen 是日志中亲和键打印的最大字符数。
	// 完整 SHA256 hex 是 64 字符,前 16 字符已经能区分 ~2^64 个会话,够用且不至于
	// 把整个 body 指纹泄露到日志里。
	claudeAffinityKeyLogLen = 16

	// tokenIdContextKey 是 TokenAuth 中间件写入的用户 token 数据库 id(int)。
	// 见 middleware/auth.go:270, 414 的 c.Set("token_id", token.Id)。
	// 我们用它作为 secret 前缀对 inner hash 再做一层 SHA256,实现"按 token 隔离" ——
	// 同一份 body 对不同 token 产出不同的最终亲和键,避免渠道选择被跨 token 影响。
	// 比 token_key (sk-xxx 明文) 更适合做日志 salt: 不会泄漏 API key 字面值。
	tokenIdContextKey = "token_id"
)

// defaultClaudeAffinityTrigger 三个开关全开。
// 如果未来要禁用某一类触发,直接改这里的 false。
// 修改请同步参考 docs/claude-affinity-middleware/AFFINITY.md 的"触发条件"章节。
var defaultClaudeAffinityTrigger = TriggerConfig{
	OnThinking: true,
	OnTool:     true,
	OnCache:    true,
}

// TriggerConfig 控制哪些触发器启用。
type TriggerConfig struct {
	OnThinking bool
	OnTool     bool
	OnCache    bool
}

// =============================================================================
// gin 中间件
// =============================================================================

// ClaudeAffinityHash 是注册到 /v1 group 上的中间件入口。
//
// 注册位置必须早于 middleware.Distribute(),否则 Distribute 读 context_string
// 时拿到空 → 退化成随机选渠道,整个改造失效。详见 router/relay-router.go。
//
// 中间件设计原则:
//   - 永不阻断业务请求:任何错误(body 过大、JSON 非法、panic)都直接 c.Next()。
//   - body 字节零修改:用 common.GetBodyStorage(c) 读字节,不消耗 Reader,
//     下游 controller 调 UnmarshalBodyReusable 会复用已缓冲的存储,零额外 IO。
//   - 路径过滤优先:非 /messages 直接放行,不读 body,几乎零开销。
func ClaudeAffinityHash() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		path := c.Request.URL.Path
		method := c.Request.Method

		// 1. 路径过滤:只处理 Claude Messages 协议
		if method != http.MethodPost ||
			!strings.HasSuffix(path, claudeMessagesPathSuffix) {
			logger.LogDebug(ctx, fmt.Sprintf(
				"%s skip path=%s method=%s reason=not_messages_endpoint",
				claudeAffinityLogPrefix, path, method,
			))
			c.Next()
			return
		}

		// 2. panic 恢复:hash 计算/gjson 解析理论上不该 panic,但万一发生绝不能
		//    阻断业务请求 → recover 后让请求继续走原本的渠道选择路径。
		defer func() {
			if r := recover(); r != nil {
				logger.LogError(ctx, fmt.Sprintf(
					"%s panic recovered path=%s panic=%v",
					claudeAffinityLogPrefix, path, r,
				))
				c.Next()
			}
		}()

		// 3. 取 body 字节(GetBodyStorage 内部会缓存到 c[KeyBodyStorage],
		//    下游 UnmarshalBodyReusable 会自动复用,不会重复读取)
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf(
				"%s get_body_storage_failed path=%s content_length=%d err=%s",
				claudeAffinityLogPrefix, path, c.Request.ContentLength, err.Error(),
			))
			c.Next()
			return
		}
		body, err := storage.Bytes()
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf(
				"%s read_body_failed path=%s content_length=%d err=%s",
				claudeAffinityLogPrefix, path, c.Request.ContentLength, err.Error(),
			))
			c.Next()
			return
		}

		bodyLen := len(body)
		startedAt := time.Now()

		// 4. 计算 hash(纯 CPU,典型 9µs/3KB,见 affinity_test.go BenchmarkInspect)
		result := Inspect(body, defaultClaudeAffinityTrigger)
		elapsed := time.Since(startedAt)

		// 5. 命中才写 context;未命中直接 Next 走原本随机选渠道
		if result.AffinityKey != "" {
			// 用用户的 token_id 作为 secret 前缀对 inner hash 再做一层 SHA256
			// (TokenAuth 已在更早的中间件链里把 token_id 写入 context, 见
			//  middleware/auth.go:270, 414)。
			// 即使 token_id 异常为 0(理论上 TokenAuth 不会放过去),也降级到不
			// 加 salt 的原始 hash,功能不会失效;日志里 salted 字段会标记。
			tokenID := c.GetInt(tokenIdContextKey)
			finalKey := result.AffinityKey
			salted := false
			if tokenID > 0 {
				finalKey = saltAffinityKey(strconv.Itoa(tokenID), result.AffinityKey)
				salted = true
			}
			c.Set(claudeAffinityContextKey, finalKey)
			keyShort := finalKey
			if len(keyShort) > claudeAffinityKeyLogLen {
				keyShort = keyShort[:claudeAffinityKeyLogLen]
			}
			logger.LogInfo(ctx, fmt.Sprintf(
				"%s hit tier=%s triggers=%v key=%s salted=%v token_id=%d model=%s body_bytes=%d elapsed=%s",
				claudeAffinityLogPrefix, result.Tier, result.Triggers,
				keyShort, salted, tokenID, result.Model, bodyLen, elapsed,
			))
		} else {
			reason := result.Reason
			if reason == "" {
				reason = "no_trigger_matched"
			}
			logger.LogDebug(ctx, fmt.Sprintf(
				"%s miss reason=%s model=%s body_bytes=%d elapsed=%s",
				claudeAffinityLogPrefix, reason, result.Model, bodyLen, elapsed,
			))
		}
		c.Next()
	}
}

// saltAffinityKey 用 secret 作为前缀对 inner hash 做一次外层 SHA256。
// 用途: 把"按 body 内容粘连"的 inner key 升级为"按用户+body 内容粘连"的最终 key,
// 避免不同用户的相同 body 被映射到同一个亲和键 → 同一个渠道。
//
// 不修改 affinity.go 原始的 computeKey() —— 那个函数是 hash 行为的权威实现,
// 跨版本必须稳定。在中间件层做二次 hash 是更安全的做法。
//
// 分隔符使用与 writeKV 相同的不可打印字节 (\x00 分隔 key/value, \x01 结尾),
// 避免任何拼接歧义(例如 secret 末尾带特殊字符撞上 inner 起始字节)。
func saltAffinityKey(secret, innerKey string) string {
	h := sha256.New()
	writeKV(h, "secret", secret)
	writeKV(h, "inner", innerKey)
	return hex.EncodeToString(h.Sum(nil))
}

// =============================================================================
// 以下是 hash 行为的权威纯函数实现
// 修改这部分会让线上会话 hash 漂移,改动前先读
// docs/claude-affinity-middleware/CLAUDE.md "改动金线"章节
// =============================================================================

// 触发标识,会出现在日志的 triggers 字段里。
const (
	TriggerThinking = "thinking"
	TriggerTool     = "tool"
	TriggerCache    = "cache"
)

// AffinityTier 表示一个请求需要被"绑定到同一上游渠道"的强度。
type AffinityTier string

const (
	// TierNone:没有任何需要绑定的特征,不注入 key。
	TierNone AffinityTier = ""

	// TierStrict:跨渠道路由会触发 400 错误。
	// 由以下"携带 provider 私有状态、跨轮次必须一致"的特征触发:
	//   - 顶层 thinking 启用(下一轮就会带 signature)
	//   - 顶层 tools[] 非空(下一轮可能带 tool_use_id)
	//   - messages 中已经出现 thinking / tool_use / tool_result 块
	//   - messages 内任何 content 块带 cache_control(多用户必然不可复用,
	//     用 strict 既保证同会话稳定,又能避免聚到同一渠道)
	// hash 输入包含 first_user,确保每个会话独立路由。
	TierStrict AffinityTier = "strict"

	// TierLoose:跨渠道路由仅会丢失 cache 命中,永远不会出错。
	// 仅当 system 或 tools 顶层带 cache_control、且无任何 strict 特征时触发。
	// hash 输入不含 first_user,使共享同一 system + tools 的不同会话塌缩到
	// 同一渠道,从而复用上游 prompt cache。
	TierLoose AffinityTier = "loose"
)

// InspectResult 是分析一次 /v1/messages 请求 body 的结果。
//
// AffinityKey 仅在请求至少命中一条已启用触发条件、并且能算出稳定 hash 时
// 才非空;为空时调用方应原样转发请求、不写任何 context key(具体原因见 Reason)。
type InspectResult struct {
	Model       string
	Tier        AffinityTier
	Triggers    []string
	AffinityKey string
	Reason      string
}

// Inspect 解析请求 body,决定是否需要计算亲和键。该函数不会修改 body。
func Inspect(body []byte, trig TriggerConfig) InspectResult {
	res := InspectResult{}

	if !gjson.ValidBytes(body) {
		res.Reason = "invalid_json"
		return res
	}

	root := gjson.ParseBytes(body)
	res.Model = root.Get("model").String()

	// 短路:如果 body 已经带了 metadata.user_id(Claude Code CLI 等客户端会
	// 自动加),就把亲和决策权交给上游 —— 上游会直接按 user_id 路由,本中间件
	// 不再写任何 context key。
	if root.Get("metadata.user_id").Exists() {
		res.Reason = "metadata_user_id"
		return res
	}

	triggered := map[string]struct{}{}
	strictHit := false
	looseHit := false

	if trig.OnThinking {
		if t := root.Get("thinking.type"); t.Exists() && t.String() != "disabled" {
			triggered[TriggerThinking] = struct{}{}
			strictHit = true
		}
	}
	if trig.OnTool {
		if root.Get("tools.0").Exists() {
			triggered[TriggerTool] = struct{}{}
			strictHit = true
		}
	}

	// 一次遍历 messages 找出 content 块级别的触发条件。
	//
	// 关于 messages 内 cache_control:判定为 STRICT 而非 loose。原因是被缓存的
	// 前缀必然包含会变化的 message 历史,不同用户根本不可能共享这块缓存;
	// 它真正能用上的场景只有"同一会话多轮的增量缓存",这本身就是 strict
	// 期望的"按会话粘连"语义。如果错误地按 loose 处理,一旦 system/tools 都
	// 为空,hash 就会退化成常量,把所有此类请求挤到同一个渠道。
	if trig.OnThinking || trig.OnTool || trig.OnCache {
		root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
			msg.Get("content").ForEach(func(_, blk gjson.Result) bool {
				switch blk.Get("type").String() {
				case "thinking", "redacted_thinking":
					if trig.OnThinking {
						triggered[TriggerThinking] = struct{}{}
						strictHit = true
					}
				case "tool_use", "tool_result", "server_tool_use":
					if trig.OnTool {
						triggered[TriggerTool] = struct{}{}
						strictHit = true
					}
				}
				if trig.OnCache && blk.Get("cache_control").Exists() {
					triggered[TriggerCache] = struct{}{}
					strictHit = true
				}
				return true
			})
			return true
		})
	}

	// system / tools 顶层的 cache_control 才是真正的 LOOSE 场景:被缓存的前缀
	// 是大家共享的 system 或 tools 段,不同用户应该被路由到同一渠道复用 cache。
	if trig.OnCache {
		if hasCacheControl(root.Get("system")) {
			triggered[TriggerCache] = struct{}{}
			looseHit = true
		}
		if hasCacheControl(root.Get("tools")) {
			triggered[TriggerCache] = struct{}{}
			looseHit = true
		}
	}

	if !strictHit && !looseHit {
		return res
	}

	// 触发列表按固定顺序输出,方便日志聚合。
	for _, t := range []string{TriggerThinking, TriggerTool, TriggerCache} {
		if _, ok := triggered[t]; ok {
			res.Triggers = append(res.Triggers, t)
		}
	}

	// strict 优先于 loose:只要任一 strict 触发条件命中,请求必须按"会话粒度"
	// 绑定(model + system + tools + first_user),以保证多轮 signature /
	// tool_use_id 跨轮次有效。
	system := normalizedSystem(root.Get("system"))
	tools := canonicalTools(root.Get("tools"))

	if strictHit {
		firstUser := firstUserText(root.Get("messages"))
		if firstUser == "" {
			res.Reason = "no_first_user_text"
			return res
		}
		res.Tier = TierStrict
		res.AffinityKey = computeKey(TierStrict, res.Model, system, tools, firstUser)
		return res
	}

	// Loose 档:仅由 cache_control 触发。hash 不含 first_user,使任意两个共享
	// 同一 model + system + tools 的请求塌缩到同一渠道,复用上游 prompt cache。
	//
	// 防退化保护:如果 system 与 tools 都为空,loose hash 会退化成 (tier+model)
	// 的近常量,把所有流量挤到一个渠道;此时既毁掉了负载均衡又拿不到任何
	// cache 收益(空前缀根本无东西可缓存),干脆跳过。
	if system == "" && tools == "" {
		res.Reason = "loose_skipped_empty_prefix"
		return res
	}
	res.Tier = TierLoose
	res.AffinityKey = computeKey(TierLoose, res.Model, system, tools, "")
	return res
}

// hasCacheControl 判断一个数组型节点(system 或 tools)的任一元素是否带有
// cache_control 字段。string 形式的 system 永远没有 cache_control。
func hasCacheControl(node gjson.Result) bool {
	if !node.Exists() || !node.IsArray() {
		return false
	}
	found := false
	node.ForEach(func(_, item gjson.Result) bool {
		if item.Get("cache_control").Exists() {
			found = true
			return false
		}
		return true
	})
	return found
}

// normalizedSystem 把 system 字段(字符串或 []block)规约成一段稳定的纯文本。
// 仅 type=text 的块会被纳入;cache_control 字段被忽略,以保证"加/移除一个
// 缓存断点"不会让同一会话的 hash 漂移。
func normalizedSystem(node gjson.Result) string {
	if !node.Exists() {
		return ""
	}
	if node.Type == gjson.String {
		return node.String()
	}
	if !node.IsArray() {
		return ""
	}
	var sb strings.Builder
	node.ForEach(func(_, blk gjson.Result) bool {
		if blk.Get("type").String() == "text" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(blk.Get("text").String())
		}
		return true
	})
	return sb.String()
}

type toolDef struct {
	name        string
	description string
	schema      string
}

// canonicalTools 把 tools 数组规约成一段确定性表示:按 name 升序排序、剔除
// cache_control。tools 定义被视为"会话身份"的一部分,因为修改它会让整段
// prompt cache 失效。
func canonicalTools(node gjson.Result) string {
	if !node.Exists() || !node.IsArray() {
		return ""
	}
	var defs []toolDef
	node.ForEach(func(_, t gjson.Result) bool {
		defs = append(defs, toolDef{
			name:        t.Get("name").String(),
			description: t.Get("description").String(),
			schema:      t.Get("input_schema").Raw,
		})
		return true
	})
	sort.Slice(defs, func(i, j int) bool { return defs[i].name < defs[j].name })

	var sb strings.Builder
	for _, d := range defs {
		sb.WriteString(d.name)
		sb.WriteByte(0x1f)
		sb.WriteString(d.description)
		sb.WriteByte(0x1f)
		sb.WriteString(d.schema)
		sb.WriteByte(0x1e)
	}
	return sb.String()
}

// firstUserText 返回 messages 中第一条 role=user 的所有 text 块拼接结果。
// 若 content 是字符串则直接返回;image / tool_result 等非文本块被忽略。
func firstUserText(messages gjson.Result) string {
	if !messages.Exists() || !messages.IsArray() {
		return ""
	}
	var out string
	messages.ForEach(func(_, msg gjson.Result) bool {
		if msg.Get("role").String() != "user" {
			return true
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			out = content.String()
			return false
		}
		if content.IsArray() {
			var sb strings.Builder
			content.ForEach(func(_, blk gjson.Result) bool {
				if blk.Get("type").String() == "text" {
					if sb.Len() > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(blk.Get("text").String())
				}
				return true
			})
			out = sb.String()
		}
		return false
	})
	return out
}

// computeKey 把规范化后的字段用显式分隔符拼接后做一次 SHA256。tier 标签写入
// hash 输入,确保 loose 永远不会和"first_user 恰好为空的 strict"算出同一个值。
func computeKey(tier AffinityTier, model, system, tools, firstUser string) string {
	h := sha256.New()
	writeKV(h, "tier", string(tier))
	writeKV(h, "model", model)
	writeKV(h, "system", system)
	writeKV(h, "tools", tools)
	if tier == TierStrict {
		writeKV(h, "first_user", firstUser)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeKV 用不可打印分隔符(\x00 分隔 key/value, \x01 结尾)写入一个键值对,
// 避免任何拼接歧义。
func writeKV(w interface{ Write(p []byte) (int, error) }, key, value string) {
	_, _ = w.Write([]byte(key))
	_, _ = w.Write([]byte{0x00})
	_, _ = w.Write([]byte(value))
	_, _ = w.Write([]byte{0x01})
}
