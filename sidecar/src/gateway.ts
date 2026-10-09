/**
 * Gateway：协议 <-> 模型层的翻译，以及本进程的全部策略判断。
 *
 * 这个类刻意不认识 gRPC（那是 server.ts 的事），也不认识 pi-ai
 * （那是 models.ts 的事）。于是它的每一条策略——总开关、JSON 校验、
 * 计量、超时——都能用一个假的模型层在单测里覆盖，
 * 而 CI 不需要 API key、不需要网络。
 */

import { GatewayError, sanitize, toGatewayError } from "./errors.ts";
import { MAX_EMBED_INPUTS, type EmbeddingLayer } from "./embeddings.ts";
import type { ModelChunk, ModelLayer, ModelRequest, ModelResult } from "./models.ts";
import type { Usage } from "@earendil-works/pi-ai";
import type { Metrics, UsageRecord, UsageSink } from "./observability.ts";

export interface CompleteInput {
  readonly model: string;
  readonly system: string;
  readonly prompt: string;
  readonly temperature: number;
  readonly maxTokens: number;
  readonly json: boolean;
  readonly requestId: string;
}

export interface UsageOut {
  readonly promptTokens: number;
  readonly completionTokens: number;
  readonly totalTokens: number;
  readonly costUsd: number;
}

export interface CompleteOutput {
  readonly text: string;
  readonly model: string;
  readonly usage: UsageOut;
}

export interface EmbedInput {
  readonly inputs: readonly string[];
  readonly model: string;
  readonly normalize: boolean;
  readonly requestId: string;
}

export interface EmbedOutput {
  readonly vectors: number[][];
  readonly model: string;
  readonly dim: number;
  readonly normalized: boolean;
  readonly usage: UsageOut;
}

export interface HealthOutput {
  readonly serving: boolean;
  readonly version: string;
  readonly providers: readonly string[];
  readonly detail: string;
}

export interface GatewayDeps {
  readonly enabled: boolean;
  readonly models: ModelLayer;
  readonly embedding: EmbeddingLayer;
  readonly usage: UsageSink;
  readonly metrics: Metrics;
  readonly version: string;
  readonly defaultMaxTokens: number;
  /** 单次调用的兜底超时（毫秒）；<=0 表示不设。 */
  readonly timeoutMs: number;
  /** 取当前时间，注入以便测试。 */
  readonly now?: () => number;
}

/**
 * CallMeta 是一次调用的计量上下文。
 *
 * 与 CallContext 分开是因为 embedding 通道也要记同一套账，
 * 而它没有 ModelRequest（没有 system/temperature/maxTokens 这些概念）。
 * 把公共部分抽出来，计量逻辑就只有一份实现——"流式路径忘了记计量"
 * 那类缺口正是从这种地方长出来的。
 */
interface CallMeta {
  readonly requestId: string;
  readonly provider: string;
  readonly model: string;
  readonly startedMs: number;
}

interface CallContext extends CallMeta {
  readonly req: ModelRequest;
}

export class Gateway {
  private readonly deps: GatewayDeps;

  constructor(deps: GatewayDeps) {
    this.deps = deps;
  }

  /**
   * 服务是否真的可用。
   *
   * 两个条件缺一不可，且都不需要发起真实请求就能判断——
   * 这一点很重要：Go 侧会周期性探活，探活不该消耗模型额度。
   */
  serving(): boolean {
    return this.deps.enabled && this.deps.models.servingProviders().length > 0;
  }

  health(): HealthOutput {
    const providers = this.deps.models.servingProviders();
    let detail: string;
    if (!this.deps.enabled) {
      detail = "AI_ENABLED=false，总开关关闭";
    } else if (providers.length === 0) {
      detail = "未配置任何 provider 的 API key（OPENAI_API_KEY / ANTHROPIC_API_KEY / DEEPSEEK_API_KEY）";
    } else {
      // 只报累计量，不报请求内容。
      // embedding 的可用性单独列出来：它与 chat 的凭据、模型、端点
      // 都是独立的，"chat 能用"不代表"向量能用"。
      const embed = this.deps.embedding.servingProviders();
      detail =
        `calls=${this.deps.metrics.calls} failures=${this.deps.metrics.failures} ` +
        `tokens=${this.deps.metrics.totalTokens} cost_usd=${this.deps.metrics.totalCostUsd.toFixed(6)} ` +
        `embedding=[${embed.join(",")}]`;
    }
    return {
      serving: this.serving(),
      version: this.deps.version,
      providers: [...providers],
      detail,
    };
  }

  /**
   * Complete 是一个薄薄的包装：真正的公共逻辑在 run() 里。
   * 这样"开关判断 + 计量 + 错误归一化"只有一份实现，
   * 不会出现"流式路径忘了记计量"这种只有一条路径才有的缺口。
   */
  async complete(input: CompleteInput): Promise<CompleteOutput> {
    return this.run("Complete", input, async (ctx) => {
      const res = await this.deps.models.complete(ctx.req);
      const out: CompleteOutput = {
        text: this.validateOutput(ctx.req, res),
        model: res.model,
        usage: this.usageOut(res),
      };
      // 成功路径的记账与返回值一起从回调里带出来（见 run 的注释）。
      return [out, out.usage];
    });
  }

  /**
   * CompleteStream 产出 ModelChunk 序列。
   *
   * 注意返回的是"一次性构造好的 AsyncIterable"：异常在**迭代时**抛出，
   * 因此在 server.ts 里必须把迭代放在 try/catch 内，而不是只包住调用。
   */
  completeStream(
    input: CompleteInput,
    signal?: AbortSignal,
  ): AsyncIterable<ModelChunk & { readonly model: string }> {
    const self = this;
    const ctx = this.newContext(input);

    return {
      async *[Symbol.asyncIterator]() {
        // 流式路径同样要能在中途停下来：调用方（gRPC 客户端）取消时若不把
        // signal 交给模型层，provider 会继续生成并继续计费——钱花了、
        // 结果没人要，而且这件事在两边日志里都看不出来。
        const text: string[] = [];
        let sawDone = false;
        try {
          self.ensureEnabled();
          const deadline = self.deadlineFor(ctx.startedMs);
          // 用带超时的迭代器：只靠"每片检查一次时间"是不够的——
          // provider 卡住时会**根本没有下一片**，检查点永远不会被执行，
          // 于是流会无限挂着（review 实测：给一片后卡住，300ms 后仍未返回）。
          const stream = self.deps.models.stream(ctx.req, signal);
          for await (const chunk of self.withStreamDeadline(stream, deadline, signal)) {
            if (self.nowMs() > deadline) {
              throw new GatewayError("timeout", "超过流式调用的时间预算");
            }
            if (sawDone) {
              // done 是**终止帧**（见 proto 注释）。终止帧之后无论是数据还是错误，
              // 都不该再往下传：下游的拼接与"是否结束"判断全都会因此错乱。
              break;
            }
            if (chunk.done) {
              // 结构化输出在**流结束时**整体校验：JSON 的合法性只能对完整文本判断。
              // 代价是校验失败时已经吐出了分片，调用方必须能接受"半途报错"。
              // 这个语义要写清楚，不能让下游以为"收到过分片 = 一定会成功"。
              self.validateStreamedOutput(ctx.req, text.join(""));
              self.record(ctx, "CompleteStream", self.usageFromChunk(chunk), "ok");
              sawDone = true;
            } else {
              text.push(chunk.delta);
            }
            yield { ...chunk, model: ctx.model };
          }
          if (!sawDone) {
            // 迭代结束却没有终止帧：这是协议违规，不能当成"成功但没内容"。
            // 放过去的话计量不会记账、下游永远等不到 done，而两边日志都显示正常。
            //
            // 这一层校验放在这里（而不是只放在具体的 ModelLayer 实现里）是刻意的：
            // 终止帧保证属于 Gateway 与调用方之间的**契约**，换实现也不该失效。
            throw new GatewayError("invalid_response", "流式响应没有给出终止帧");
          }
        } catch (err) {
          const gerr = toGatewayError(err);
          self.record(ctx, "CompleteStream", { promptTokens: 0, completionTokens: 0, totalTokens: 0, costUsd: 0 }, gerr.code);
          throw gerr;
        }
      },
    };
  }

  /**
   * 流式结构化输出的校验。
   *
   * 与 complete() 复用同一条判断（validateOutput），差别只在于输入是拼接后的
   * 完整文本。之所以必须做：`response_format=json` 是**契约**的一部分
   * （见 proto 注释），流式路径漏掉校验会让下游拿到半截 JSON 却以为成功
   * （review 抓到的真实缺口）。
   */
  private validateStreamedOutput(req: ModelRequest, fullText: string): void {
    if (!req.json) return;
    this.validateOutput(req, { text: fullText, model: "", usage: emptyUsage(), costUsd: 0 });
  }

  /**
   * 所有非流式调用的公共外壳：开关 -> 调用 -> 计量 -> 错误归一化。
   *
   * 回调返回 [结果, 计量] 而不是只返回结果：这样成功路径的记账也发生在这里，
   * "忘了记计量"与"记错了"都只有一个地方可能出错。做成两段式（先结果后记账）
   * 则会多出一个必须先算 usage 才能拿结果的耦合。
   */
  private async run<T>(
    method: string,
    input: CompleteInput,
    call: (ctx: CallContext) => Promise<[T, UsageOut]>,
  ): Promise<T> {
    const ctx = this.newContext(input);
    try {
      this.ensureEnabled();
      // 超时必须在这里强制，而不是"只交给 Go 侧的 deadline"。
      //
      // 为什么：Go 侧取消时 grpc-go 只是不再等响应，**不会**让 sidecar 停止
      // 等待 provider；一个卡住的 provider 调用会一直占着这里的 promise，
      // 既算不出失败计量，也让"调用超时"在 sidecar 日志里完全看不见。
      // 这里用 race 保证"无论如何都在预算内收尾"，abort 由调用方另外传。
      const [result, usage] = await this.withDeadline(ctx.startedMs, call(ctx));
      this.record(ctx, method, usage, "ok");
      return result;
    } catch (err) {
      const gerr = toGatewayError(err);
      // 失败也要记一条：否则"AI 从来没成功过"这件事在计量里看不出来。
      this.record(ctx, method, { promptTokens: 0, completionTokens: 0, totalTokens: 0, costUsd: 0 }, gerr.code);
      throw gerr;
    }
  }

  /**
   * Embed 走与 complete 完全相同的公共外壳（开关 -> 调用 -> 计量 -> 错误归一化）。
   *
   * 复用而不是另写一份：两条通道的"关闭时不许外呼""失败也要记账"
   * 这些性质必须同时成立，两份实现必然有一份先漂移。
   *
   * 与 complete 的差别只在入参校验与出参形状上。
   */
  async embed(input: EmbedInput): Promise<EmbedOutput> {
    const inputs = this.validateEmbedInputs(input.inputs);
    const startedMs = this.nowMs();
    const model = input.model.trim() === "" ? this.deps.embedding.defaultModel() ?? "" : input.model.trim();
    const meta: CallMeta = {
      requestId: input.requestId,
      provider: this.deps.embedding.servingProviders()[0] ?? "",
      model,
      startedMs,
    };
    try {
      this.ensureEnabled();
      const res = await this.withDeadline(
        startedMs,
        this.deps.embedding.embed({
          inputs,
          model: input.model,
          normalize: input.normalize,
          requestId: input.requestId,
        }),
      );
      if (input.normalize && !res.normalized) {
        // 请求与实现不一致时**不能静默接受**：没归一化的向量在余弦检索里
        // 会让"长度更长的文本"系统性占优，而分数看起来仍然正常。
        throw new GatewayError("invalid_response", "请求要求归一化，但 embedding 层返回了未归一化的向量");
      }
      const out: EmbedOutput = {
        vectors: res.vectors,
        model: res.model,
        dim: res.dim,
        normalized: res.normalized,
        usage: {
          promptTokens: res.usage.promptTokens,
          completionTokens: res.usage.completionTokens,
          totalTokens: res.usage.totalTokens,
          costUsd: res.usage.costUsd,
        },
      };
      this.record(meta, "Embed", out.usage, "ok");
      return out;
    } catch (err) {
      const gerr = toGatewayError(err);
      this.record(meta, "Embed", { promptTokens: 0, completionTokens: 0, totalTokens: 0, costUsd: 0 }, gerr.code);
      throw gerr;
    }
  }

  /**
   * embedding 入参校验。
   *
   * 三条都是"拒绝而不是修正"：
   *   - 空列表：一次什么都不做的调用没有意义，放过去只会白吃一次超时预算；
   *   - 超过上限：gRPC 消息有大小限制，超了会在传输层报一个与原因无关的错；
   *   - 空白文本：provider 对空串的处理各不相同（有的报错、有的返回零向量）。
   *     零向量入库之后与任何东西的余弦都是 0，看起来像"这条内容没有语义"，
   *     而真实原因是调用方传了空串。
   */
  private validateEmbedInputs(inputs: readonly string[]): string[] {
    if (inputs.length === 0) {
      throw new GatewayError("bad_request", "embedding 请求的 inputs 不能为空");
    }
    if (inputs.length > MAX_EMBED_INPUTS) {
      throw new GatewayError(
        "bad_request",
        `embedding 请求一次最多 ${MAX_EMBED_INPUTS} 条文本，实际 ${inputs.length} 条`,
      );
    }
    const out: string[] = [];
    for (let i = 0; i < inputs.length; i++) {
      const text = inputs[i] ?? "";
      if (text.trim() === "") {
        throw new GatewayError("bad_request", `embedding 第 ${i} 条文本为空`);
      }
      out.push(text);
    }
    return out;
  }

  /**
   * 给异步迭代器加一个**整体**的截止时间。
   *
   * 与逐片检查的区别：逐片检查在"provider 不再产出任何东西"时永远不会触发，
   * 而这正是最常见的卡死形态（连接半开、provider 内部排队）。
   * 这里每取下一片都与截止时间赛跑，因此卡住也能在预算内失败。
   *
   * 取消（signal）同样立即结束迭代：客户端已经不想要结果了，
   * 继续等只会白占连接。
   */
  private async *withStreamDeadline(
    source: AsyncIterable<ModelChunk>,
    deadline: number,
    signal?: AbortSignal,
  ): AsyncIterable<ModelChunk> {
    const iterator = source[Symbol.asyncIterator]();
    try {
      for (;;) {
        if (signal?.aborted) {
          throw new GatewayError("timeout", "调用方已取消");
        }

        // 剩余预算是**每一轮重新算**的，这是本函数最容易写错的地方：
        // 第一版把 `deadline - now` 在进入循环前算成常量传进来，
        // 于是每片都拿到同一个初始预算，定时器在"两次取片之间"根本不成立——
        // 实测表现为 provider 卡住 5 秒都没触发超时（而单测里因为第一片
        // 立即返回、后续片很快就到，反而看不出来）。
        const remaining = deadline - this.nowMs();
        if (!Number.isFinite(remaining)) {
          // 没配超时（deadline = +Inf）：不建定时器。
          // setTimeout(Infinity) 会被 Node 降级成 1ms，让"不限时"变成"立刻超时"。
          const next = await iterator.next();
          if (next.done === true) return;
          yield next.value;
          continue;
        }
        if (remaining <= 0) {
          throw new GatewayError("timeout", "超过流式调用的时间预算");
        }

        let timer: NodeJS.Timeout | undefined;
        const timeout = new Promise<never>((_, reject) => {
          timer = setTimeout(() => reject(new GatewayError("timeout", "等待下一片超时")), remaining);
          timer.unref?.();
        });

        let next: IteratorResult<ModelChunk>;
        try {
          next = await Promise.race([iterator.next(), timeout]);
        } finally {
          if (timer !== undefined) clearTimeout(timer);
        }
        if (next.done === true) return;
        yield next.value;
      }
    } finally {
      // 收掉底层迭代器，让 provider 侧的生成循环结束（async generator 的
      // return 会触发它的 finally）。
      //
      // **绝对不要 await 它**：如果 provider 正卡在一个很慢的 await 上，
      // return() 的完成要等那个 await 结束——await 它就会把"超时"变成
      // "等到 provider 自己醒来"，调用方照样被拖住（实测：40ms 的预算，
      // 因为这一行 await，错误直到 5 秒后才到消费者手上）。
      // 超时的意义就是"不再等"，所以这里只发起收尾、不等它完成。
      void iterator.return?.(undefined as never);
    }
  }

  /**
   * 把一次调用限制在超时预算内。
   *
   * 用 Promise.race 而不是 Promise.race + AbortSignal：pi-ai 的 complete()
   * 不接受 AbortSignal，无法真正掐断底层请求；这里能保证的是"调用方在预算内
   * 拿到结果"，被抛弃的那个 promise 由 provider 自己的 HTTP 超时兜底。
   * 这个取舍必须写清楚，否则下一个人会以为超时已经彻底解决了。
   */
  private async withDeadline<T>(startedMs: number, p: Promise<T>): Promise<T> {
    const budget = this.deps.timeoutMs;
    if (budget <= 0) return p;

    let timer: NodeJS.Timeout | undefined;
    try {
      return await Promise.race([
        p,
        new Promise<never>((_, reject) => {
          timer = setTimeout(() => {
            reject(new GatewayError("timeout", `超过 ${budget}ms 的调用预算`));
          }, Math.max(1, this.deadlineFor(startedMs) - this.nowMs()));
          // 不要让这个定时器把进程吊住（Node 里 unref 后才能正常退出）。
          timer.unref?.();
        }),
      ]);
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  /** 记录成功调用并累加指标。run 的成功路径由各方法在拿到 usage 后调用。 */
  private usageOut(res: ModelResult): UsageOut {
    return {
      promptTokens: res.usage.input,
      completionTokens: res.usage.output,
      totalTokens: res.usage.totalTokens,
      costUsd: res.usage.cost.total,
    };
  }

  private usageFromChunk(chunk: ModelChunk): UsageOut {
    if (!chunk.usage) {
      return { promptTokens: 0, completionTokens: 0, totalTokens: 0, costUsd: 0 };
    }
    return {
      promptTokens: chunk.usage.input,
      completionTokens: chunk.usage.output,
      totalTokens: chunk.usage.totalTokens,
      costUsd: chunk.costUsd ?? chunk.usage.cost.total,
    };
  }

  private newContext(input: CompleteInput): CallContext {
    const req = this.toRequest(input);
    const resolved = this.deps.models.resolve(req);
    return {
      req,
      requestId: req.requestId,
      provider: resolved?.provider ?? "",
      model: resolved?.model ?? req.model,
      startedMs: this.nowMs(),
    };
  }

  private ensureEnabled(): void {
    if (!this.deps.enabled) {
      // 关掉时连模型层都不碰：这条判断放在最外层，是为了让"关闭"在
      // 任何 provider 配置状态下都成立。
      throw new GatewayError("disabled", "AI_ENABLED=false，sidecar 不提供模型服务");
    }
  }

  private deadlineFor(startedMs: number): number {
    return this.deps.timeoutMs > 0 ? startedMs + this.deps.timeoutMs : Number.POSITIVE_INFINITY;
  }

  private toRequest(input: CompleteInput): ModelRequest {
    return {
      system: input.system,
      prompt: input.prompt,
      temperature: input.temperature,
      maxTokens: input.maxTokens > 0 ? input.maxTokens : this.deps.defaultMaxTokens,
      json: input.json,
      model: input.model,
      requestId: input.requestId,
    };
  }

  /**
   * 结构化输出校验。
   *
   * 要求 JSON 时，返回体必须是**能解析的对象**。解析失败就报错，而不是
   * 把半截文本交回去：调用方会直接把它入库，脏数据比一次明确的失败难查得多
   * （docs/AI-00 第 4 节）。这里只校验"是 JSON 对象"，不校验业务 schema——
   * 那属于调用方的事，改 schema 不该动 sidecar。
   */
  private validateOutput(req: ModelRequest, res: ModelResult): string {
    if (!req.json) return res.text;
    const trimmed = res.text.trim();
    let parsed: unknown;
    try {
      parsed = JSON.parse(trimmed);
    } catch {
      throw new GatewayError(
        "invalid_response",
        `要求 JSON 输出，但返回体无法解析: ${sanitize(trimmed.slice(0, 200))}`,
      );
    }
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      throw new GatewayError("invalid_response", "要求 JSON 对象，模型返回的不是对象");
    }
    return trimmed;
  }

  private nowMs(): number {
    return (this.deps.now ?? Date.now)();
  }

  private record(ctx: CallMeta, method: string, usage: UsageOut, outcome: string): void {
    if (outcome === "ok") {
      this.deps.metrics.calls += 1;
      this.deps.metrics.totalTokens += usage.totalTokens;
      this.deps.metrics.totalCostUsd += usage.costUsd;
    } else {
      this.deps.metrics.failures += 1;
    }
    const rec: UsageRecord = {
      ts: new Date(this.nowMs()).toISOString(),
      request_id: ctx.requestId,
      method,
      provider: ctx.provider,
      model: ctx.model,
      prompt_tokens: usage.promptTokens,
      completion_tokens: usage.completionTokens,
      total_tokens: usage.totalTokens,
      cost_usd: usage.costUsd,
      latency_ms: Math.round(this.nowMs() - ctx.startedMs),
      outcome,
    };
    this.deps.usage.record(rec);
  }
}

/**
 * 一个全零的 Usage。
 *
 * 只用于"把拼接后的文本塞进 validateOutput"这种只关心 text 的场合：
 * 那时还没有 usage（或者不重新计算），显式给一个零值比编造一个数字清楚。
 */
function emptyUsage(): Usage {
  return {
    input: 0,
    output: 0,
    cacheRead: 0,
    cacheWrite: 0,
    totalTokens: 0,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
  };
}
