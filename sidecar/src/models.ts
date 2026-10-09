/**
 * 模型层：把 pi-ai 包成"这个进程唯一认识模型的地方"。
 *
 * 为什么单独一层：
 *   - gateway.ts 只做"协议 <-> 业务参数"的翻译，不认识 pi-ai 的任何类型，
 *     于是它可以用一个假的模型层做单测（CI 不需要 API key）。
 *   - 换 provider、换 SDK 只动本文件。
 *
 * pi-ai 的关键事实（读源码确认过，不是猜的）：
 *   - provider 的模型目录是**静态内置**的，不需要联网拉 catalog；
 *   - `models.getAuth(providerId)` 在未配置 key 时返回 undefined ——
 *     这正是我们判断"能不能提供服务"的依据，且**不需要真的发一次请求**；
 *   - 认证失败/模型出错时，`completeSimple` 不抛异常，而是返回
 *     stopReason === "error" 且带 errorMessage 的消息。漏判这一条会把
 *     一次失败当成空结果写进库，所以下面每一处都显式检查。
 */

import {
  calculateCost,
  contentText,
  createModels,
  fauxAssistantMessage,
  fauxProvider,
  type FauxResponseFactory,
  type AssistantMessage,
  type Context,
  type Model,
  type Api,
  type Usage,
} from "@earendil-works/pi-ai";
import { anthropicProvider } from "@earendil-works/pi-ai/providers/anthropic";
import { deepseekProvider } from "@earendil-works/pi-ai/providers/deepseek";
import { openaiProvider } from "@earendil-works/pi-ai/providers/openai";

import { configuredProviderIds } from "./config.ts";
import { GatewayError } from "./errors.ts";

export interface ModelRequest {
  readonly system: string;
  readonly prompt: string;
  readonly temperature: number;
  readonly maxTokens: number;
  readonly json: boolean;
  readonly model: string;
  readonly requestId: string;
}

export interface ModelResult {
  readonly text: string;
  readonly model: string;
  readonly usage: Usage;
  readonly costUsd: number;
}

export interface ModelChunk {
  readonly delta: string;
  readonly done: boolean;
  readonly usage?: Usage;
  readonly costUsd?: number;
}

/**
 * ModelLayer 是 gateway 唯一依赖的模型能力。
 * 定义成接口的价值：单测里可以塞一个脚本化的假实现，
 * 于是"JSON 校验失败怎么办""provider 报错怎么映射"这些分支都能被覆盖，
 * 而 CI 完全不需要网络与 API key。
 */
export interface ModelLayer {
  /** 可用的 provider id 列表（已配置凭据的）。 */
  servingProviders(): readonly string[];
  /** 该模型是否存在于已知目录中。 */
  hasModel(provider: string, model: string): boolean;
  /** 默认模型 id（首个可用 provider 的第一个模型）。 */
  defaultModel(): string | undefined;
  complete(req: ModelRequest): Promise<ModelResult>;
  /**
   * signal 用于把"调用方取消了"传达给 provider。
   *
   * 为什么必须有：grpc-js 在客户端取消后只是不再发送数据，**不会**停止
   * sidecar 侧的生成；不把 signal 传下去的话，provider 会继续生成、
   * 继续计费，而结果没人接收。这类"钱花了但两边的日志都看不出问题"的
   * 泄漏只能靠显式传递取消信号来消除。
   */
  stream(req: ModelRequest, signal?: AbortSignal): AsyncIterable<ModelChunk>;
  /** 把 provider/model 解析成"用哪个模型"，供日志与响应回填。 */
  resolve(req: ModelRequest): { provider: string; model: string } | undefined;
}

/** 支持的 provider 工厂。刻意是白名单而不是"遍历所有 provider"：
 *  显式列表让"支持哪些 provider"这件事在代码里可读，也让新增 provider
 *  变成一次有意识的改动。 */
const PROVIDER_FACTORIES: Record<string, () => { id: string }> = {
  openai: openaiProvider as unknown as () => { id: string },
  anthropic: anthropicProvider as unknown as () => { id: string },
  deepseek: deepseekProvider as unknown as () => { id: string },
};

export interface CreateModelLayerOptions {
  /** 注入 provider 的 id 列表；不传则按环境变量探测。 */
  readonly providerIds?: readonly string[];
  /** 注入默认模型名。 */
  readonly defaultModel?: string;
  /**
   * 离线演示模式：注册 pi-ai 的 faux provider，并按给定文本作答。
   *
   * 只在 AI_PROVIDER=faux 时由装配层打开（见 config.ts 的三条安全边界）。
   * 它让"发布 -> 打标 -> 按标签查"这条闭环可以离线复现：
   * 没有它，P1 的核心验收项就只能靠人工在配了 key 的环境里点一遍。
   *
   * responses 是**按序循环**的多条应答（P2 前置项 S1）。为空时
   * 退回 response 单条模式（它同样可重复，不再是一次性的）。
   */
  readonly faux?: { readonly response: string; readonly responses?: readonly string[] };
  /**
   * 注入一个已经装配好的 Models 集合（测试用）。
   *
   * 为什么留这个口子，而不是在测试里另写一份假模型层：
   * 本文件对 pi-ai 的用法（contentText、stopReason、usage.cost、事件名）
   * 全是按源码约定写的，**只有让测试跑真实的 pi-ai 代码**才能守住这些约定。
   * 有了它，测试可以用 pi-ai 自带的 faux provider 跑完整链路，
   * 而 CI 依然不需要 API key、不需要网络。
   */
  readonly models?: ModelsCollection;
}

/** pi-ai 的 Models 集合。用一个别名收口，避免本文件到处出现 pi-ai 类型。 */
export type ModelsCollection = ReturnType<typeof createModels>;

/**
 * 创建一个基于 pi-ai 的模型层。
 *
 * 它是 async 的，因为"哪些 provider 真的可用"必须**在启动时算一次**：
 * pi-ai 用 `getAuth()` 解析凭据（异步），而 `servingProviders()` 会被
 * 同步调用（Health 处理器与 Gateway 都要用）。与其在每次探活时去猜，
 * 不如在启动时解析一次——凭据来自环境变量与本地凭据库，进程生命周期内不变。
 *
 * 它**不会**发起任何网络请求：pi-ai 的模型目录是静态内置的，
 * 凭据解析是纯本地操作。因此这个函数在没有任何 API key 的环境里也总是成功——
 * 这正是"sidecar 起不来也不能影响 API/Worker"这一条在本进程内的对应物。
 */
export async function createModelLayer(options: CreateModelLayerOptions = {}): Promise<ModelLayer> {
  const models = options.models ?? createModels();
  const candidates: string[] = options.models
    ? // 注入的集合里已经装好了 provider（测试场景），直接采用它报告的列表。
      options.models.getProviders().map((p) => String(p.id))
    : [];

  if (!options.models && options.faux) {
    // 演示模式：注册 faux provider 并脚本化它的回答。
    //
    // 放在这里而不是"另写一套假模型层"：走的仍是 createModels / completeSimple /
    // streamSimple 这条真实路径，因此演示覆盖的是生产代码，而不是一份镜像实现。
    // faux provider 的模型目录默认只有一个 `faux-1`，而 Go 侧会带上
    // AI_DEFAULT_MODEL（compose 里的默认值是 gpt-4o-mini）请求模型。
    // 不把配置里的模型名注册进去，演示就会以
    // `未知模型: gpt-4o-mini` 失败——而这**正是** S2 要修的场景：
    // 离线演示必须"仅靠环境变量"就能通过 compose 跑通，
    // 不该要求使用者知道 sidecar 内部那个 faux 模型 id。
    //
    // 注意只注册**配置里的那一个** id，不做"任何模型名都放行"：
    // 后者会让真实环境里"模型名写错了"这件事静默变成"用了别的模型"。
    const fauxModelId = (options.defaultModel ?? "").trim();
    const faux = fauxProvider(fauxModelId === "" ? undefined : { models: [{ id: fauxModelId }] });
    models.setProvider(faux.provider);
    const scripted = (options.faux.responses ?? []).filter((r) => r.trim() !== "");
    const pool = scripted.length > 0 ? scripted : [options.faux.response];
    // 用**应答工厂**而不是应答队列。
    //
    // 为什么：pi-ai 的 faux 是"队列语义"——`setResponses([...])` 里的每一项
    // 只会被 `shift()` 消费一次，队列空了之后每次调用都返回
    // `No more faux responses queued`（实测：连续发布 3 条视频，第 2、3 条全部失败）。
    // 这对演示与评测是致命的：P2 的精排一次请求里可能调用多次模型，
    // 而批量回填会让失败率随条数线性上升。
    //
    // 工厂是 pi-ai 明确支持的形态（FauxResponseFactory），但它**仍然挂在队列上**：
    // `shift()` 取走工厂之后队列就空了，下一次调用照样报
    // "No more faux responses queued"（实测：只塞一个工厂仍然只成功一次）。
    // 所以工厂必须把自己放回队列——这样队列永远不空，而每次调用仍然
    // 拿得到"按序的第几个应答"（state.callCount 从 1 开始）。
    const scriptedFactory: FauxResponseFactory = (_context, _streamOptions, state) => {
      const idx = Math.max(0, state.callCount - 1) % pool.length;
      faux.appendResponses([scriptedFactory]);
      return fauxAssistantMessage(pool[idx] ?? pool[0] ?? "");
    };
    faux.setResponses([scriptedFactory]);
    candidates.push("faux");
  } else if (!options.models) {
    for (const id of options.providerIds ?? configuredProviderIds()) {
      const factory = PROVIDER_FACTORIES[id];
      if (!factory) continue;
      models.setProvider(factory() as never);
      candidates.push(id);
    }
  }

  // 只把"确实解析到凭据"的 provider 记为可用。
  // 注意：注册了但没有 key 的 provider 不算可用——这正是"sidecar 起来了但
  // 不提供服务"的判定依据，也是 Go 侧决定要不要走真实调用的依据。
  const registered: string[] = [];
  for (const id of candidates) {
    try {
      const auth = await models.getAuth(id);
      if (auth !== undefined) registered.push(id);
    } catch {
      // 凭据解析本身失败（比如凭据文件损坏）不该让进程起不来，
      // 只把这一个 provider 视为不可用。启动期没有任何日志基建依赖，故不打印。
    }
  }

  const defaultModelId = options.defaultModel;

  /**
   * 选出要用的模型。
   *
   * 语义有两条，都很容易写错：
   *  1. **没指定模型** → 用默认模型；默认模型也不可用时，退到第一个可用
   *     provider 的第一个模型。这是"开箱即用"的路径。
   *  2. **明确指定了模型** → 必须找到它，找不到就返回 undefined（由调用方报
   *     model_not_found）。绝不能"找不到就悄悄换一个"：
   *     对打标/审核这种要落库的场景，用错模型意味着结果口径悄悄变了，
   *     而日志里只会看到一切正常。
   *
   * 支持 "provider:model" 写法：跨 provider 重名（不同家的同名模型能力不同）
   * 时必须能精确指定，否则排查"为什么结果变了"会变成猜谜。
   */
  const pickModel = (req: ModelRequest): Model<Api> | undefined => {
    const explicit = req.model.trim();
    if (explicit !== "") {
      const [maybeProvider, maybeModel] = explicit.includes(":") ? explicit.split(":", 2) : [undefined, explicit];
      if (maybeProvider !== undefined && maybeModel !== undefined) {
        return models.getModel(maybeProvider, maybeModel);
      }
      for (const id of registered) {
        const m = models.getModel(id, maybeModel!);
        if (m) return m;
      }
      return undefined;
    }

    if (defaultModelId !== undefined && defaultModelId !== "") {
      const [dp, dm] = defaultModelId.includes(":") ? defaultModelId.split(":", 2) : [undefined, defaultModelId];
      const found =
        dp !== undefined && dm !== undefined
          ? models.getModel(dp, dm)
          : registered.map((id) => models.getModel(id, dm!)).find((m) => m !== undefined);
      if (found) return found;
    }
    for (const id of registered) {
      const first = models.getModels(id)[0];
      if (first) return first;
    }
    return undefined;
  };

  const ensureServing = (): void => {
    if (registered.length === 0) {
      // 一个 key 都没配：这是合法的启动状态，不是崩溃。
      // 用明确的 code 让 Go 侧能把它与"真的坏了"区分开。
      throw new GatewayError(
        "provider_unavailable",
        "sidecar 未配置任何 provider 的 API key，不提供模型服务",
      );
    }
  };

  const buildContext = (req: ModelRequest): Context => {
    const messages: Context["messages"] = [];
    if (req.prompt.trim() !== "") {
      messages.push({ role: "user", content: req.prompt, timestamp: Date.now() });
    }
    let systemPrompt = req.system;
    if (req.json) {
      // 结构化输出的约束放在 system 里，而不是"由调用方自己拼"：
      // 调用方（Go 侧）不应该知道模型喜欢什么样的措辞，
      // 那是 provider 适配层的事。返回后我们还会再校验一次 JSON。
      systemPrompt = [systemPrompt, JSON_OUTPUT_INSTRUCTION].filter((s) => s.trim() !== "").join("\n\n");
    }
    return systemPrompt.trim() === "" ? { messages } : { systemPrompt, messages };
  };

  const extraOptions = (req: ModelRequest) => {
    const opts: { temperature?: number; maxTokens?: number } = {};
    // 0 表示"未指定"：不要把一个显式的 0 温度传成"用 provider 默认值"，
    // 也不要反过来把未指定当成 0（那会让输出完全确定，打标场景里是灾难）。
    if (req.temperature > 0) opts.temperature = req.temperature;
    if (req.maxTokens > 0) opts.maxTokens = req.maxTokens;
    return opts;
  };

  const toResult = (msg: AssistantMessage, model: Model<Api>): ModelResult => {
    if (msg.stopReason === "error") {
      // provider 的失败被表示为一条 stopReason=error 的消息，而不是异常。
      // 必须在这里显式转成错误：否则一个空结果会被当成"模型说没有内容"。
      throw new GatewayError("provider_error", msg.errorMessage ?? "provider 返回 stopReason=error");
    }
    const text = contentText(msg.content);
    if (text.trim() === "") {
      // 空输出一律算失败，**包括被 max_tokens 截断的情况（stopReason=length）**。
      //
      // 这里曾经留过 "length 例外"，理由是"截断属于调用方的预算问题"。
      // 那是错的：对打标/审核这类要落库的场景，空字符串会被当成模型输出写进库，
      // 变成一行无法解释的脏数据；而"预算不够"应当表现为一次明确的失败，
      // 让调用方去调大 max_tokens，而不是静默写入空值。
      // （review 抓到：这是真实 sidecar 唯一会产出空文本的路径。）
      throw new GatewayError(
        "invalid_response",
        msg.stopReason === "length" ? "模型输出被 max_tokens 截断且内容为空" : "模型返回了空内容",
      );
    }
    return { text, model: msg.responseModel ?? model.id, usage: msg.usage, costUsd: costOf(model, msg.usage) };
  };

  return {
    servingProviders: () => [...registered],
    hasModel: (provider, model) => models.getModel(provider, model) !== undefined,
    defaultModel: () => pickModel({ model: "", system: "", prompt: "", temperature: 0, maxTokens: 0, json: false, requestId: "" })?.id,

    async complete(req: ModelRequest): Promise<ModelResult> {
      ensureServing();
      const model = pickModel(req);
      if (!model) {
        throw new GatewayError("model_not_found", `未知模型: ${req.model || defaultModelId || "(未指定)"}`);
      }
      const msg = await models.completeSimple(model, buildContext(req), extraOptions(req));
      return toResult(msg, model);
    },

    stream(req: ModelRequest, signal?: AbortSignal): AsyncIterable<ModelChunk> {
      ensureServing();
      const model = pickModel(req);
      if (!model) {
        throw new GatewayError("model_not_found", `未知模型: ${req.model || defaultModelId || "(未指定)"}`);
      }
      // 把取消信号与调用参数一起交给 pi-ai：它负责掐断底层 HTTP 请求。
      const events = models.streamSimple(model, buildContext(req), { ...extraOptions(req), signal });
      let sawError: string | undefined;
      let sawTerminal = false;

      const iterate = async function* (): AsyncIterable<ModelChunk> {
        for await (const ev of events) {
          if (ev.type === "text_delta") {
            // done 之后不该再有任何数据：协议上 done 是终止帧，
            // 出现"done 之后还有分片"会让下游的分片拼接逻辑错乱。
            if (sawTerminal) break;
            yield { delta: ev.delta, done: false };
          } else if (ev.type === "done") {
            if (ev.message.stopReason === "error") {
              sawError = ev.message.errorMessage ?? "provider 返回 stopReason=error";
              break;
            }
            sawTerminal = true;
            yield {
              delta: "",
              done: true,
              usage: ev.message.usage,
              costUsd: costOf(model, ev.message.usage),
            };
          } else if (ev.type === "error") {
            // 事件流以 error 终止时不会再有 done 事件。
            // 注意 ev.error 本身是一条 AssistantMessage（不是 Error 对象），
            // 失败原因在它的 errorMessage 字段里。
            sawError = ev.error.errorMessage ?? "provider 流式返回错误";
            break;
          }
        }
        if (sawError !== undefined) {
          throw new GatewayError("provider_error", sawError);
        }
        if (!sawTerminal) {
          // 迭代结束却没有终止帧：这是协议违规，不能当作"成功但没内容"。
          // 否则计量不会记账、下游也永远等不到 done。
          throw new GatewayError("invalid_response", "provider 的流没有给出终止帧");
        }
      };

      return iterate();
    },

    resolve(req: ModelRequest) {
      const model = pickModel(req);
      return model ? { provider: String(model.provider), model: model.id } : undefined;
    },
  };
}

/** 结构化输出时附加到 system 的指令。 */
export const JSON_OUTPUT_INSTRUCTION =
  "你必须只输出一个合法的 JSON 对象，不要输出 Markdown 代码块、解释或任何 JSON 之外的字符。";

function costOf(model: Model<Api>, usage: Usage): number {
  // pi-ai 的 calculateCost 按模型目录里的价目表算钱，价目表缺失时返回 0。
  // 计量失败不该让功能失败，所以这里不抛错。
  try {
    return calculateCost(model, usage).total;
  } catch {
    return 0;
  }
}
