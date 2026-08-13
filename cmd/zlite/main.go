// Command zlite 是轻量级 CLI Coding Agent 的入口。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/helloxz/zlite/internal/version"
)

func main() {
	mode := flag.String("m", "", "初始模式: plan | build（覆盖配置文件 agent.mode）")
	cont := flag.Bool("c", false, "继续当前目录最近的会话")
	list := flag.Bool("l", false, "列出当前目录的会话")
	acpMode := flag.Bool("acp", false, "以 ACP 协议模式运行（stdio，供编辑器等客户端接入）")
	showVersion := flag.Bool("version", false, "打印版本并退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("zlite %s\n", version.String())
		return
	}

	// 子命令形式兼容：zlite acp 与 zlite --acp 等价
	acp := *acpMode
	if args := flag.Args(); len(args) > 0 && args[0] == "acp" {
		acp = true
	}

	if err := run(options{mode: *mode, cont: *cont, list: *list, acp: acp}); err != nil {
		fmt.Fprintln(os.Stderr, "zlite:", err)
		os.Exit(1)
	}
}
