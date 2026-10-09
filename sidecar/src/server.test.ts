/**
 * 端到端测试：真实 gRPC 客户端 -> 真实 proto 文件 -> 真实 handler -> 真实模型层。
 *
 * 为什么必须有这一层：
 *   单测可以证明 Gateway 的策略对，但证明不了"注册到 gRPC 上的方法名对不对、
 *   字段名对不对、错误码能不能穿过连接"。这类问题在部署时的表现是
 *   UNIMPLEMENTED 或字段全空，而排查成本很高——因为看起来"服务起来了"。
 *
 * 这里用 pi-ai 的 faux provider 替掉真实模型，因此 CI 不需要 API key、不需要网络，
 * 而除"模型本身"之外的所有环节都是真的（真的 socket、真的 protobuf 编解码）。
 */

import { strict as assert } from "node:assert";
import { test } from "node:test";

import {
  credentials,
  loadPackageDefinition,
  status as GrpcStatus,
  Client as GrpcClient,
  type ServiceError,
} from "@grpc/grpc-js";
import { createRequire } from "node:module";

import { fauxAssistantMessage, fauxProvider, createModels } from "@earendil-works/pi-ai";

import { loadConfig, type SidecarConfig } from "./config.ts";
import { createEmbeddingLayer } from "./embeddings.ts";
import { startServer } from "./server.ts";

const require = createRequire(import.meta.url);

/** 与 server.ts 相同的加载方式，保证测试客户端与服务的定义来自同一个 proto 文件。 */
function modelGatewayClientCtor(protoPath: string): new (addr: string, creds: unknown) => unknown {
  const loader = require("@grpc/proto-loader") as {
    loadSync(path: string, options: Record<string, unknown>): unknown;
  };
  const def = loader.loadSync(protoPath, { keepCase: true, longs: String, enums: String, defaults: true, oneofs: true });
  const pkg = loadPackageDefinition(def as never) as unknown as {
    ai: { v1: { ModelGateway: new (addr: string, creds: unknown) => unknown } };
  };
  return pkg.ai.v1.ModelGateway;
}

interface GatewayCaller {
  health(req: Record<string, never>, cb: (err: ServiceError | null, res: HealthResponse) => void): void;
  complete(req: Record<string, unknown>, cb: (err: ServiceError | null, res: CompleteResponse) => void): void;
  completeStream(req: Record<string, unknown>): NodeJS.EventEmitter & { cancel(): void };
  embed(req: Record<string, unknown>, cb: (err: ServiceError | null, res: EmbedResponse) => void): void;
  close(): void;
}

interface EmbedResponse {
  vectors: Array<{ values: number[] }>;
  model: string;
  dim: number;
  normalized: boolean;
  usage: { prompt_tokens: number; total_tokens: number; cost_usd: number };
}

interface HealthResponse {
  serving: boolean;
  version: string;
  providers: string[];
  detail: string;
}

interface CompleteResponse {
  text: string;
  model: string;
  usage: { prompt_tokens: number; completion_tokens: number; total_tokens: number; cost_usd: number };
}

interface CompleteChunk {
  delta: string;
  done: boolean;
  usage: { total_tokens: number };
}

function testConfig(over: Partial<SidecarConfig> = {}): SidecarConfig {
  return {
    enabled: true,
    // 端口 0 让内核分配一个空闲端口：测试之间不会抢端口，也不会与开发环境冲突。
    listenAddr: "127.0.0.1:0",
    defaultModel: "",
    maxTokens: 64,
    protoPath: loadConfig({}).protoPath,
    usageLogPath: "",
    fauxProvider: false,
    fauxResponse: "",
    fauxResponses: [],
    embedding: {
      model: "text-embedding-3-small",
      dim: 8,
      normalize: true,
      baseUrl: "http://127.0.0.1:0/v1",
      apiKey: "",
      timeoutMs: 1_000,
    },
    ...over,
  };
}

async function startAndConnect(cfg: SidecarConfig, withFaux: boolean) {
  const faux = fauxProvider();
  // withFaux=true 时注入一个装好 faux 的集合（常规测试用）；
  // false 时**不注入**——让 startServer 自己按配置决定 provider，
  // 这正是演示模式（AI_PROVIDER=faux）要覆盖的路径。
  const models = createModels();
  if (withFaux) models.setProvider(faux.provider);

  const started = withFaux
    ? await startServer(cfg, { models })
    : await startServer(cfg);
  const Ctor = modelGatewayClientCtor(cfg.protoPath);
  const client = new Ctor(`127.0.0.1:${started.port}`, credentials.createInsecure()) as GatewayCaller;
  return { started, client, faux, models };
}

test("Health 通过真实 gRPC 连接可读，且服务定义与 proto 一致", async () => {
  const { started, client } = await startAndConnect(testConfig({ enabled: false }), false);
  try {
    const res = await new Promise<HealthResponse>((resolve, reject) => {
      client.health({}, (err, r) => (err ? reject(err) : resolve(r)));
    });
    assert.equal(res.serving, false);
    assert.ok(res.version.length > 0, "version 必须回填");
    assert.match(res.detail, /AI_ENABLED/, "关闭时必须说明原因");
    assert.deepEqual(res.providers, []);
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("总开关关闭时 Complete 返回 FAILED_PRECONDITION（而不是 UNAVAILABLE）", async () => {
  const { started, client } = await startAndConnect(testConfig({ enabled: false }), true);
  try {
    const err = await new Promise<ServiceError | null>((resolve) => {
      client.complete({ prompt: "x" }, (e) => resolve(e));
    });
    assert.ok(err, "关闭时必须报错");
    // 这两个码在 Go 侧的处理完全不同：FAILED_PRECONDITION = 配置决定的正常状态，
    // UNAVAILABLE = 依赖不可用。混用会让日志分级整体错位。
    assert.equal(err.code, GrpcStatus.FAILED_PRECONDITION);
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("没有任何 provider 时 Complete 返回 UNAVAILABLE", async () => {
  const { started, client } = await startAndConnect(testConfig({ enabled: true }), false);
  try {
    const err = await new Promise<ServiceError | null>((resolve) => {
      client.complete({ prompt: "x" }, (e) => resolve(e));
    });
    assert.ok(err);
    assert.equal(err.code, GrpcStatus.UNAVAILABLE);
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("开启且有模型时 Complete 返回文本与计量（字段名与 proto 一致）", async () => {
  const { started, client, faux } = await startAndConnect(testConfig(), true);
  try {
    faux.setResponses([fauxAssistantMessage('{"tags":["猫"]}')]);

    const res = await new Promise<CompleteResponse>((resolve, reject) => {
      client.complete(
        { prompt: "给这个视频打标签", response_format: "json", request_id: "e2e-1", max_tokens: 32 },
        (err, r) => (err ? reject(err) : resolve(r)),
      );
    });

    assert.equal(res.text, '{"tags":["猫"]}');
    assert.ok(res.model.length > 0);
    // 字段名用 proto 里的下划线形式（keepCase），这是 Go 侧读到的同一组名字。
    assert.ok(res.usage.total_tokens > 0, "计量必须穿过连接");
    assert.equal(typeof res.usage.prompt_tokens, "number");
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("要求 JSON 但模型返回非 JSON 时返回 UNKNOWN（provider_error/invalid_response 档）", async () => {
  const { started, client, faux } = await startAndConnect(testConfig(), true);
  try {
    faux.setResponses([fauxAssistantMessage("这不是 JSON")]);
    const err = await new Promise<ServiceError | null>((resolve) => {
      client.complete({ prompt: "x", response_format: "json" }, (e) => resolve(e));
    });
    assert.ok(err, "非法 JSON 必须被拒绝（脏数据比失败更难查）");
    assert.notEqual(err.code, GrpcStatus.OK);
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("CompleteStream 通过真实连接逐片返回，最后一帧 done=true 且带计量", async () => {
  const { started, client, faux } = await startAndConnect(testConfig(), true);
  try {
    faux.setResponses([fauxAssistantMessage("第一段第二段")]);

    const chunks: CompleteChunk[] = [];
    await new Promise<void>((resolve, reject) => {
      const call = client.completeStream({ prompt: "写摘要" });
      call.on("data", (c: CompleteChunk) => chunks.push(c));
      call.on("end", () => resolve());
      call.on("error", (e: ServiceError) => reject(e));
    });

    const text = chunks.map((c) => c.delta).join("");
    assert.equal(text, "第一段第二段");
    const last = chunks.at(-1);
    assert.equal(last?.done, true, "最后一帧必须是 done");
    assert.ok((last?.usage.total_tokens ?? 0) > 0, "done 帧必须带计量");
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("演示模式（AI_PROVIDER=faux）能用脚本化输出跑通完整链路", async () => {
  // 这条用例存在的理由：P1 的演示闭环（发布 -> 打标 -> 按标签查）必须有一条
  // 不依赖真实 API key 的验证路径。演示模式就是那条路径。
  const cfg = testConfig({
    fauxProvider: true,
    fauxResponse: '{"summary":"离线摘要","tags":[{"name":"演示标签","confidence":0.9}]}',
  });
  const { started, client } = await startAndConnect(cfg, false);
  try {
    const health = await new Promise<HealthResponse>((resolve, reject) => {
      client.health({}, (err, r) => (err ? reject(err) : resolve(r)));
    });
    // 演示模式也必须报告 serving=true，否则 Go 侧会直接降级、闭环跑不起来。
    assert.equal(health.serving, true);
    assert.deepEqual(health.providers, ["faux"]);

    const res = await new Promise<CompleteResponse>((resolve, reject) => {
      client.complete({ prompt: "x", response_format: "json" }, (err, r) => (err ? reject(err) : resolve(r)));
    });
    assert.equal(res.text, cfg.fauxResponse);
    assert.ok(res.usage.total_tokens >= 0);
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("默认不进入演示模式（不设置 AI_PROVIDER 时不注册 faux）", async () => {
  // 安全边界：演示模式必须显式开启。默认情况下"没有 key"就等于"不提供服务"。
  const { started, client } = await startAndConnect(testConfig(), false);
  try {
    const health = await new Promise<HealthResponse>((resolve, reject) => {
      client.health({}, (err, r) => (err ? reject(err) : resolve(r)));
    });
    assert.equal(health.serving, false);
    assert.deepEqual(health.providers, []);
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("Embed 通过真实 gRPC 连接可用，字段与 proto 逐字对应", async () => {
  // 这一层要证明的是"注册到 gRPC 上的方法名与字段名对不对"：
  // 写错时部署表现为 UNIMPLEMENTED 或字段全空，而看起来"服务起来了"。
  const cfg = testConfig({ fauxProvider: true });
  const started = await startServer(cfg, {
    embedding: createEmbeddingLayer({ config: cfg.embedding, faux: true }),
  });
  const Ctor = modelGatewayClientCtor(cfg.protoPath);
  const client = new Ctor(`127.0.0.1:${started.port}`, credentials.createInsecure()) as GatewayCaller;
  try {
    const res = await new Promise<EmbedResponse>((resolve, reject) => {
      client.embed({ inputs: ["第一条", "第二条"], normalize: true }, (err, r) =>
        err ? reject(err) : resolve(r),
      );
    });
    assert.equal(res.vectors.length, 2, "返回条数必须与输入一致");
    assert.equal(res.dim, cfg.embedding.dim);
    assert.equal(res.normalized, true);
    for (const v of res.vectors) {
      assert.equal(v.values.length, cfg.embedding.dim);
      const norm = Math.hypot(...v.values);
      assert.ok(Math.abs(norm - 1) < 1e-5, `要求归一化时模长应为 1，实际 ${norm}`);
    }
    assert.ok(res.model.length > 0, "model 必须回填");
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("Embed 的入参错误以 INVALID_ARGUMENT 穿过 gRPC", async () => {
  const cfg = testConfig({ fauxProvider: true });
  const started = await startServer(cfg, {
    embedding: createEmbeddingLayer({ config: cfg.embedding, faux: true }),
  });
  const Ctor = modelGatewayClientCtor(cfg.protoPath);
  const client = new Ctor(`127.0.0.1:${started.port}`, credentials.createInsecure()) as GatewayCaller;
  try {
    const err = await new Promise<ServiceError | null>((resolve) => {
      client.embed({ inputs: [] }, (e) => resolve(e));
    });
    assert.ok(err, "空 inputs 必须失败");
    assert.equal(err?.code, GrpcStatus.INVALID_ARGUMENT);
    // 错误消息必须是清洗过的短语，不能带请求原文之外的任何东西。
    assert.ok(!String(err?.details).includes("sk-"), "错误里不能出现密钥痕迹");
  } finally {
    client.close();
    await started.shutdown();
  }
});

test("未配置 embedding 凭据时 Embed 报 UNAVAILABLE 而不是崩溃", async () => {
  // 这是最常见的部署状态：chat 有 key、embedding 没配。
  // Go 侧据此静默关闭语义路，Feed 必须照常返回。
  const cfg = testConfig();
  const started = await startServer(cfg);
  const Ctor = modelGatewayClientCtor(cfg.protoPath);
  const client = new Ctor(`127.0.0.1:${started.port}`, credentials.createInsecure()) as GatewayCaller;
  try {
    const err = await new Promise<ServiceError | null>((resolve) => {
      client.embed({ inputs: ["甲"] }, (e) => resolve(e));
    });
    assert.ok(err, "没有凭据时必须失败");
    // provider_unavailable -> UNAVAILABLE：与"sidecar 不在"同一个码，
    // 两者的处置相同（降级 + 一条 Warn），不需要区分。
    assert.equal(err?.code, GrpcStatus.UNAVAILABLE);
  } finally {
    client.close();
    await started.shutdown();
  }
});
