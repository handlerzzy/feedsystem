/**
 * 模型层的测试。
 *
 * 关键点：**不需要 API key、不需要网络**。
 * pi-ai 的模型目录是静态内置的，没有 key 时 getAuth 返回 undefined，
 * 于是"没配置 provider"这条路径可以被确定性地测出来——
 * 而这正是 sidecar 最常见的部署状态。
 */

import { strict as assert } from "node:assert";
import { test } from "node:test";

import { fauxAssistantMessage, fauxProvider } from "@earendil-works/pi-ai";

import { GatewayError } from "./errors.ts";
import { createModelLayer, JSON_OUTPUT_INSTRUCTION, type ModelLayer, type ModelRequest } from "./models.ts";

function req(over: Partial<ModelRequest> = {}): ModelRequest {
  return {
    system: "",
    prompt: "给这个视频打标签",
    temperature: 0,
    maxTokens: 0,
    json: false,
    model: "",
    requestId: "req-1",
    ...over,
  };
}

test("没有任何 provider 时构造仍然成功（不发起任何请求）", async () => {
  // 传空列表：模拟"一个 key 都没配"。
  const layer = await createModelLayer({ providerIds: [] });
  assert.deepEqual(layer.servingProviders(), []);
  assert.equal(layer.defaultModel(), undefined);

  await assert.rejects(() => layer.complete(req()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    // provider_unavailable 而不是 disabled：前者是"缺配置"，
    // 后者是"总开关关闭"，Go 侧对两者的日志级别不同。
    assert.equal(err.code, "provider_unavailable");
    return true;
  });
});

test("未配置 key 的真实 provider 不会进入可用列表", async () => {
  // openai provider 会去读 OPENAI_API_KEY；测试环境里通常没有。
  const saved = process.env.OPENAI_API_KEY;
  delete process.env.OPENAI_API_KEY;
  try {
    const layer = await createModelLayer({ providerIds: ["openai"] });
    // 关键断言：注册了 provider，但它没有凭据 → 不应该被认为可用。
    // 这条判断不发起任何网络请求（pi-ai 的 getAuth 是本地解析）。
    assert.deepEqual(layer.servingProviders(), []);
  } finally {
    if (saved !== undefined) process.env.OPENAI_API_KEY = saved;
  }
});

test("有 key 时 provider 进入可用列表", async () => {
  const saved = process.env.OPENAI_API_KEY;
  process.env.OPENAI_API_KEY = "sk-not-a-real-key";
  try {
    const layer = await createModelLayer({ providerIds: ["openai"] });
    assert.deepEqual(layer.servingProviders(), ["openai"]);
  } finally {
    if (saved === undefined) delete process.env.OPENAI_API_KEY;
    else process.env.OPENAI_API_KEY = saved;
  }
});

/**
 * 一个不走网络的端到端用例：把 pi-ai 的 faux provider 注册进真实的
 * createModels 集合，验证"我们的模型层"与"pi-ai 的真实类型"确实对得上。
 *
 * 为什么值得写：models.ts 里对 pi-ai 的用法（contentText、stopReason、
 * usage.cost）全是按源码约定写的。用 faux provider 跑一遍，
 * 就把这些约定变成了可执行的断言——pi-ai 升级改了字段名时，这里会红。
 */
test("用 faux provider 端到端跑通 complete（验证 pi-ai 用法）", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  faux.setResponses([fauxAssistantMessage("这是一段摘要")]);

  const res = await layer.complete(req());
  assert.equal(res.text, "这是一段摘要");
  assert.ok(res.usage.totalTokens > 0, "计量必须被解析出来");
  assert.equal(typeof res.usage.input, "number");
  assert.equal(typeof res.usage.cost.total, "number");
});

test("provider 返回 stopReason=error 时转成错误而不是空结果", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  // 这是 pi-ai 表达失败的方式：不抛异常，而是给一条 stopReason=error 的消息。
  // 漏判这一条会把失败当成"模型说没有内容"写进库。
  faux.setResponses([fauxAssistantMessage([], { stopReason: "error", errorMessage: "额度不足" })]);

  await assert.rejects(() => layer.complete(req()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "provider_error");
    assert.match(err.message, /额度不足/);
    return true;
  });
});

test("模型返回空内容时也报错（空结果比失败更难查）", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  faux.setResponses([fauxAssistantMessage("")]);

  await assert.rejects(() => layer.complete(req()), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "invalid_response");
    return true;
  });
});

test("要求 JSON 时 system 里会被加上结构化输出约束", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  let seenSystem = "";
  faux.setResponses([
    (ctx) => {
      seenSystem = ctx.messages
        .filter((m) => m.role === "system")
        .map((m) => String(m.content))
        .join("\n");
      return fauxAssistantMessage('{"ok":true}');
    },
  ]);

  const res = await layer.complete(req({ json: true, system: "你是标注员" }));
  assert.equal(res.text, '{"ok":true}');
  assert.ok(seenSystem.includes("你是标注员"), "原有 system 不能丢");
  assert.ok(seenSystem.includes(JSON_OUTPUT_INSTRUCTION), "必须附加 JSON 约束");
});

test("流式返回 text_delta 并在最后一帧带上计量", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  faux.setResponses([fauxAssistantMessage("第一段第二段")]);

  const deltas: string[] = [];
  let done = false;
  let usage: unknown;
  for await (const chunk of layer.stream(req())) {
    if (chunk.done) {
      done = true;
      usage = chunk.usage;
    } else {
      deltas.push(chunk.delta);
    }
  }
  assert.ok(done, "必须有一个 done 分片");
  assert.equal(deltas.join(""), "第一段第二段");
  assert.ok(usage, "done 分片必须带计量");
});

test("模型名支持 provider:model 形式，未知模型报 model_not_found", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  const modelId = faux.getModel().id;
  assert.equal(layer.hasModel("faux", modelId), true);
  assert.equal(layer.hasModel("faux", "no-such-model"), false);

  faux.setResponses([fauxAssistantMessage("ok")]);
  await assert.rejects(
    () => layer.complete(req({ model: "faux:no-such-model" })),
    (err: unknown) => {
      assert.ok(err instanceof GatewayError);
      assert.equal(err.code, "model_not_found");
      return true;
    },
  );
});

test("resolve 报告实际使用的 provider 与模型", async () => {
  const faux = fauxProvider();
  const layer = await createModelLayerWithFaux(faux);
  const r = layer.resolve(req());
  assert.equal(r?.provider, "faux");
  assert.equal(r?.model, faux.getModel().id);
});

// ---------------------------------------------------------------------------
// 测试辅助：把 faux provider 装进**真实的**模型层
// ---------------------------------------------------------------------------

import { createModels } from "@earendil-works/pi-ai";
import type { FauxProviderHandle } from "@earendil-works/pi-ai";

/**
 * 用 pi-ai 自带的 faux provider + 真实的 createModelLayer 造出模型层。
 *
 * 关键点：这里没有任何"镜像实现"——走的就是生产代码里的 models.ts。
 * 于是本文件对 pi-ai 的所有用法约定（contentText、stopReason、usage.cost、
 * 流式事件名）都被真实执行过，pi-ai 升级改字段时这里会立刻红，
 * 而 CI 依然不需要 API key 与网络。
 */
async function createModelLayerWithFaux(faux: FauxProviderHandle): Promise<ModelLayer> {
  const models = createModels();
  models.setProvider(faux.provider);
  return createModelLayer({ models, defaultModel: faux.getModel().id });
}

// ---------------------------------------------------------------------------
// 离线演示模式的连续应答（P2 前置项 S1）
// ---------------------------------------------------------------------------

test("离线演示模式可以连续应答 N≥5 次，而不是只成功一次", async () => {
  // 背景：pi-ai 的 faux 是**队列**语义——每一项只被 shift() 消费一次，
  // 队列空了之后每次调用都返回 `No more faux responses queued`。
  // 旧实现只塞了一条，实测连续发布 3 条视频时第 2、3 条全部失败。
  // P2 的精排一次请求里可能调用多次模型，这个限制会变成开发阻力。
  const layer = await createModelLayer({ faux: { response: '{"summary":"单条"}' } });
  for (let i = 1; i <= 6; i++) {
    const res = await layer.complete(req({ json: true }));
    assert.match(res.text, /单条/, `第 ${i} 次调用必须成功`);
  }
});

test("多条脚本化应答按序返回，并且用完后循环", async () => {
  // 保留"按序返回不同响应"的能力是刻意的：否则测不了"多次调用拿到不同结果"
  // 这类场景（例如精排在第二批候选上给出不同的顺序）。
  const layer = await createModelLayer({
    faux: { response: "兜底", responses: ["一", "二", "三"] },
  });
  const got: string[] = [];
  for (let i = 0; i < 7; i++) {
    got.push((await layer.complete(req())).text);
  }
  assert.deepEqual(got, ["一", "二", "三", "一", "二", "三", "一"]);
});

test("脚本化应答里的空白条目被忽略，不会变成空应答", async () => {
  // 空字符串会被上游当成"模型没写出内容"，那是脏数据的来源。
  const layer = await createModelLayer({ faux: { response: "兜底", responses: ["甲", "  ", "乙"] } });
  const got: string[] = [];
  for (let i = 0; i < 4; i++) got.push((await layer.complete(req())).text);
  assert.deepEqual(got, ["甲", "乙", "甲", "乙"]);
});

test("离线演示模式下，配置里的默认模型名可以直接请求", async () => {
  // S2 要修的场景：`docker compose up` 只靠环境变量就能跑通离线闭环。
  // 而 compose 里的 AI_DEFAULT_MODEL 默认是 gpt-4o-mini，faux provider
  // 自带的模型却叫 faux-1 —— 不注册配置里的名字，请求会以
  // `未知模型: gpt-4o-mini` 失败，演示第一步就断。
  const layer = await createModelLayer({
    defaultModel: "gpt-4o-mini",
    faux: { response: '{"ok":true}' },
  });
  const res = await layer.complete(req({ model: "gpt-4o-mini", json: true }));
  assert.match(res.text, /ok/);

  // 但**其他**未知模型仍然必须失败：静默换模型会让"模型名写错了"
  // 变成"结果口径变了"，而日志里一切正常。
  await assert.rejects(() => layer.complete(req({ model: "no-such-model" })), (err: unknown) => {
    assert.ok(err instanceof GatewayError);
    assert.equal(err.code, "model_not_found");
    return true;
  });
});
