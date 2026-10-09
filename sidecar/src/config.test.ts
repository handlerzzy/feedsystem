/**
 * 配置与开关的测试。
 *
 * 为什么先测这个：sidecar 的全部安全性都建立在"默认关闭"之上。
 * 默认值一旦被改成 true，所有没显式配置的环境都会开始真的调模型，
 * 而这个变化在代码 review 里非常容易被忽略（一行布尔字面量）。
 */

import { strict as assert } from "node:assert";
import { test } from "node:test";

import { configuredProviderIds, loadConfig } from "./config.ts";

test("未设置任何环境变量时默认全关", () => {
  const cfg = loadConfig({});
  assert.equal(cfg.enabled, false, "默认必须是关的：缺省 = AI 全关");
  // 必须是 host:port 的完整形式：写成 ":50051" 会让 grpc-js 在 Node 24 上
  // 因空主机名而启动失败（只在运行期暴露）。
  assert.equal(cfg.listenAddr, "0.0.0.0:50051");
  assert.equal(cfg.defaultModel, "gpt-4o-mini");
  assert.equal(cfg.maxTokens, 512);
  // 没有任何 key 时不应探测出 provider。
  assert.deepEqual(configuredProviderIds({}), []);
});

test("AI_ENABLED 的常见写法都能识别", () => {
  for (const raw of ["1", "true", "TRUE", "yes", "on"]) {
    assert.equal(loadConfig({ AI_ENABLED: raw }).enabled, true, `${raw} 应视为打开`);
  }
  for (const raw of ["0", "false", "no", "off", ""]) {
    assert.equal(loadConfig({ AI_ENABLED: raw }).enabled, false, `${raw} 应视为关闭`);
  }
});

test("AI_ENABLED 写错时保留关闭而不是猜", () => {
  // 与 Go 侧 config.ApplyEnvOverrides 的行为一致：写错值既不打开也不报错，
  // 保持原状态（关）。一次拼写错误不该静默打开模型调用。
  for (const raw of ["maybe", "tru", "yes please"]) {
    assert.equal(loadConfig({ AI_ENABLED: raw }).enabled, false, `${raw} 不该被当成打开`);
  }
});

test("非法数字回落到默认值而不是 NaN", () => {
  const cfg = loadConfig({ AI_MAX_TOKENS: "abc" });
  assert.equal(cfg.maxTokens, 512);
  assert.ok(Number.isFinite(cfg.maxTokens));
});

test("provider 探测只看 key 是否存在", () => {
  assert.deepEqual(configuredProviderIds({ OPENAI_API_KEY: "sk-x" }), ["openai"]);
  assert.deepEqual(configuredProviderIds({ ANTHROPIC_API_KEY: "a", DEEPSEEK_API_KEY: "d" }), [
    "anthropic",
    "deepseek",
  ]);
  // 空白串不算配置。
  assert.deepEqual(configuredProviderIds({ OPENAI_API_KEY: "   " }), []);
});

test("proto 路径可以被环境变量覆盖", () => {
  // 容器里的相对位置与仓库里不同，写死路径会造成"本地能跑、镜像里跑不起来"。
  const cfg = loadConfig({ AI_GATEWAY_PROTO_PATH: "/app/api/proto/ai/v1/model_gateway.proto" });
  assert.equal(cfg.protoPath, "/app/api/proto/ai/v1/model_gateway.proto");
  // 未覆盖时必须给出一个路径（默认值指向仓库内位置），不能是空串。
  assert.ok(loadConfig({}).protoPath.endsWith("model_gateway.proto"));
});

test("embedding 配置有安全的默认值，且默认要求归一化", () => {
  const cfg = loadConfig({});
  assert.equal(cfg.embedding.model, "text-embedding-3-small");
  assert.equal(cfg.embedding.dim, 1536);
  // 默认 true：没归一化的向量会让长文本在余弦检索里系统性占优，
  // 而这种偏差看起来像"模型偏好长内容"，几乎不可能反查到。
  assert.equal(cfg.embedding.normalize, true);
  assert.equal(cfg.embedding.apiKey, "", "默认不能有任何凭据");
  assert.ok(cfg.embedding.timeoutMs > 0);
});

test("embedding 凭据优先用专用变量，否则退回 OPENAI_API_KEY", () => {
  assert.equal(loadConfig({ OPENAI_API_KEY: "sk-a" }).embedding.apiKey, "sk-a");
  assert.equal(
    loadConfig({ OPENAI_API_KEY: "sk-a", AI_EMBEDDING_API_KEY: "sk-b" }).embedding.apiKey,
    "sk-b",
    "专用变量优先：兼容端点通常给一个单独的 key",
  );
});

test("AI_FAUX_RESPONSES 只接受字符串数组，写错时退回单条模式而不是抛错", () => {
  assert.deepEqual(loadConfig({ AI_FAUX_RESPONSES: '["a","b"]' }).fauxResponses, ["a", "b"]);
  // 非字符串元素被剔除，合法的仍然保留（而不是整份配置作废）。
  assert.deepEqual(loadConfig({ AI_FAUX_RESPONSES: '["a",1,null,{"x":1}]' }).fauxResponses, ["a"]);
  // 写错值不该让进程起不来：AI 必须完全可选。
  for (const bad of ['{"a":1}', "not-json", '"a"']) {
    const cfg = loadConfig({ AI_FAUX_RESPONSES: bad });
    assert.ok(Array.isArray(cfg.fauxResponses), bad);
    assert.deepEqual(cfg.fauxResponses, [], bad);
  }
  assert.deepEqual(loadConfig({}).fauxResponses, []);
});
