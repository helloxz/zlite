# zlite

轻量级 CLI Coding Agent：体积小、内存占用低，面向 Linux / macOS / Windows，用于日常对话、服务器运维等场景。


## 快速安装

### Linux / macOS

```bash
curl -fsSL https://raw.githubusercontent.com/helloxz/zlite/main/install.sh | bash
```

安装到 `~/.zlite/bin/`，并在 `~/.local/bin/` 创建 symlink。支持指定版本和自定义目录：

```bash
# 安装指定版本
bash install.sh -v 0.1.0

# 自定义安装目录（通常需要 sudo）
bash install.sh --dir /usr/local/bin

# 使用镜像下载
bash install.sh --base-url https://ghproxy.net/https://github.com
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/helloxz/zlite/main/install.ps1 | iex
```

安装到 `$HOME\.zlite\bin\`，并自动添加到用户 PATH。支持指定版本：

```powershell
# 安装指定版本（通过环境变量传参）
$env:ZLITE_VERSION = '0.1.0'; irm https://raw.githubusercontent.com/helloxz/zlite/main/install.ps1 | iex

# 或下载后执行
powershell -ExecutionPolicy Bypass -File install.ps1 -Version 0.1.0
```


## 一键更新

### Linux / macOS

```bash
bash update.sh
```

更新到最新版本，支持 `--force` 强制重装和 `-v` 指定版本：

```bash
bash update.sh --force          # 强制重装当前版本
bash update.sh -v 0.2.0         # 更新到指定版本
sudo bash update.sh             # 当安装在 /usr/local/bin 时
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/helloxz/zlite/main/update.ps1 | iex
```

支持 `-Force` 和 `-Version` 参数：

```powershell
# 强制更新
$env:ZLITE_FORCE = '1'; irm https://raw.githubusercontent.com/helloxz/zlite/main/update.ps1 | iex

# 或下载后执行
powershell -ExecutionPolicy Bypass -File update.ps1 -Version 0.2.0 -Force
```


## 安装布局

安装采用版本化布局，支持回滚到上一版本：

```
~/.zlite/bin/
├── zlite              # 命令入口（symlink / copy，指向当前版本）
├── zlite-0.1.0        # 版本化二进制
└── zlite-0.2.0        # 更新后保留上一版本用于回滚
```


## 从源码构建

要求 Go ≥ 1.25（本地已验证 1.25.7）。

```bash
make build        # 产物 bin/zlite，自动注入版本号
make test         # 单元测试
make vet          # go vet
bin/zlite --version
```


## 指令和快捷键

* `/init`：初始化项目
* `/new`：新建对话，快捷键`Ctrl + N`
* 切换模式：快捷键`Tab`
* `/switch`：切换模型：快捷键`Shift + Tab`
* `/sessions`：切换对话，快捷键`Ctrl + L`
* `/thinking`：思考强度切换，快捷键`Ctrl + T`
* `/exit`：退出TUI终端，快捷键`Ctrl + C`
* 聊天区滚动：`PgUp`/`PgDn` 翻页，`Home`/`End` 跳到顶部/底部

## 许可

AGPL-3.0（见 [LICENSE](./LICENSE)）
