/**
 * embedding 层：**手写**一条 OpenAI 兼容的 `/v1/embeddings` 调用路径。
 *
 * 为什么不复用 pi-ai（models.ts）：
 *   pi-ai 只收录支持 function calling 的模型，**不含 embedding**
 *   （它的 README 有明确说明）。把 embedding 硬塞进 chat 通道
 *   （例如在 prompt 里要一段 JSON 数字数组）是必然出错的捷径：
 *   维度、批量、归一化、计量这四件事在 chat 通道里全都没有对应概念。
 *
 * 为什么单独一层而不是直接写在 gateway.ts 里：
 *   gateway 只做"协议 <-> 业务参数"的翻译，不认识 HTTP 与 fetch。
 *   于是它的校验、超时、计量策略都能用一个假的 embedding 层在单测里覆盖，
 *   CI 不需要网络、不需要 API key。
 *
 * 安全边界（与 models.ts 同纪律）：
 *   - API key 只在本文件的请求头里出现，绝不进入日志、错误消息或响应体；
 *   - 出错时只把 provider 的响应体**截断并清洗**后带出（见 errors.ts 的 sanitize），
 *     因为 4xx 响应体里可能回显 key 片段。
 */

import { GatewayError, sanitize } from "./errors.ts";
import type { EmbeddingConfig } from "./config.ts";

export interface EmbedRequest {
  readonly inputs: readonly string[];
  readonly model: string;
  readonly normalize: boolean;
  readonly requestId: string;
}

export interface EmbedUsage {
  readonly promptTokens: number;
  readonly totalTokens: number;
  /** embedding 没有生成量，恒为 0。保留字段是为了让成本核算只有一套口径。 */
  readonly completionTokens: number;
  readonly costUsd: number;
}

export interface EmbedResult {
  readonly vectors: number[][];
  readonly model: string;
  readonly dim: number;
  /** 实际是否做了归一化（回显行为，不是回显请求）。 */
  readonly normalized: boolean;
  readonly usage: EmbedUsage;
}

/**
 * EmbeddingLayer 是 gateway 唯一依赖的 embedding 能力。
 *
 * 定义成接口的价值与 ModelLayer 相同：单测可以塞一个脚本化假实现，
 * 于是"维度不符怎么办""返回条数不对怎么办""HTTP 500 怎么映射"这些分支
 * 都能被覆盖，而 CI 完全不需要网络与 API key。
 */
export interface EmbeddingLayer {
  /** 可用的 embedding provider（已配置凭据的）。 */
  servingProviders(): readonly string[];
  /** 默认模型 id。 */
  defaultModel(): string | undefined;
  /** 该模型的向量维度；未知时返回 undefined。 */
  dimensionOf(model: string): number | undefined;
  embed(req: EmbedRequest, signal?: AbortSignal): Promise<EmbedResult>;
}

/** 一次请求最多几条文本。与 proto 注释里的 64 保持一致。 */
export const MAX_EMBED_INPUTS = 64;

export interface CreateEmbeddingLayerOptions {
  readonly config: EmbeddingConfig;
  /**
   * 离线演示模式：用进程内的确定性词法向量代替真实 provider。
   *
   * 与 chat 的 faux 同一条安全边界：默认关闭、不引入新的网络出口、
   * 不读取真实凭据。它让"发布 -> 生成向量 -> 语义召回"这条链路
   * 在没有 API key 的环境里也能被端到端验证。
   */
  readonly faux?: boolean;
  /** 注入 fetch（测试用）。 */
  readonly fetchImpl?: typeof fetch;
}

/** OpenAI 兼容端点返回体的形状（只列出我们真正读的字段）。 */
interface EmbeddingsResponseBody {
  data?: Array<{ embedding?: unknown; index?: unknown }>;
  model?: unknown;
  usage?: { prompt_tokens?: unknown; total_tokens?: unknown };
}

export function createEmbeddingLayer(options: CreateEmbeddingLayerOptions): EmbeddingLayer {
  const cfg = options.config;
  if (options.faux) {
    return createFauxEmbeddingLayer(cfg);
  }
  const doFetch = options.fetchImpl ?? fetch;
  const available = cfg.apiKey !== "";

  return {
    servingProviders: () => (available ? ["openai-compatible"] : []),
    defaultModel: () => (available ? cfg.model : undefined),
    dimensionOf: (model) => (model === cfg.model ? cfg.dim : undefined),

    async embed(req, signal) {
      if (!available) {
        // 与 chat 的 provider_unavailable 同一个 code：
        // Go 侧据此"直接降级"而不是当成故障刷告警。
        throw new GatewayError(
          "provider_unavailable",
          "未配置 embedding 凭据（AI_EMBEDDING_API_KEY 或 OPENAI_API_KEY），不提供向量服务",
        );
      }
      const model = req.model.trim() === "" ? cfg.model : req.model.trim();

      // 超时用 AbortSignal.timeout，并与调用方的取消信号合并。
      // 不设超时的话，一次卡住的 HTTP 请求会让这个 promise 永远挂着——
      // gateway 那层的 race 只能让**调用方**按时拿到失败，
      // 底层连接与配额却一直被占着。
      const signals: AbortSignal[] = [];
      if (cfg.timeoutMs > 0) signals.push(AbortSignal.timeout(cfg.timeoutMs));
      if (signal) signals.push(signal);
      const composed = signals.length === 0 ? undefined : signals.length === 1 ? signals[0] : AbortSignal.any(signals);

      let res: Response;
      try {
        res = await doFetch(`${cfg.baseUrl}/embeddings`, {
          method: "POST",
          headers: {
            "content-type": "application/json",
            authorization: `Bearer ${cfg.apiKey}`,
          },
          body: JSON.stringify({
            model,
            input: [...req.inputs],
            // 显式要 float：有些端点默认返回 base64，
            // 那时解析出来的"向量"会是一串乱码而不是报错。
            encoding_format: "float",
          }),
          ...(composed ? { signal: composed } : {}),
        });
      } catch (err) {
        // 超时/取消要与"端点坏了"分开：前者是常态（模型端点慢），
        // 后者才需要人看。翻译成不同的 code 让 Go 侧能分级。
        if (composed?.aborted) {
          throw new GatewayError("timeout", `embedding 请求超过 ${cfg.timeoutMs}ms 未完成`);
        }
        throw new GatewayError(
          "provider_error",
          `embedding 请求失败: ${sanitize(err instanceof Error ? err.message : String(err))}`,
        );
      }

      const bodyText = await res.text().catch(() => "");
      if (!res.ok) {
        // 只带状态码与截断后的响应体：响应体里可能回显 key 片段。
        throw new GatewayError(
          "provider_error",
          `embedding 端点返回 HTTP ${res.status}: ${sanitize(bodyText.slice(0, 300))}`,
        );
      }

      let parsed: EmbeddingsResponseBody;
      try {
        parsed = JSON.parse(bodyText) as EmbeddingsResponseBody;
      } catch {
        throw new GatewayError(
          "invalid_response",
          `embedding 响应不是合法 JSON: ${sanitize(bodyText.slice(0, 200))}`,
        );
      }

      return normalizeEmbeddingResponse(parsed, {
        expectedCount: req.inputs.length,
        wantNormalize: req.normalize,
        fallbackModel: model,
        declaredDim: cfg.dim,
      });
    },
  };
}

export interface NormalizeOptions {
  readonly expectedCount: number;
  readonly wantNormalize: boolean;
  readonly fallbackModel: string;
  readonly declaredDim: number;
}

/**
 * 校验并归一化一次 embedding 响应。
 *
 * 单独抽成导出函数是为了能被直接单测：这里每一条判断都对应一类
 * "看起来成功、实际有毒"的响应，而它们只会在检索质量上体现出来。
 *
 * 四条硬校验：
 *  1. data 必须是数组，且**条数与输入一致**——错位入库会让文本与向量
 *     永久错配，检索结果看起来只是"不够准"；
 *  2. 每条 embedding 必须是数字数组，且维度非 0；
 *  3. 所有向量维度必须一致，且等于声明的维度——换模型/换维度时
 *     这里必须先失败，否则库里会混进两种维度的向量；
 *  4. 按 index 重排（端点不保证顺序时仍然正确）。
 */
export function normalizeEmbeddingResponse(
  body: EmbeddingsResponseBody,
  opts: NormalizeOptions,
): EmbedResult {
  const data = body.data;
  if (!Array.isArray(data)) {
    throw new GatewayError("invalid_response", "embedding 响应缺少 data 数组");
  }
  if (data.length !== opts.expectedCount) {
    throw new GatewayError(
      "invalid_response",
      `embedding 返回 ${data.length} 条向量，与输入的 ${opts.expectedCount} 条不一致：拒绝入库`,
    );
  }

  const ordered: number[][] = new Array(data.length);
  for (let i = 0; i < data.length; i++) {
    const item = data[i];
    const idx = typeof item?.index === "number" && Number.isInteger(item.index) && item.index >= 0 && item.index < data.length
      ? item.index
      : i;
    const raw = item?.embedding;
    if (!Array.isArray(raw) || raw.length === 0) {
      throw new GatewayError("invalid_response", `embedding 第 ${i} 条不是非空数组`);
    }
    const vec = new Array<number>(raw.length);
    for (let j = 0; j < raw.length; j++) {
      const v = raw[j];
      if (typeof v !== "number" || !Number.isFinite(v)) {
        throw new GatewayError("invalid_response", `embedding 第 ${i} 条第 ${j} 维不是有限数字`);
      }
      vec[j] = v;
    }
    if (vec.length !== opts.declaredDim) {
      throw new GatewayError(
        "invalid_response",
        `embedding 维度为 ${vec.length}，与声明的 ${opts.declaredDim} 不一致（换模型时必须同步更新 AI_EMBEDDING_DIM）`,
      );
    }
    ordered[idx] = vec;
  }

  const dim = ordered[0]?.length ?? 0;
  const vectors = opts.wantNormalize ? ordered.map(l2Normalize) : ordered;

  const promptTokens = numberOr(body.usage?.prompt_tokens, 0);
  const totalTokens = numberOr(body.usage?.total_tokens, promptTokens);
  return {
    vectors,
    model: typeof body.model === "string" && body.model.trim() !== "" ? body.model : opts.fallbackModel,
    dim,
    // 回显**行为**：请求要求归一化时我们确实归一化了；没要求就是没做。
    normalized: opts.wantNormalize,
    usage: { promptTokens, completionTokens: 0, totalTokens, costUsd: 0 },
  };
}

/** L2 归一化；零向量原样返回（除零会得到 NaN，NaN 入库会毁掉整列检索）。 */
export function l2Normalize(vec: readonly number[]): number[] {
  let sum = 0;
  for (const v of vec) sum += v * v;
  if (sum <= 0) return [...vec];
  const inv = 1 / Math.sqrt(sum);
  return vec.map((v) => v * inv);
}

function numberOr(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}

/**
 * 离线演示用的确定性 embedding。
 *
 * 它做的是**词法**向量（带符号的特征哈希 + L2 归一化），不是语义向量：
 * 同话题的文本因为共享词汇而相似，换一种说法就不像了。
 * 这一点必须说清楚，否则"本地演示召回效果不错"会被误当成模型能力的证据。
 *
 * 为什么仍然值得有它：没有 API key 的环境（CI、新克隆的仓库）需要一条
 * 端到端的路径来验证"请求 -> 向量 -> 存储 -> 召回 -> 过滤"这条链路本身。
 * 真实效果必须用真实模型测。
 */
function createFauxEmbeddingLayer(cfg: EmbeddingConfig): EmbeddingLayer {
  const dim = cfg.dim > 0 ? cfg.dim : 1536;
  const model = "faux-lexical";
  return {
    servingProviders: () => ["faux"],
    defaultModel: () => model,
    dimensionOf: (m) => (m === model || m === "" ? dim : undefined),
    async embed(req) {
      const vectors = req.inputs.map((text) => {
        const vec = hashedLexicalVector(text, dim);
        return req.normalize ? l2Normalize(vec) : vec;
      });
      const chars = req.inputs.reduce((acc, s) => acc + s.length, 0);
      return {
        vectors,
        model,
        dim,
        normalized: req.normalize,
        usage: {
          // 与真实端点同口径的粗略估算（4 字符 ≈ 1 token），
          // 只为让计量链路被走到，不用于任何成本结论。
          promptTokens: Math.ceil(chars / 4),
          completionTokens: 0,
          totalTokens: Math.ceil(chars / 4),
          costUsd: 0,
        },
      };
    },
  };
}

/** 带符号的特征哈希，把词法单元投到固定维度上。 */
export function hashedLexicalVector(text: string, dim: number): number[] {
  const vec = new Array<number>(dim).fill(0);
  for (const token of lexicalUnits(text)) {
    const h = fnv1a(token);
    const idx = h % dim;
    // 第二个哈希决定符号：不做符号的话，任何两个词都会让余弦非负，
    // 向量之间的区分度会明显变差（所有文本都"有点像"）。
    const sign = (Math.floor(h / dim) & 1) === 0 ? 1 : -1;
    // noUncheckedIndexedAccess 下 vec[idx] 的类型带 undefined；
    // 这里 idx 由取模得到、必然落在 [0, dim)，用 ?? 0 兜底而不是断言，
    // 是为了让"万一越界"表现为丢一个词而不是写坏整个向量。
    vec[idx] = (vec[idx] ?? 0) + sign;
  }
  return vec;
}

/**
 * 词法单元：英文/数字按连续串（小写），汉字按单字 + 相邻 bigram。
 *
 * 与 Go 侧 internal/evalset/lexical.go 的思路一致——两边都用同一套
 * "字符 bigram 兜住中文"的策略，是为了让离线演示与离线评测的口径可比。
 */
export function lexicalUnits(text: string): string[] {
  const units: string[] = [];
  let word = "";
  const flush = (): void => {
    if (word !== "") {
      units.push(word);
      word = "";
    }
  };
  for (const ch of text) {
    if (/[\p{Script=Han}]/u.test(ch)) {
      flush();
      units.push(ch);
    } else if (/[\p{L}\p{N}]/u.test(ch)) {
      word += ch.toLowerCase();
    } else {
      flush();
    }
  }
  flush();

  const tokens: string[] = [];
  for (let i = 0; i < units.length; i++) {
    const cur = units[i] ?? "";
    tokens.push(cur);
    if (i > 0) tokens.push((units[i - 1] ?? "") + cur);
  }
  return tokens;
}

/** FNV-1a 32 位哈希：确定性、无依赖、分布足够用于特征哈希。 */
export function fnv1a(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h >>> 0;
}
