/**
 * embedding 层的测试。
 *
 * 与模型层的测试同一条纪律：**不需要网络、不需要 API key**。
 * 这里的每一条断言都对应一类"看起来成功、实际有毒"的响应——
 * 它们在功能上不会报错，只会在检索质量上体现出来，
 * 所以必须在这里被钉死。
 */

import { strict as assert } from "node:assert";
import { test } from "node:test";

import type { EmbeddingConfig } from "./config.ts";
import {
  createEmbeddingLayer,
  hashedLexicalVector,
  lexicalUnits,
  l2Normalize,
  MAX_EMBED_INPUTS,
  type EmbedRequest,
} from "./embeddings.ts";
import { GatewayError } from "./errors.ts";

function cfg(over: Partial<EmbeddingConfig> = {}): EmbeddingConfig {
  return {
    model: "text-embedding-3-small",
    dim: 4,
    normalize: true,
    baseUrl: "https://example.invalid/v1",
    apiKey: "sk-secret-key-value",
    timeoutMs: 1_000,
    ...over,
  };
}

function req(over: Partial<EmbedRequest> = {}): EmbedRequest {
  return { inputs: ["第一条", "第二条"], model: "", normalize: true, requestId: "r1", ...over };
}

/** 记录调用参数的假 fetch。 */
function recordingFetch(
  body: unknown,
  init: { status?: number; text?: string; throwOnCall?: Error } = {},
): { fetchImpl: typeof fetch; calls: Array<{ url: string; init: RequestInit }> } {
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const fetchImpl = (async (url: string | URL | Request, opts?: RequestInit) => {
    calls.push({ url: String(url), init: opts ?? {} });
    if (init.throwOnCall) throw init.throwOnCall;
    const status = init.status ?? 200;
    const payload = init.text ?? JSON.stringify(body);
    return new Response(payload, { status, headers: { "content-type": "application/json" } });
  }) as unknown as typeof fetch;
  return { fetchImpl, calls };
}

function vectorsBody(vectors: number[][], over: { model?: string; total?: number } = {}): unknown {
  return {
    object: "list",
    data: vectors.map((embedding, index) => ({ object: "embedding", index, embedding })),
    model: over.model ?? "text-embedding-3-small",
    usage: { prompt_tokens: 7, total_tokens: 7 },
  };
}

test("未配置凭据时不提供向量服务，且一次外呼都不发生", async () => {
  const { fetchImpl, calls } = recordingFetch(vectorsBody([[1, 2, 3, 4]]));
  const layer = createEmbeddingLayer({ config: cfg({ apiKey: "" }), fetchImpl });

  assert.deepEqual(layer.servingProviders(), [], "没有 key 就不能报告可用");
  await assert.rejects(() => layer.embed(req()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    // provider_unavailable：Go 侧据此"直接降级"而不是当成故障刷告警。
    assert.equal(err.code, "provider_unavailable");
    return true;
  });
  assert.equal(calls.length, 0, "没有 key 时不该发出任何网络请求");
});

test("请求体与鉴权头正确，且 key 不出现在返回值里", async () => {
  const { fetchImpl, calls } = recordingFetch(vectorsBody([[1, 0, 0, 0], [0, 1, 0, 0]]));
  const layer = createEmbeddingLayer({ config: cfg(), fetchImpl });

  const res = await layer.embed(req({ inputs: ["甲", "乙"] }));
  assert.equal(calls.length, 1);
  assert.equal(calls[0]?.url, "https://example.invalid/v1/embeddings");
  const headers = calls[0]?.init.headers as Record<string, string>;
  assert.match(headers.authorization ?? "", /^Bearer /);
  const sent = JSON.parse(String(calls[0]?.init.body)) as Record<string, unknown>;
  assert.deepEqual(sent.input, ["甲", "乙"], "批量输入必须逐条原样传出");
  assert.equal(sent.model, "text-embedding-3-small");
  // 显式要 float：有些端点默认返回 base64，那时解析出来的不是向量而是乱码。
  assert.equal(sent.encoding_format, "float");

  assert.equal(res.dim, 4);
  assert.equal(res.vectors.length, 2);
  assert.ok(!JSON.stringify(res).includes("sk-secret"), "返回值里绝不能出现密钥");
});

test("HTTP 失败时带出的是清洗过的响应体", async () => {
  // 真实 provider 的 4xx 响应体里常常回显 key 片段。
  const { fetchImpl } = recordingFetch(null, {
    status: 401,
    text: '{"error":{"message":"Incorrect API key provided: sk-secret-key-value"}}',
  });
  const layer = createEmbeddingLayer({ config: cfg(), fetchImpl });

  await assert.rejects(() => layer.embed(req()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "provider_error");
    assert.ok(!err.message.includes("sk-secret"), `错误消息里泄漏了密钥: ${err.message}`);
    assert.match(err.message, /401/);
    return true;
  });
});

test("超时被翻译成 timeout 而不是 provider_error", async () => {
  // 一个永不 resolve 的 fetch：只能靠 AbortSignal 结束。
  const fetchImpl = ((_url: unknown, opts?: RequestInit) =>
    new Promise((_resolve, reject) => {
      opts?.signal?.addEventListener("abort", () => reject(new Error("aborted")));
    })) as unknown as typeof fetch;
  const layer = createEmbeddingLayer({ config: cfg({ timeoutMs: 20 }), fetchImpl });

  await assert.rejects(() => layer.embed(req()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    // 分级的意义：超时是常态（端点慢），provider_error 才需要人看。
    assert.equal(err.code, "timeout");
    return true;
  });
});

test("返回条数与输入不一致时拒绝整批", async () => {
  const { fetchImpl } = recordingFetch(vectorsBody([[1, 0, 0, 0]]));
  const layer = createEmbeddingLayer({ config: cfg(), fetchImpl });
  await assert.rejects(() => layer.embed(req({ inputs: ["甲", "乙"] })), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "invalid_response");
    // 错位入库会让文本与向量永久错配，而检索结果看起来只是"不够准"。
    assert.match(err.message, /不一致/);
    return true;
  });
});

test("维度与声明不一致时拒绝（换模型的检测点）", async () => {
  const { fetchImpl } = recordingFetch(vectorsBody([[1, 0, 0]]));
  const layer = createEmbeddingLayer({ config: cfg({ dim: 4 }), fetchImpl });
  await assert.rejects(() => layer.embed(req({ inputs: ["甲"] })), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "invalid_response");
    assert.match(err.message, /维度/);
    return true;
  });
});

test("非数字/NaN 分量被拒绝", async () => {
  const { fetchImpl } = recordingFetch({ data: [{ index: 0, embedding: [1, null, 3, 4] }] });
  const layer = createEmbeddingLayer({ config: cfg(), fetchImpl });
  await assert.rejects(() => layer.embed(req({ inputs: ["甲"] })), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "invalid_response");
    return true;
  });
});

test("按 index 重排，顺序与输入严格对应", async () => {
  const { fetchImpl } = recordingFetch({
    data: [
      { index: 1, embedding: [0, 1, 0, 0] },
      { index: 0, embedding: [1, 0, 0, 0] },
    ],
    usage: { prompt_tokens: 2, total_tokens: 2 },
  });
  const layer = createEmbeddingLayer({ config: cfg({ normalize: false }), fetchImpl });
  const res = await layer.embed(req({ inputs: ["甲", "乙"], normalize: false }));
  assert.deepEqual(res.vectors[0], [1, 0, 0, 0]);
  assert.deepEqual(res.vectors[1], [0, 1, 0, 0]);
});

test("归一化按请求执行并回显行为", async () => {
  const { fetchImpl } = recordingFetch(vectorsBody([[3, 4, 0, 0]]));
  const layer = createEmbeddingLayer({ config: cfg(), fetchImpl });

  const on = await layer.embed(req({ inputs: ["甲"], normalize: true }));
  assert.equal(on.normalized, true);
  const norm = Math.hypot(...(on.vectors[0] ?? []));
  assert.ok(Math.abs(norm - 1) < 1e-9, `归一化后模长应为 1，实际 ${norm}`);

  const { fetchImpl: f2 } = recordingFetch(vectorsBody([[3, 4, 0, 0]]));
  const layer2 = createEmbeddingLayer({ config: cfg(), fetchImpl: f2 });
  const off = await layer2.embed(req({ inputs: ["甲"], normalize: false }));
  assert.equal(off.normalized, false);
  assert.deepEqual(off.vectors[0], [3, 4, 0, 0], "未要求归一化时不得擅自归一化");
});

test("l2Normalize 对零向量不做除零", () => {
  // NaN 入库会毁掉整列检索（与任何向量的余弦都是 NaN）。
  assert.deepEqual(l2Normalize([0, 0, 0]), [0, 0, 0]);
  const v = l2Normalize([0, 3, 4]);
  assert.ok(Math.abs(Math.hypot(...v) - 1) < 1e-12);
});

test("离线演示层是确定性的、维度正确、且词面相近的文本更接近", async () => {
  const layer = createEmbeddingLayer({ config: cfg({ dim: 64 }), faux: true });
  assert.deepEqual(layer.servingProviders(), ["faux"]);

  const a = await layer.embed(req({ inputs: ["Go 并发：goroutine 与 channel"], normalize: true }));
  const b = await layer.embed(req({ inputs: ["Go 并发：goroutine 与 channel"], normalize: true }));
  assert.deepEqual(a.vectors, b.vectors, "同一输入必须得到同一向量（演示要可复现）");
  assert.equal(a.dim, 64);

  const near = await layer.embed(req({ inputs: ["Go 并发：goroutine 调度"], normalize: true }));
  const far = await layer.embed(req({ inputs: ["家庭烘焙：戚风蛋糕"], normalize: true }));
  const sim = (x: number[], y: number[]): number => x.reduce((acc, v, i) => acc + v * (y[i] ?? 0), 0);
  const sNear = sim(a.vectors[0] ?? [], near.vectors[0] ?? []);
  const sFar = sim(a.vectors[0] ?? [], far.vectors[0] ?? []);
  assert.ok(sNear > sFar, `同话题相似度(${sNear}) 应高于跨话题(${sFar})`);
});

test("词法单元：汉字按单字+bigram，英文按整词", () => {
  const units = lexicalUnits("Go 并发");
  assert.ok(units.includes("go"), "英文应小写成一个整词");
  assert.ok(units.includes("并"));
  assert.ok(units.includes("并发"), "汉字应生成相邻 bigram");
  assert.ok(units.includes("发"));
});

test("特征哈希：维度固定且非零", () => {
  const v = hashedLexicalVector("测试文本 with English 123", 32);
  assert.equal(v.length, 32);
  assert.ok(v.some((x) => x !== 0));
});

test("max_tokens 上限与 proto 注释里的 64 保持一致", () => {
  // 两处不一致时，超限的请求会在传输层报一个与原因无关的错。
  assert.equal(MAX_EMBED_INPUTS, 64);
});
