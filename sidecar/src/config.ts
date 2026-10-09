/**
 * sidecar 的配置：**只从环境变量读**，没有任何必填项。
 *
 * 为什么把"永不抛错"当成硬要求：
 * 这个进程的定位是"锦上添花"。配置写错时正确的反应是**起来但声明自己不提供服务**
 * （Health 返回 serving=false），而不是退出——退出会让编排层反复重启它，
 * 在日志里制造一堆与业务无关的噪音，还会让人误以为整个系统坏了。
 *
 * 另一条纪律：provider 的 key 只在本文件里被读进内存，且**只**交给 pi-ai 的
 * auth 解析。它绝不进入日志、错误消息或 gRPC 响应体（见 log.ts 与 toStatus）。
 */

import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export interface SidecarConfig {
  /** 总开关。false 时服务照常监听，但所有模型调用都会被拒绝。 */
  readonly enabled: boolean;
  /**
   * gRPC 监听地址，形如 "0.0.0.0:50051"。
   *
   * 必须是 host:port 的完整形式，**不能**写成 ":50051"：
   * grpc-js 会把空 host 交给 dns.lookup，得到一个空主机名，
   * 在 Node 24 上直接失败（`No addresses resolved`）——而它只在运行期报，
   * 类型检查与单测都发现不了。容器内监听 0.0.0.0 才是"所有网卡"。
   */
  readonly listenAddr: string;
  /** 默认模型；请求里带 model 时以其为准。 */
  readonly defaultModel: string;
  /** 默认生成上限；<=0 表示交给 pi-ai / provider 决定。 */
  readonly maxTokens: number;
  /** proto 文件路径。默认为仓库内的相对路径，compose 里用环境变量覆盖。 */
  readonly protoPath: string;
  /** 计量日志落盘路径；为空则不落盘，只打日志。 */
  readonly usageLogPath: string;
  /**
   * 离线演示模式：用 pi-ai 自带的 faux provider 代替真实 provider，
   * 并把 AI_FAUX_RESPONSE 的取值当作模型输出返回。
   *
   * 存在的理由：P1 的演示闭环（发布 -> 打标 -> listByTag 查到）需要一个
   * **确定性的模型输出**才能被复现。没有它，这个闭环只能靠"有一个真 key
   * 的环境"来验证，而 CI 与离线开发环境都做不到——那等于 P1 的核心验收项
   * 没有自动化验证路径。
   *
   * 安全边界（三条，改动时别破坏）：
   *   1. 默认关闭，只有显式设置 AI_PROVIDER=faux 才生效；
   *   2. 它不引入任何新的网络出口：faux provider 完全在进程内作答；
   *   3. 它不会读取任何真实凭据，也不会因为设置了它就忽略总开关。
   */
  readonly fauxProvider: boolean;
  /** 离线演示模式下返回的模型输出（通常是一段 JSON）。 */
  readonly fauxResponse: string;
  /**
   * 离线演示模式下按序返回的多条模型输出（JSON 数组，元素是字符串）。
   *
   * 为什么需要它（P2 前置项 S1）：一个 sidecar 进程过去只成功应答**一次**，
   * 之后所有请求都返回 "No more faux responses queued"。
   * 连续发布 2 条视频就会失败，而精排与批量评测在一次请求里可能调用多次模型——
   * 这个限制会立刻变成开发阻力。有了它，演示与联调可以连续跑 N 次。
   *
   * 为空时退回 fauxResponse（单条、可重复）。两者都设置时以本项为准：
   * 显式给了队列却只用第一条，是最难查的那种"配置没生效"。
   */
  readonly fauxResponses: readonly string[];
  /**
   * embedding 通道的配置。
   *
   * 它**不经过 pi-ai**：pi-ai 只收录支持 function calling 的模型，不含 embedding。
   * 因此这里独立配置一条 OpenAI 兼容的 `/v1/embeddings` 调用路径。
   */
  readonly embedding: EmbeddingConfig;
}

export interface EmbeddingConfig {
  /** 默认 embedding 模型。换它必须同时改 Go 侧配置与向量表的 model/dim 记录。 */
  readonly model: string;
  /**
   * 期望的向量维度。
   *
   * 声明出来的用途不是"告诉 provider 要多少维"（那由模型决定），
   * 而是**校验**：provider 返回的维度与它不一致时立刻失败，
   * 而不是把一批维度不对的向量写进库——那种数据只在检索时暴露，
   * 表现为"模型效果差"，几乎不可能反查到是维度问题。
   */
  readonly dim: number;
  /** 是否要求返回 L2 归一化向量。写进协议并由响应回显，避免两边理解不一致。 */
  readonly normalize: boolean;
  /** OpenAI 兼容端点的基础地址（不含 /embeddings）。 */
  readonly baseUrl: string;
  /** 端点凭据；为空时退回 OPENAI_API_KEY。 */
  readonly apiKey: string;
  /** 单次 embedding 请求的超时（毫秒）。 */
  readonly timeoutMs: number;
}

const moduleDir = dirname(fileURLToPath(import.meta.url));

/**
 * 解析 proto 路径。
 *
 * 默认值同时兼容两种运行位置：
 *   - 仓库内本地跑：sidecar/src/../../backend/api/proto/...
 *   - 容器内跑：/app/dist 或 /app/src + /app/api/proto/...（由 Dockerfile 摆好）
 * 找不到文件时**不报错**：交给 gRPC 加载阶段去报，那时错误信息更具体。
 */
function defaultProtoPath(): string {
  return resolve(moduleDir, "..", "..", "backend", "api", "proto", "ai", "v1", "model_gateway.proto");
}

function parseBool(raw: string | undefined, fallback: boolean): boolean {
  if (raw === undefined || raw.trim() === "") return fallback;
  const v = raw.trim().toLowerCase();
  if (["1", "true", "yes", "on"].includes(v)) return true;
  if (["0", "false", "no", "off"].includes(v)) return false;
  // 写错值时不猜：保留 fallback（默认关）。这与 Go 侧 ParseBool 的行为一致：
  // 一次拼写错误不该静默打开模型调用。
  return fallback;
}

function parseIntOr(raw: string | undefined, fallback: number): number {
  if (raw === undefined || raw.trim() === "") return fallback;
  const n = Number.parseInt(raw, 10);
  return Number.isFinite(n) ? n : fallback;
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): SidecarConfig {
  const fauxProvider = (env.AI_PROVIDER ?? "").trim().toLowerCase() === "faux";
  return {
    // 默认关：没显式打开时，一个 key 都不配也不会发起任何调用。
    enabled: parseBool(env.AI_ENABLED, false),
    listenAddr: env.AI_GATEWAY_LISTEN_ADDR?.trim() || "0.0.0.0:50051",
    defaultModel: env.AI_DEFAULT_MODEL?.trim() || "gpt-4o-mini",
    maxTokens: parseIntOr(env.AI_MAX_TOKENS, 512),
    protoPath: env.AI_GATEWAY_PROTO_PATH?.trim() || defaultProtoPath(),
    usageLogPath: env.AI_USAGE_LOG?.trim() || "",
    // 只有显式声明才进入演示模式：拼错或未设置一律走真实 provider 路径。
    fauxProvider,
    fauxResponse:
      env.AI_FAUX_RESPONSE?.trim() ||
      '{"summary":"离线演示摘要","tags":[{"name":"演示标签","confidence":0.9}]}',
    fauxResponses: parseResponseList(env.AI_FAUX_RESPONSES),
    embedding: {
      model: env.AI_EMBEDDING_MODEL?.trim() || "text-embedding-3-small",
      dim: parseIntOr(env.AI_EMBEDDING_DIM, 1536),
      normalize: parseBool(env.AI_EMBEDDING_NORMALIZE, true),
      baseUrl: (env.AI_EMBEDDING_BASE_URL?.trim() || "https://api.openai.com/v1").replace(/\/+$/, ""),
      // 独立的 key 优先，否则退回 OPENAI_API_KEY：
      // 兼容端点（自建、代理、云厂商）通常给一个单独的 key，
      // 而直接用 OpenAI 时不该要求配两遍。
      apiKey: env.AI_EMBEDDING_API_KEY?.trim() || env.OPENAI_API_KEY?.trim() || "",
      timeoutMs: parseIntOr(env.AI_EMBEDDING_TIMEOUT_MS, 10_000),
    },
  };
}

/**
 * 解析 AI_FAUX_RESPONSES。
 *
 * 只接受 JSON 字符串数组，解析失败时返回空数组（而不是抛错）：
 * 这个变量只影响离线演示，写错时正确的反应是"退回单条响应模式并让
 * Health 的 detail 说明"，而不是让进程起不来——那与"AI 必须完全可选"
 * 这条红线直接冲突。
 */
export function parseResponseList(raw: string | undefined): string[] {
  if (raw === undefined || raw.trim() === "") return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter((v): v is string => typeof v === "string" && v.trim() !== "");
  } catch {
    return [];
  }
}

/**
 * 已配置凭据的 provider 列表。
 *
 * 只**查看**环境变量是否存在，不读取也不记录其值：
 * 用 `Boolean(...)` 而不是把值带出去，是这个函数唯一需要小心的地方。
 *
 * 变量名必须与 pi-ai 的 env-api-keys 一致（多个都要列全）：
 * Anthropic 除了 ANTHROPIC_API_KEY 还接受 AUTH_TOKEN / OAUTH_TOKEN 作为
 * 合法凭据。只探测 *_API_KEY 会让"其实配好了"的环境被报告成不可用——
 * 表现为 sidecar 明明能用却一直 serving=false，Go 侧无缘无故降级。
 */
const CREDENTIAL_ENV_VARS: Record<string, readonly string[]> = {
  openai: ["OPENAI_API_KEY"],
  anthropic: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN"],
  deepseek: ["DEEPSEEK_API_KEY"],
};

export function configuredProviderIds(env: NodeJS.ProcessEnv = process.env): string[] {
  const ids: string[] = [];
  for (const [id, vars] of Object.entries(CREDENTIAL_ENV_VARS)) {
    if (vars.some((name) => (env[name] ?? "").trim() !== "")) ids.push(id);
  }
  return ids;
}
