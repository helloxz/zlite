# zlite 插件开发 (docs/plugin.md)

> 轻量 JS 插件：单文件 `index.js` + `plugin.json`，无 `npm`，动态加载，`~/.zlite/plugins` 存放。

## 1. 原理

`fastschema/qjs` = `QuickJS-NG -> qjs.wasm -> wazero(Pure Go)`，无 `cgo`，跨平台。

插件 = **动态工具注入**：`~/.zlite/plugins/*/index.js` 在首轮对话前 `qjs.New -> Eval -> activate(ctx) -> ctx.registerTool -> tools.Registry.Register -> ForMode -> LLM 可见 -> Execute 时回调 JS`。

无插件时零开销：不编译 `WASM`，不创建 `Runtime`。

## 2. 目录

```
~/.zlite/plugins/<name>/
  plugin.json
  index.js
~/.zlite/plugins_data/<name>.json  # kv 持久化，自动创建
```

`plugin.json` 最小：

```json
{
  "name": "hello",
  "version": "0.1.0",
  "description": "demo",
  "author": "helloxz",
  "homepage": "https://github.com/helloxz/zlite-plugins-hello",
  "modes": ["build"],
  "timeout_ms": 5000
}
```

`name` 正则 `^[a-z0-9][a-z0-9_-]{0,63}$`，`modes` 缺省 `["build"]`，`timeout_ms` 缺省 `5000`，上限 `30000`。`author/homepage` 可选，`homepage` 需 `http(s)://`。
`MaxPlugins=10`：按目录名排序取前10个，超出警告并跳过（防上下文爆炸，与 `skills` 截断策略一致）。
## 3. 能力集（首期）

`registerTool + kv + fetch + log`，无 `fs`。

`activate(ctx)` 收到：

```js
ctx = {
  registerTool({name, description, parameters, execute}),
  fetch(url, opts) -> {status, body, headers}, // 同步，Go net/http 代理
  kv: { get(key) -> string|null, set(key, value), delete(key) },
  log: { info(msg), error(msg) }
}
// 全局别名：registerTool, fetch, kv, log 同可直接调用
```

`parameters` 为 JSON Schema：`{type:"object", properties:{...}, required:[...]}`。

## 4. 最小 Demo

`~/.zlite/plugins/hello/plugin.json`:

```json
{
  "name": "hello",
  "version": "0.1.0",
  "description": "demo hello",
  "author": "helloxz",
  "homepage": "https://github.com/helloxz/zlite-plugins-hello",
  "modes": ["build", "plan"]
}
```

`~/.zlite/plugins/hello/index.js`:

```js
globalThis.activate = function(ctx) {
  ctx.registerTool({
    name: "hello_world",
    description: "say hello",
    parameters: {
      type: "object",
      properties: { name: { type: "string", description: "name" } },
      required: ["name"]
    },
    execute: function(args) {
      var n = args.name || "world";
      var cnt = kv.get("count");
      var c = cnt ? parseInt(cnt) : 0;
      c++;
      kv.set("count", ""+c);
      log.info("hello " + n + " count=" + c);
      return "hello " + n + " count=" + c;
    }
  });
};
```

安装后重启 `zlite`，首轮对话模型即可看到 `hello_world`。

## 5. 网络 Demo

```js
globalThis.activate = function(ctx) {
  ctx.registerTool({
    name: "gh_user",
    description: "fetch github user",
    parameters: {
      type: "object",
      properties: { username: { type: "string" } },
      required: ["username"]
    },
    execute: function(args) {
      var res = fetch("https://api.github.com/users/" + encodeURIComponent(args.username), {
        headers: { "User-Agent": "zlite-plugin" }
      });
      if (res.status !== 200) return "fetch failed status=" + res.status + " body=" + res.body;
      var data = JSON.parse(res.body);
      return "user " + data.login + " followers=" + data.followers;
    }
  });
};
```

`fetch(url, {method, headers, body})` 同步，`body` 限制 `64K`，超时取 `plugin.json:timeout_ms`。

## 6. 约束

* 单文件，无 `npm`/`import`/`export`：用 `globalThis.activate = function(ctx){...}`，顶层也可直接 `registerTool` 而不写 `activate`。
* 重名跳过：工具名已存在（内置/MCP/其他插件）则跳过并 `Notify` 警告。
* 隔离：每插件一 `Runtime`，`MemoryLimit 16M`，`MaxStack 256K`，`execute` 串行化（`sync.Mutex`）。
* 调试：`log.info` 打到 stdout，加载失败经 `agent.Notify` 以 `system` 消息展示。

## 7. 调试

```bash
ls ~/.zlite/plugins
cat ~/.zlite/plugins/hello/index.js
# 改后下轮对话生效（preRun 幂等增量加载）
# 查看 kv
cat ~/.zlite/plugins_data/hello.json
```
