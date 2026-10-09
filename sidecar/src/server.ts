/**
 * gRPC server：把 proto 契约接到 Gateway 上。
 *
 * 这个文件是 sidecar 里唯一 import gRPC 的地方，也是唯一需要"翻译错误码"的地方。
 * 它不含任何业务判断——所有策略都在 gateway.ts 里，这样策略才能被单测覆盖。
 *
 * 两条纪律：
 *   1. **绝不把 error.message 之外的任何东西放进 gRPC 状态**：
 *      metadata 里不放、detail 里不放、请求原文不放。错误消息已由
 *      errors.ts 的 sanitize 过一遍。
 *   2. **proto 路径来自配置**：容器里与仓库里的相对位置不同，
 *      写死路径会让"本地能跑、镜像里跑不起来"。
 */

import { appendFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import type { Server, ServerUnaryCall, ServerWritableStream, sendUnaryData, ServiceError } from "@grpc/grpc-js";
import {
  Metadata as GrpcMetadata,
  ServerCredentials,
  Server as GrpcServer,
  loadPackageDefinition,
} from "@grpc/grpc-js";

import { loadConfig, type SidecarConfig } from "./config.ts";
import { createEmbeddingLayer, type EmbeddingLayer } from "./embeddings.ts";
import { GatewayError, grpcCodeFor, sanitize, toGatewayError } from "./errors.ts";
import { Gateway, type CompleteInput, type EmbedInput } from "./gateway.ts";
import { createModelLayer, type ModelsCollection } from "./models.ts";
import { createUsageSink, newMetrics } from "./observability.ts";

const require = createRequire(import.meta.url);

export const VERSION = "0.1.0";

/** proto 里的消息形状（只列出我们真正读写的字段）。 */
interface ProtoCompleteRequest {
  model?: string;
  system?: string;
  prompt?: string;
  temperature?: number;
  max_tokens?: number;
  response_format?: string;
  request_id?: string;
}

interface ProtoUsage {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cost_usd: number;
}

interface ProtoCompleteResponse {
  text: string;
  model: string;
  usage: ProtoUsage;
}

interface ProtoCompleteChunk {
  delta: string;
  done: boolean;
  /**
   * usage 是可缺省字段：proto 约定它**只在终止帧**上有值。
   * 类型上标成可选，正是为了让"每一帧都塞一个零值 usage"这种写法编译不过。
   */
  usage?: ProtoUsage;
}

interface ProtoEmbedRequest {
  inputs?: string[];
  model?: string;
  normalize?: boolean;
  request_id?: string;
}

interface ProtoEmbedding {
  values: number[];
}

interface ProtoEmbedResponse {
  vectors: ProtoEmbedding[];
  model: string;
  dim: number;
  normalized: boolean;
  usage: ProtoUsage;
}

interface ProtoHealthResponse {
  serving: boolean;
  version: string;
  providers: string[];
  detail: string;
}

/**
 * 方法名必须与 proto 里的 RPC 名逐字一致（首字母小写的驼峰），
 * grpc-js 是按名字在服务定义里查找 handler 的：写成 Complete 会在
 * 注册时**静默**找不到实现（启动不报错，调用时报 UNIMPLEMENTED）。
 */
interface ModelGatewayHandlers {
  complete(call: ServerUnaryCall<ProtoCompleteRequest, ProtoCompleteResponse>, cb: sendUnaryData<ProtoCompleteResponse>): void;
  completeStream(call: ServerWritableStream<ProtoCompleteRequest, ProtoCompleteChunk>): void;
  health(call: ServerUnaryCall<unknown, ProtoHealthResponse>, cb: sendUnaryData<ProtoHealthResponse>): void;
  embed(call: ServerUnaryCall<ProtoEmbedRequest, ProtoEmbedResponse>, cb: sendUnaryData<ProtoEmbedResponse>): void;
}

/**
 * 加载 proto 并构造服务定义。
 *
 * 用 proto-loader 在**运行期**加载而不是编译成 JS：proto 是 Go 与 Node
 * 共享的唯一契约文件，运行期加载保证两边永远读的是同一个文件——
 * 编译产物一旦入库，就会出现"proto 改了但 sidecar 用的还是旧定义"。
 *
 * 注意 loadSync 只产出 PackageDefinition，把它变成可注册的服务要交给
 * grpc-js 的 loadPackageDefinition（proto-loader 0.8 起不再导出这个函数）。
 */
export function loadServiceDefinition(protoPath: string): { service: unknown } {
  const loader = require("@grpc/proto-loader") as {
    loadSync(path: string, options: Record<string, unknown>): unknown;
  };
  const definition = loader.loadSync(protoPath, {
    // keepCase：不把 max_tokens 转成 maxTokens。保持与 proto 逐字一致，
    // 这样排查时"proto 里的字段名"与"代码里的字段名"可以直接对照。
    keepCase: true,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
  });
  const pkg = loadPackageDefinition(definition as never) as unknown as {
    ai: { v1: { ModelGateway: { service: unknown } } };
  };
  return { service: pkg.ai.v1.ModelGateway.service };
}

function toUsage(u: { promptTokens: number; completionTokens: number; totalTokens: number; costUsd: number }): ProtoUsage {
  return {
    prompt_tokens: u.promptTokens,
    completion_tokens: u.completionTokens,
    total_tokens: u.totalTokens,
    cost_usd: u.costUsd,
  };
}

/** 认识的结构化输出取值。空串 = 不要求结构化输出。 */
const JSON_FORMATS = new Set(["json", "json_object"]);

/**
 * 把 proto 请求翻译成 Gateway 的输入。
 *
 * response_format 的规则（写清楚，因为它是**契约**的一部分）：
 *   - 空串 → 不要求结构化输出；
 *   - "json" / "json_object"（大小写与首尾空白宽松）→ 要求 JSON 对象；
 *   - 其他任何取值 → 直接以 INVALID_ARGUMENT 拒绝。
 *
 * 为什么"未知取值必须拒绝"而不是"忽略它继续当普通文本"：
 * 调用方明确表达了对输出格式的要求，静默降级会让它拿到不符合契约的结果
 * 却以为一切正常——那正是脏数据入库的路径。宁可失败得响亮。
 */
function toCompleteInput(req: ProtoCompleteRequest): CompleteInput {
  const rawFormat = (req.response_format ?? "").trim().toLowerCase();
  if (rawFormat !== "" && !JSON_FORMATS.has(rawFormat)) {
    throw new GatewayError("bad_request", `不支持的 response_format: ${rawFormat}`);
  }
  return {
    model: req.model ?? "",
    system: req.system ?? "",
    prompt: req.prompt ?? "",
    temperature: typeof req.temperature === "number" ? req.temperature : 0,
    maxTokens: typeof req.max_tokens === "number" ? req.max_tokens : 0,
    json: rawFormat !== "",
    requestId: req.request_id ?? "",
  };
}

/**
 * 把 proto 请求翻译成 Gateway 的 embedding 输入。
 *
 * 三条默认值都是刻意的：
 *   - inputs 缺省 → 空数组，交给 Gateway 以 INVALID_ARGUMENT 拒绝
 *     （"什么都没传"不是"传了空文本"，两者都该失败，但原因不同）；
 *   - model 缺省 → 空串，表示"用 sidecar 配置的默认模型"；
 *   - normalize 缺省 → **true**。proto3 的 bool 没有"未指定"状态，
 *     所以这里必须选一个默认值。选 true 是因为 L2 归一化是余弦检索的
 *     前提，没归一化的向量会让长文本系统性占优，而这种偏差在结果里
 *     看起来像"模型偏好长内容"，几乎不可能反查到是缺了一个字段。
 */
function toEmbedInput(req: ProtoEmbedRequest): EmbedInput {
  return {
    inputs: Array.isArray(req.inputs) ? req.inputs : [],
    model: req.model ?? "",
    normalize: req.normalize !== false,
    requestId: req.request_id ?? "",
  };
}

/** 构造 gRPC handler。单独导出是为了能在不监听端口的情况下测它。 */
export function createHandlers(gateway: Gateway): ModelGatewayHandlers {
  return {
    complete(call, cb) {
      let input: CompleteInput;
      try {
        input = toCompleteInput(call.request ?? {});
      } catch (err: unknown) {
        cb(toGrpcError(err));
        return;
      }
      gateway
        .complete(input)
        .then((out) => {
          cb(null, { text: out.text, model: out.model, usage: toUsage(out.usage) });
        })
        .catch((err: unknown) => {
          cb(toGrpcError(err));
        });
    },

    completeStream(call) {
      let input: CompleteInput;
      try {
        input = toCompleteInput(call.request ?? {});
      } catch (err: unknown) {
        call.destroy(toGrpcError(err));
        return;
      }

      // 客户端取消 -> AbortSignal -> provider 停止生成。
      //
      // 为什么必须有：grpc-js 在客户端取消后只是静默丢弃后续 write，
      // **不会**停止 sidecar 里的生成循环。不接这个信号的话，provider 会一直
      // 生成到最后，token 照常计费，而两边日志都看不出异常（实测：取消后
      // 400ms 内仍在产出分片）。这是纯粹的钱与配额的泄漏。
      const controller = new AbortController();
      call.on("cancelled", () => {
        controller.abort();
        // 取消属于"调用方主动放弃"，与失败区分开：它是运维需要看到的信号
        // （可能意味着网关超时设置太紧），故为 warn 而不是 error。
        console.warn(`[sidecar] 客户端取消了流式调用 request_id=${input.requestId}`);
      });

      // 迭代必须包在 try/catch 内：流式路径的异常是在**迭代时**抛出的
      // （见 Gateway.completeStream 的注释），包住调用本身是抓不到的。
      void (async () => {
        try {
          for await (const chunk of gateway.completeStream(input, controller.signal)) {
            call.write({
              delta: chunk.delta,
              done: chunk.done,
              // usage **只在终止帧上**有值（proto 的约定）。
              // 以前每一帧都写一个零值 usage，会让"靠 usage 是否存在判断
              // 是否收到终止帧"的消费方永远判断不出来。
              ...(chunk.done && chunk.usage
                ? {
                    usage: toUsage({
                      promptTokens: chunk.usage.input,
                      completionTokens: chunk.usage.output,
                      totalTokens: chunk.usage.totalTokens,
                      costUsd: chunk.costUsd ?? chunk.usage.cost.total,
                    }),
                  }
                : {}),
            });
          }
          call.end();
        } catch (err: unknown) {
          // 客户端已经取消时不要再 destroy：那只会产生一条误导性的错误日志。
          if (controller.signal.aborted) return;
          call.destroy(toGrpcError(err));
        }
      })();
    },

    embed(call, cb) {
      let input: EmbedInput;
      try {
        input = toEmbedInput(call.request ?? {});
      } catch (err: unknown) {
        cb(toGrpcError(err));
        return;
      }
      gateway
        .embed(input)
        .then((out) => {
          cb(null, {
            // 逐条转成 proto 的 Embedding 消息。
            //
            // 用 for 循环而不是 map：这里必须保证**顺序与条数**与输入严格一致，
            // 而 .map 在没有显式返回时会静默产出空槽（得到 undefined 元素），
            // 那会在序列化阶段以一个与原因无关的错误暴露出来。
            vectors: out.vectors.map((values) => ({ values })),
            model: out.model,
            dim: out.dim,
            normalized: out.normalized,
            usage: toUsage(out.usage),
          });
        })
        .catch((err: unknown) => {
          cb(toGrpcError(err));
        });
    },

    health(_call, cb) {
      const h = gateway.health();
      cb(null, {
        serving: h.serving,
        version: h.version,
        providers: [...h.providers],
        detail: h.detail,
      });
    },
  };
}

/**
 * 内部错误 -> gRPC 状态。
 *
 * 只把 message 带出去，不带 stack、不带 metadata：stack 里可能出现
 * 请求相关的变量名与值（排查时有用，但可能含敏感内容）。
 * 需要 stack 的场景是 sidecar 自己的日志，不是跨进程响应。
 *
 * provider 的错误文本（限流、额度、模型不存在）对排查有价值，
 * 但它必须先经 errors.ts 的 sanitize 清洗——toGatewayError 已经做了这件事。
 */
function toGrpcError(err: unknown): ServiceError {
  const gerr: GatewayError = toGatewayError(err);
  // 出口处再清洗一次是刻意的冗余：toGatewayError 已经清洗过，
  // 但"密钥绝不跨进程"这条约束不能依赖单一环节的正确性——
  // 将来有人在 GatewayError 之外新加一条错误路径时，这一层仍然兜得住。
  const message = sanitize(gerr.message);
  return {
    name: gerr.name,
    message,
    code: grpcCodeFor(gerr.code),
    details: message,
    metadata: new GrpcMetadata(),
  };
}

export interface StartedServer {
  readonly server: Server;
  readonly port: number;
  shutdown(): Promise<void>;
}

export interface StartServerOptions {
  /**
   * 注入一个已经装配好的 embedding 层（测试用）。
   *
   * 与 models 同一个理由：让"真实 gRPC 客户端 -> proto -> handler -> 层"
   * 这条链路能被完整覆盖，而 CI 不需要 API key、不需要网络。
   */
  readonly embedding?: EmbeddingLayer;
  /**
   * 注入一个已经装配好的 Models 集合（测试用）。
   *
   * 有了它，测试可以用 pi-ai 自带的 faux provider 跑通"真实 gRPC 客户端 ->
   * 真实 proto -> 真实 handler -> 模型层"的完整链路，而 CI 不需要 API key。
   * 没有它的话，端到端路径就只能靠"有一个真 key 的环境"来验证。
   */
  readonly models?: ModelsCollection;
}

/** 启动 gRPC 服务并返回句柄。 */
export async function startServer(
  cfg: SidecarConfig = loadConfig(),
  options: StartServerOptions = {},
): Promise<StartedServer> {
  const models = await createModelLayer({
    defaultModel: cfg.defaultModel,
    models: options.models,
    // 只有显式 AI_PROVIDER=faux 时才注入（见 config.ts 的安全边界）。
    ...(cfg.fauxProvider
      ? { faux: { response: cfg.fauxResponse, responses: cfg.fauxResponses } }
      : {}),
  });
  // embedding 与 chat 共用 AI_PROVIDER=faux 这一个演示开关：
  // 演示场景要的是"整条链路离线可复现"，让两个通道各有一个开关
  // 只会制造"chat 是假的、向量是真的"这种半真半假的状态。
  const embedding =
    options.embedding ?? createEmbeddingLayer({ config: cfg.embedding, faux: cfg.fauxProvider });
  // 用 promise 版 appendFile：错误会以 reject 的形式回到 sink，
  // 由它统一记一条 warn（见 observability.ts 的注释）。
  const usage = createUsageSink(cfg.usageLogPath, (path, data) => appendFile(path, data));
  const gateway = new Gateway({
    enabled: cfg.enabled,
    models,
    embedding,
    usage,
    metrics: newMetrics(),
    version: VERSION,
    defaultMaxTokens: cfg.maxTokens,
    // 服务端兜底超时留得比 Go 侧（默认 2s）宽：Go 侧的 deadline 才是
    // 真正的预算，sidecar 这一层只是防止一次调用把连接挂死。
    timeoutMs: 30_000,
  });

  const { service } = loadServiceDefinition(cfg.protoPath);
  const handlers = createHandlers(gateway);

  const server = new GrpcServer({
    "grpc.max_receive_message_length": 4 * 1024 * 1024,
  });
  server.addService(service as never, handlers as never);


  const port = await new Promise<number>((resolve, reject) => {
    server.bindAsync(cfg.listenAddr, ServerCredentials.createInsecure(), (err, boundPort) => {
      if (err) reject(err);
      else resolve(boundPort);
    });
  });

  const h = gateway.health();
  console.log(
    `[sidecar] v${VERSION} 监听 ${cfg.listenAddr}（端口 ${port}）；` +
      `enabled=${cfg.enabled} serving=${h.serving} providers=[${h.providers.join(",")}] proto=${cfg.protoPath}`,
  );
  if (cfg.fauxProvider) {
    // 演示模式必须在启动日志里说清楚：它的输出是脚本化的，
    // 任何基于它的指标/结论都不代表真实模型的效果。故为 warn。
    console.warn("[sidecar] 离线演示模式（AI_PROVIDER=faux）：模型输出是脚本化的，仅供链路验证");
  }
  if (!h.serving) {
    // 这是合法状态，故为 warn 而不是 error：没配 key 时 sidecar 照样活着，
    // Go 侧看到 serving=false 会直接降级。
    console.warn(`[sidecar] 不提供模型服务：${h.detail}`);
  }

  return {
    server,
    port,
    async shutdown() {
      await new Promise<void>((resolve) => {
        // forceShutdown 的窗口交给编排层（SIGKILL）兜底；
        // 这里只把在途调用尽量放完，最多等 5 秒。
        const timer = setTimeout(() => {
          server.forceShutdown();
          resolve();
        }, 5_000);
        server.tryShutdown(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    },
  };
}

// 直接执行本文件时启动（tsx src/server.ts）。
// 被测试 import 时不启动：避免测试用例意外占端口。
//
// 用 pathToFileURL 而不是字符串拼 "file://"+argv[1]：路径里有空格或 # 时，
// import.meta.url 是百分号编码的而拼出来不是，比较会失败——表现为"进程退出码 0
// 但什么都没监听"，这是最难查的一类"看起来启动了"。
const invokedDirectly =
  process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;

if (invokedDirectly) {
  // 进程级兜底钩子必须在启动**之前**注册：启动期失败（proto 不存在、端口被占）
  // 也走同一条路径，否则会以原始堆栈崩掉，在 restart: always 下变成静默重启循环。
  let started: StartedServer | undefined;
  const stop = (signal: string): void => {
    console.log(`[sidecar] 收到 ${signal}，开始优雅关闭`);
    if (started === undefined) process.exit(0);
    void started.shutdown().then(() => process.exit(0));
  };
  process.on("SIGTERM", () => stop("SIGTERM"));
  process.on("SIGINT", () => stop("SIGINT"));

  // 未捕获异常不静默：这个进程挂了不影响主系统，但日志里必须留下原因。
  // 注意只打 message，不打整个错误对象：错误里可能带请求上下文。
  process.on("unhandledRejection", (reason) => {
    console.error(`[sidecar] unhandledRejection: ${reason instanceof Error ? reason.message : String(reason)}`);
  });

  try {
    started = await startServer(loadConfig());
  } catch (err) {
    // 启动期失败给一行能行动的原因就退出。刻意不做无限重试：
    // 配置错误不会因为重试而变好，而 restart: always 已经提供了重启语义。
    console.error(`[sidecar] 启动失败: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(1);
  }
}
