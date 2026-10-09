/**
 * Gateway 策略的测试。
 *
 * 这些用例全部跑在一个**假模型层**上：不联网、不需要 API key。
 * 这正是把"策略"与"pi-ai"分开的回报——总开关、JSON 校验、计量、
 * 错误映射这些最容易写错的判断，都能在 CI 里被确定性覆盖。
 */

import { strict as assert } from "node:assert";
import { test } from "node:test";

import { MAX_EMBED_INPUTS, type EmbeddingLayer, type EmbedResult } from "./embeddings.ts";
import { GatewayError, grpcCodeFor, sanitize } from "./errors.ts";
import { Gateway, type CompleteInput } from "./gateway.ts";
import type { ModelChunk, ModelLayer, ModelRequest, ModelResult } from "./models.ts";
import { createUsageSink, newMetrics, type UsageRecord } from "./observability.ts";

function input(over: Partial<CompleteInput> = {}): CompleteInput {
  return {
    model: "",
    system: "",
    prompt: "给这个视频打标签",
    temperature: 0,
    maxTokens: 0,
    json: false,
    requestId: "req-1",
    ...over,
  };
}

function result(text: string, over: Partial<ModelResult> = {}): ModelResult {
  return {
    text,
    model: "fake-1",
    usage: {
      input: 10,
      output: 5,
      cacheRead: 0,
      cacheWrite: 0,
      totalTokens: 15,
      cost: { input: 0.001, output: 0.002, cacheRead: 0, cacheWrite: 0, total: 0.003 },
    },
    costUsd: 0.003,
    ...over,
  };
}

/** 脚本化的假模型层。 */
function fakeModels(opts: {
  providers?: string[];
  complete?: (req: ModelRequest) => Promise<ModelResult>;
  chunks?: ModelChunk[];
  onStreamRequest?: (req: ModelRequest) => void;
} = {}): ModelLayer & { seen: ModelRequest[] } {
  const seen: ModelRequest[] = [];
  return {
    seen,
    servingProviders: () => opts.providers ?? ["openai"],
    hasModel: () => true,
    defaultModel: () => "fake-1",
    resolve: (req) => ({ provider: "openai", model: req.model || "fake-1" }),
    async complete(req) {
      seen.push(req);
      if (opts.complete) return opts.complete(req);
      return result("ok");
    },
    stream(req, _signal?: AbortSignal) {
      seen.push(req);
      opts.onStreamRequest?.(req);
      const chunks = opts.chunks ?? [{ delta: "你好", done: false }, { delta: "", done: true }];
      return (async function* () {
        for (const c of chunks) yield c;
      })();
    },
  };
}

/**
 * 一个脚本化的假 embedding 层。
 *
 * 与假模型层同样的目的：让"维度不符""条数不符""HTTP 失败""未归一化"
 * 这些分支都能被确定性覆盖，而 CI 不需要网络与 API key。
 */
function fakeEmbedding(over: {
  dim?: number;
  providers?: string[];
  embed?: (req: { inputs: readonly string[]; model: string; normalize: boolean }) => Promise<EmbedResult>;
} = {}): EmbeddingLayer {
  const dim = over.dim ?? 4;
  const model = "fake-embed";
  return {
    servingProviders: () => over.providers ?? ["openai-compatible"],
    defaultModel: () => model,
    dimensionOf: (m) => (m === model || m === "" ? dim : undefined),
    async embed(req) {
      if (over.embed) return over.embed(req);
      return {
        vectors: req.inputs.map((_, i) => Array.from({ length: dim }, (_, j) => (i + j) / dim)),
        model,
        dim,
        normalized: req.normalize,
        usage: { promptTokens: 3, completionTokens: 0, totalTokens: 3, costUsd: 0 },
      };
    },
  };
}

function newGateway(over: {
  enabled?: boolean;
  models?: ModelLayer;
  embedding?: EmbeddingLayer;
  records?: UsageRecord[];
  timeoutMs?: number;
  now?: () => number;
} = {}): { gateway: Gateway; records: UsageRecord[]; metrics: ReturnType<typeof newMetrics> } {
  const records: UsageRecord[] = over.records ?? [];
  const metrics = newMetrics();
  const gateway = new Gateway({
    enabled: over.enabled ?? true,
    models: over.models ?? fakeModels(),
    embedding: over.embedding ?? fakeEmbedding(),
    usage: createUsageSink("", () => {}),
    metrics,
    version: "test",
    defaultMaxTokens: 128,
    timeoutMs: over.timeoutMs ?? 0,
    now: over.now,
  });
  // 把记录同时收集到数组里便于断言。
  const orig = gateway["deps"].usage;
  const wrapped = {
    record(rec: UsageRecord) {
      records.push(rec);
      orig.record(rec);
    },
  };
  (gateway as unknown as { deps: { usage: typeof wrapped } }).deps.usage = wrapped;
  return { gateway, records, metrics };
}

// ---------------------------------------------------------------------------
// 总开关
// ---------------------------------------------------------------------------

test("关闭时不做任何模型调用", async () => {
  const models = fakeModels();
  const { gateway, records, metrics } = newGateway({ enabled: false, models });

  await assert.rejects(() => gateway.complete(input()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "disabled");
    return true;
  });

  assert.equal(models.seen.length, 0, "关闭时连模型层都不该被碰到");
  assert.equal(metrics.calls, 0);
  // 仍然要记一条：否则"AI 从来没成功过"在计量里看不出来。
  assert.equal(records.length, 1);
  assert.equal(records[0]?.outcome, "disabled");
});

test("关闭时流式调用同样被拒绝，且不触碰模型层", async () => {
  const models = fakeModels();
  const { gateway } = newGateway({ enabled: false, models });

  await assert.rejects(
    async () => {
      for await (const _ of gateway.completeStream(input())) {
        assert.fail("关闭时不该产出任何分片");
      }
    },
    (err: unknown) => {
      assert.ok(err instanceof GatewayError);
      assert.equal(err.code, "disabled");
      return true;
    },
  );
  assert.equal(models.seen.length, 0);
});

test("health 在没有 provider 时报告 not serving 而不是报错", () => {
  const { gateway } = newGateway({ enabled: true, models: fakeModels({ providers: [] }) });
  const h = gateway.health();
  assert.equal(h.serving, false);
  // 必须说明原因，否则运维只能看到一个 false。
  assert.match(h.detail, /API key/);
  assert.deepEqual(h.providers, []);
});

test("health 在关闭且无 provider 时优先说明是总开关", () => {
  const { gateway } = newGateway({ enabled: false, models: fakeModels({ providers: [] }) });
  const h = gateway.health();
  assert.equal(h.serving, false);
  assert.match(h.detail, /AI_ENABLED/);
});

// ---------------------------------------------------------------------------
// 结构化输出校验
// ---------------------------------------------------------------------------

test("要求 JSON 时非法输出被拒绝", async () => {
  const cases: Array<[string, string]> = [
    ["纯文本", "这不是 JSON"],
    ["代码块包裹", "```json\n{\"a\":1}\n```"],
    ["数组", "[1,2,3]"],
    ["null", "null"],
    ["截断的 JSON", "{\"a\":"],
  ];
  for (const [name, text] of cases) {
    const { gateway } = newGateway({
      models: fakeModels({ complete: async () => result(text) }),
    });
    await assert.rejects(
      () => gateway.complete(input({ json: true })),
      (err: unknown) => {
        assert.ok(err instanceof GatewayError, `${name}: 应抛 GatewayError`);
        assert.equal(err.code, "invalid_response", `${name}: 错误码`);
        return true;
      },
      `${name} 应当被拒绝`,
    );
  }
});

test("要求 JSON 时合法对象被接受（并去掉首尾空白）", async () => {
  const { gateway } = newGateway({
    models: fakeModels({ complete: async () => result('  {"tags":["猫"]}  ') }),
  });
  const out = await gateway.complete(input({ json: true }));
  assert.equal(out.text, '{"tags":["猫"]}');
});

test("不要求 JSON 时原样返回，不做任何解析", async () => {
  const raw = "这是一段普通文本 {\"不是\": 完整 JSON";
  const { gateway } = newGateway({ models: fakeModels({ complete: async () => result(raw) }) });
  const out = await gateway.complete(input({ json: false }));
  assert.equal(out.text, raw);
});

// ---------------------------------------------------------------------------
// 参数传递与默认值
// ---------------------------------------------------------------------------

test("maxTokens 未指定时用 sidecar 的默认值", async () => {
  const models = fakeModels();
  const { gateway } = newGateway({ models });
  await gateway.complete(input({ maxTokens: 0 }));
  assert.equal(models.seen[0]?.maxTokens, 128, "0 表示未指定，应填默认值");

  await gateway.complete(input({ maxTokens: 32 }));
  assert.equal(models.seen[1]?.maxTokens, 32, "显式值必须优先");
});

test("temperature=0 不传给模型层（0 表示未指定）", async () => {
  const models = fakeModels();
  const { gateway } = newGateway({ models });
  await gateway.complete(input({ temperature: 0 }));
  assert.equal(models.seen[0]?.temperature, 0);
  await gateway.complete(input({ temperature: 0.7 }));
  assert.equal(models.seen[1]?.temperature, 0.7);
});

// ---------------------------------------------------------------------------
// 计量
// ---------------------------------------------------------------------------

test("成功调用记录 token 与成本", async () => {
  const { gateway, records, metrics } = newGateway();
  const out = await gateway.complete(input());

  assert.equal(out.usage.totalTokens, 15);
  assert.equal(out.usage.promptTokens, 10);
  assert.equal(out.usage.completionTokens, 5);
  assert.equal(out.usage.costUsd, 0.003);

  assert.equal(records.length, 1);
  const rec = records[0]!;
  assert.equal(rec.outcome, "ok");
  assert.equal(rec.total_tokens, 15);
  assert.equal(rec.cost_usd, 0.003);
  assert.equal(rec.request_id, "req-1");
  assert.ok(rec.latency_ms >= 0);

  assert.equal(metrics.calls, 1);
  assert.equal(metrics.totalTokens, 15);
  assert.ok(Math.abs(metrics.totalCostUsd - 0.003) < 1e-9);
});

test("失败调用也记录，但计入 failures 而不是 calls", async () => {
  const { gateway, records, metrics } = newGateway({
    models: fakeModels({
      complete: async () => {
        throw new Error("provider 502");
      },
    }),
  });
  await assert.rejects(() => gateway.complete(input()));
  assert.equal(metrics.calls, 0);
  assert.equal(metrics.failures, 1);
  assert.equal(records.length, 1);
  assert.equal(records[0]?.outcome, "provider_error");
});

test("流式调用在 done 分片时记录计量", async () => {
  const { gateway, records, metrics } = newGateway({
    models: fakeModels({
      chunks: [
        { delta: "你", done: false },
        { delta: "好", done: false },
        {
          delta: "",
          done: true,
          usage: {
            input: 3,
            output: 2,
            cacheRead: 0,
            cacheWrite: 0,
            totalTokens: 5,
            cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0.0001 },
          },
          costUsd: 0.0001,
        },
      ],
    }),
  });

  const deltas: string[] = [];
  for await (const chunk of gateway.completeStream(input())) {
    if (!chunk.done) deltas.push(chunk.delta);
  }
  assert.deepEqual(deltas, ["你", "好"]);
  assert.equal(metrics.calls, 1);
  assert.equal(metrics.totalTokens, 5);
  assert.equal(records.at(-1)?.outcome, "ok");
});

test("流式超时按分片检查并记一次失败", async () => {
  let clock = 0;
  const { gateway, metrics } = newGateway({
    timeoutMs: 1000,
    now: () => clock,
    models: fakeModels({
      chunks: [
        { delta: "a", done: false },
        { delta: "b", done: false },
      ],
      onStreamRequest: () => {
        // 第一次检查时已超预算。
        clock = 5000;
      },
    }),
  });

  await assert.rejects(
    async () => {
      for await (const _ of gateway.completeStream(input())) {
        /* 消费直到抛出 */
      }
    },
    (err: unknown) => {
      assert.ok(err instanceof GatewayError);
      assert.equal(err.code, "timeout");
      return true;
    },
  );
  assert.equal(metrics.failures, 1);
  assert.equal(metrics.calls, 0);
});

// ---------------------------------------------------------------------------
// 错误映射与密钥清洗
// ---------------------------------------------------------------------------

test("错误码到 gRPC 状态码的映射是稳定的", () => {
  // 这些数字是 Go 侧判断降级方式的依据，改动等于改协议。
  assert.equal(grpcCodeFor("disabled"), 9); // FAILED_PRECONDITION
  assert.equal(grpcCodeFor("provider_unavailable"), 14); // UNAVAILABLE
  assert.equal(grpcCodeFor("model_not_found"), 5); // NOT_FOUND
  assert.equal(grpcCodeFor("timeout"), 4); // DEADLINE_EXCEEDED
  assert.equal(grpcCodeFor("bad_request"), 3); // INVALID_ARGUMENT
  assert.equal(grpcCodeFor("provider_error"), 2); // UNKNOWN
});

test("provider 错误文本会被清洗掉形似密钥的片段", async () => {
  const secret = "sk-abcdefghijklmnopqrstuvwxyz";
  const { gateway } = newGateway({
    models: fakeModels({
      complete: async () => {
        throw new Error(`401 unauthorized: api key ${secret} rejected`);
      },
    }),
  });

  await assert.rejects(() => gateway.complete(input()), (err: unknown) => {
    assert.ok(err instanceof Error);
    assert.ok(!err.message.includes(secret), `错误信息里泄漏了密钥: ${err.message}`);
    assert.match(err.message, /\[redacted\]/);
    return true;
  });
});

test("sanitize 覆盖常见的密钥形态", () => {
  assert.equal(sanitize("Bearer sk-1234567890abcdef"), "Bearer [redacted]");
  assert.ok(!sanitize("key-abcdefgh12345678").includes("abcdefgh12345678"));
  // 普通文本不该被误伤。
  assert.equal(sanitize("model gpt-4o-mini not found"), "model gpt-4o-mini not found");
});

test("非法 JSON 的错误消息里不会带上整段模型输出", async () => {
  const huge = "x".repeat(5000) + "not-json";
  const { gateway } = newGateway({ models: fakeModels({ complete: async () => result(huge) }) });
  await assert.rejects(() => gateway.complete(input({ json: true })), (err: unknown) => {
    assert.ok(err instanceof Error);
    assert.ok(err.message.length < 500, `错误消息过长，可能带上了整段输出: ${err.message.length}`);
    return true;
  });
});

// ---------------------------------------------------------------------------
// review 抓到的回归点：这些用例存在的唯一目的就是"别再犯"
// ---------------------------------------------------------------------------

test("GatewayError 也要被清洗（provider 文本会带 key）", async () => {
  // 这是 review 实测到的真实泄漏路径：models.ts 把 provider SDK 的
  // errorMessage 直接塞进 GatewayError，而旧的 toGatewayError 对
  // "已经是 GatewayError" 的分支直接 return，sanitize 被整条跳过，
  // 于是 sk-proj-… 经由 gRPC status 的 message/details 进了 Go 日志。
  const { gateway } = newGateway({
    models: fakeModels({
      complete: async () => {
        throw new GatewayError(
          "provider_error",
          "openai 401 Incorrect API key provided: sk-proj-ZZZZYYYYXXXXWWWWVVVVUUUU",
        );
      },
    }),
  });

  await assert.rejects(() => gateway.complete(input()), (err: unknown) => {
    assert.ok(err instanceof Error);
    assert.ok(
      !err.message.includes("sk-proj-ZZZZYYYYXXXXWWWWVVVVUUUU"),
      `GatewayError 分支绕过了清洗，密钥泄漏: ${err.message}`,
    );
    return true;
  });
});

test("Complete 受 timeoutMs 约束（不是只靠调用方的 deadline）", async () => {
  // 旧实现里 timeoutMs 只对流式的逐片检查生效，unary 调用完全不受约束：
  // 一个卡住的 provider 会让 handler 永远挂着（实测 20ms 预算跑了 301ms 才返回）。
  const { gateway, metrics } = newGateway({
    timeoutMs: 30,
    models: fakeModels({
      complete: () => new Promise<ModelResult>(() => {
        /* 永不 resolve：模拟 provider 卡死 */
      }),
    }),
  });

  const start = Date.now();
  await assert.rejects(() => gateway.complete(input()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "timeout");
    return true;
  });
  assert.ok(Date.now() - start < 2000, "应当在预算附近返回，而不是一直挂着");
  assert.equal(metrics.failures, 1);
});

test("流式在 done 之后不再产出分片", async () => {
  // 协议上 done 是终止帧。若 provider 之后还吐分片而我们照转，
  // 下游的拼接逻辑会拿到 done 之后的数据。
  const { gateway } = newGateway({
    models: fakeModels({
      chunks: [
        { delta: "a", done: false },
        { delta: "", done: true },
        { delta: "不该出现", done: false },
      ],
    }),
  });

  const deltas: string[] = [];
  let sawDone = false;
  for await (const c of gateway.completeStream(input())) {
    if (c.done) {
      sawDone = true;
      continue;
    }
    assert.ok(!sawDone, "done 之后不该再收到分片");
    deltas.push(c.delta);
  }
  assert.deepEqual(deltas, ["a"]);
});

test("流式没有终止帧时按失败处理（而不是静默成功）", async () => {
  // "迭代结束但没有 done" 是协议违规。当成成功的话：计量不会记账、
  // 下游永远等不到终止帧，而两边日志都显示一切正常。
  const { gateway, metrics, records } = newGateway({
    models: fakeModels({ chunks: [{ delta: "半截", done: false }] }),
  });

  await assert.rejects(
    async () => {
      for await (const _ of gateway.completeStream(input())) {
        /* 消费到结束 */
      }
    },
    (err: unknown) => {
      assert.ok(err instanceof GatewayError);
      assert.equal(err.code, "invalid_response");
      return true;
    },
  );
  assert.equal(metrics.calls, 0);
  assert.equal(records.at(-1)?.outcome, "invalid_response");
});

test("流式请求 JSON 时，非法 JSON 在终止时被拒绝", async () => {
  // 旧实现只在 unary 路径校验 JSON，流式路径完全不校验（rule 4 的缺口）。
  const { gateway } = newGateway({
    models: fakeModels({
      chunks: [
        { delta: "这不是", done: false },
        { delta: " JSON", done: false },
        { delta: "", done: true },
      ],
    }),
  });

  await assert.rejects(
    async () => {
      for await (const _ of gateway.completeStream(input({ json: true }))) {
        /* 消费到终止 */
      }
    },
    (err: unknown) => {
      assert.ok(err instanceof GatewayError);
      assert.equal(err.code, "invalid_response");
      return true;
    },
  );
});

test("流式请求 JSON 且输出合法时正常结束", async () => {
  const { gateway, metrics } = newGateway({
    models: fakeModels({
      chunks: [{ delta: '{"ok":', done: false }, { delta: "true}", done: false }, { delta: "", done: true }],
    }),
  });
  let done = false;
  for await (const c of gateway.completeStream(input({ json: true }))) {
    if (c.done) done = true;
  }
  assert.ok(done);
  assert.equal(metrics.calls, 1);
});

test("取消信号会传给模型层（否则 provider 继续生成、继续计费）", async () => {
  let received: AbortSignal | undefined;
  const models = fakeModels();
  const original = models.stream.bind(models);
  models.stream = (req, signal) => {
    received = signal;
    return original(req, signal);
  };
  const { gateway } = newGateway({ models });

  const controller = new AbortController();
  for await (const _ of gateway.completeStream(input(), controller.signal)) {
    /* 消费完 */
  }
  assert.ok(received, "模型层必须收到 signal");
  assert.equal(received, controller.signal);
});

test("流式在 provider 卡住时也会在预算内失败（不是只检查已到达的分片）", async () => {
  // review 实测的形态：provider 给出一片后长时间不再产出任何东西。
  // 旧的"每片检查时间"逻辑永远等不到下一片，流就无限挂着了。
  //
  // 这里用 setTimeout 模拟"很久之后才来下一片"（而不是永不 resolve 的 promise）：
  // 永不 resolve 会让测试进程结束后仍留着一个悬挂等待，
  // 表现是"用例全绿但进程不退出"——上一版就是这么把 npm test 挂住的。
  const models = fakeModels();
  models.stream = () =>
    (async function* () {
      yield { delta: "只有一片", done: false };
      await new Promise((resolve) => setTimeout(resolve, 5000));
      yield { delta: "", done: true };
    })();

  const { gateway } = newGateway({ models, timeoutMs: 40 });

  const start = Date.now();
  await assert.rejects(
    async () => {
      for await (const _ of gateway.completeStream(input())) {
        /* 消费到失败 */
      }
    },
    (err: unknown) => {
      assert.ok(err instanceof GatewayError);
      assert.equal(err.code, "timeout");
      return true;
    },
  );
  assert.ok(Date.now() - start < 3000, "应在预算附近返回，而不是无限挂着");
});

// ---------------------------------------------------------------------------
// Embed（P2 的向量通道）
// ---------------------------------------------------------------------------

function embedInput(over: Partial<import("./gateway.ts").EmbedInput> = {}) {
  return { inputs: ["甲", "乙"], model: "", normalize: true, requestId: "r1", ...over };
}

/** 一个记录调用的假 embedding 层；用来断言"关闭时一次都不碰"。 */
function spyingEmbedding(over: { result?: Partial<EmbedResult> } = {}) {
  const calls: Array<{ inputs: readonly string[]; normalize: boolean }> = [];
  const layer = fakeEmbedding({
    dim: 4,
    embed: async (req) => {
      calls.push({ inputs: req.inputs, normalize: req.normalize });
      return {
        vectors: req.inputs.map(() => [1, 0, 0, 0]),
        model: "fake-embed",
        dim: 4,
        normalized: req.normalize,
        usage: { promptTokens: 1, completionTokens: 0, totalTokens: 1, costUsd: 0 },
        ...over.result,
      };
    },
  });
  return { layer, calls };
}

test("总开关关闭时 embed 一次都不碰 embedding 层", async () => {
  const { layer, calls } = spyingEmbedding();
  const { gateway } = newGateway({ enabled: false, embedding: layer });

  await assert.rejects(() => gateway.embed(embedInput()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    // disabled 而不是 provider_unavailable：前者是"配置决定的正常状态"，
    // Go 侧据此静默降级，不记告警。
    assert.equal(err.code, "disabled");
    return true;
  });
  assert.equal(calls.length, 0, "关闭状态下不该有任何 embedding 调用");
});

test("embed 入参校验：空列表/超限/空白条目都被拒绝且不外呼", async () => {
  const { layer, calls } = spyingEmbedding();
  const { gateway } = newGateway({ embedding: layer });

  const cases: Array<[string, string[]]> = [
    ["空列表", []],
    ["超过上限", Array.from({ length: MAX_EMBED_INPUTS + 1 }, (_, i) => `第${i}条`)],
    ["空白条目", ["正常", "   "]],
  ];
  for (const [name, inputs] of cases) {
    await assert.rejects(() => gateway.embed(embedInput({ inputs })), (err: unknown) => {
      assert.ok(err instanceof GatewayError, name);
      assert.equal(err.code, "bad_request", name);
      return true;
    }, name);
  }
  assert.equal(calls.length, 0, "入参不合法时不该发出任何调用");
});

test("请求要求归一化但 embedding 层返回未归一化 → 整批失败", async () => {
  // 没归一化的向量会让长文本在余弦检索里系统性占优，
  // 而分数看起来仍然正常——所以必须在这里响亮地失败。
  const { layer } = spyingEmbedding({ result: { normalized: false } });
  const { gateway } = newGateway({ embedding: layer });

  await assert.rejects(() => gateway.embed(embedInput({ normalize: true })), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "invalid_response");
    return true;
  });
});

test("embed 成功与失败都记进计量", async () => {
  const { layer } = spyingEmbedding();
  const { gateway, records, metrics } = newGateway({ embedding: layer });

  const out = await gateway.embed(embedInput());
  assert.equal(out.dim, 4);
  assert.equal(out.vectors.length, 2);
  const ok = records.find((r) => r.method === "Embed");
  assert.ok(ok, "成功路径必须记账，否则'向量到底用了多少'无从核对");
  assert.equal(ok?.outcome, "ok");
  assert.equal(metrics.calls, 1);

  // 失败路径也要记一条：否则"embedding 从来没成功过"在计量里看不出来。
  const bad = newGateway({
    embedding: fakeEmbedding({
      embed: async () => {
        throw new GatewayError("provider_error", "端点挂了");
      },
    }),
  });
  await assert.rejects(() => bad.gateway.embed(embedInput()));
  assert.ok(bad.records.some((r) => r.method === "Embed" && r.outcome === "provider_error"));
  assert.equal(bad.metrics.failures, 1);
});

test("embed 走的是与 complete 相同的超时预算", async () => {
  const never = fakeEmbedding({
    embed: () => new Promise<EmbedResult>(() => {}),
  });
  const { gateway } = newGateway({ embedding: never, timeoutMs: 40 });
  await assert.rejects(() => gateway.embed(embedInput()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "timeout");
    return true;
  });
});
