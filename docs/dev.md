# zlite 开发者说明（dev）

本文件记录开发过程中需要手动维护的事项。

## 版本号维护

**版本号唯一维护位置：`internal/version/version.go` 中的 `Version` 变量。**

发版时手动修改该变量的值即可，例如：

```go
// internal/version/version.go
Version = "0.1.0" // 发版时改为新版本号，如 "0.2.0"
```

说明：

- 版本号**不**由 `Makefile` 注入（`Makefile` 只注入 `Commit` 与 `BuildTime`），
  因此无论用 `make build` 还是直接 `go build`（含 Windows 的
  `GOOS=windows go build ./cmd/zlite`），产物中的版本号都来自上述唯一位置。
- 版本号会体现在两处输出中：
  - `zlite -version` / `zlite --version`：打印 `zlite <版本号> (<commit>) built <时间>`；
  - 会话（session）元数据中的 `version` 字段。
- 若希望语义化版本与 git tag 保持一致，发版时同步打 tag 即可，但版本号本身以
  `version.go` 为准。

## 上下文压缩规则

1. 超过30轮，自从裁剪丢弃原来的对话
2. 超过60轮，直接拒绝对话
3. 压缩内容必须超过10轮
4. 压缩后原来的对话将被丢弃，从压缩的内容重新开始

## mcp实现

1. 超过5个工具，后续会被丢弃