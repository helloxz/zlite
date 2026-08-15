# Zlite

Zlite 是一个轻量级 CLI AI Agent——小体积、低内存，适合日常对话、轻量编码，也可作为服务器软件的 Agent 大脑，用自然语言直接操作和管理 `nginx` 等服务。

## 特性

* **轻量级**：单文件二进制，体积 <12MB、内存占用 ≈20MB，下载即用
* **跨平台**：同时支持 Linux、macOS、Windows
* **多模型接入**：支持任意 OpenAI、Anthropic 兼容模型，可配置多个 Provider 随时切换
* **TUI 界面**：全功能终端交互，支持快捷键操作与 plan / build 模式切换
* **内置工具集**：读文件、搜索、写 / 改 / 删文件、Shell 执行、网页抓取、网络搜索等能力开箱即用
* **MCP 支持**：可调用任意 MCP 服务器提供的工具，接入丰富的外部生态
* **Skills 支持**：可加载项目级与全局 Skills，复用指令与工作流
* **ACP 协议**：作为 Agent 端接入任意 ACP 客户端，如 [Zacp](https://github.com/helloxz/zacp)、Zed、Codeg
* **上下文压缩**：长对话自动压缩，防止上下文爆炸
* **会话管理**：持久存储，支持会话恢复与切换
* **权限确认**：危险操作（如 Shell 命令）执行前请求人工确认


## 快速开始

### 安装

### Linux / macOS

```bash
curl -fsSL https://raw.githubusercontent.com/helloxz/zlite/main/install.sh | bash
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/helloxz/zlite/main/install.ps1 | iex
```

### 初始化

执行`zlite`根据提示填写 OpenAI 兼容模型信息。

### 指令和快捷键

* `/init`：初始化项目
* `/new`：新建对话，快捷键`Ctrl + N`
* 切换模式：快捷键`Tab`
* `/switch`：切换模型：快捷键`Shift + Tab`
* `/sessions`：切换对话，快捷键`Ctrl + L`
* `/thinking`：思考强度切换，快捷键`Ctrl + T`
* `/exit`：退出TUI终端，快捷键`Ctrl + C`
* 聊天区滚动：`PgUp`/`PgDn` 翻页，`Home`/`End` 跳到顶部/底部

### 一键更新

**Linux & macOS**

```bash
curl -fsSL https://raw.githubusercontent.com/helloxz/zlite/main/update.sh | bash
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/helloxz/zlite/main/update.ps1 | iex
```

## 从源码构建

要求 Go ≥ 1.25

```bash
make build        # 产物 bin/zlite，自动注入版本号
bin/zlite --version
```

## 联系作者

* Blog: [https://blog.xiaoz.org/](https://blog.xiaoz.org/)
* X: [https://x.com/xiaozblog](https://x.com/xiaozblog)
