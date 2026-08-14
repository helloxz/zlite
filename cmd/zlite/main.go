// Command zlite 是轻量级 CLI Coding Agent 的入口。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/helloxz/zlite/internal/version"
)

func main() {
	mode := flag.String("m", "", "initial mode: plan | build (overrides agent.mode in config)")
	cont := flag.Bool("c", false, "continue the most recent session in the current directory")
	list := flag.Bool("l", false, "list sessions in the current directory")
	acpMode := flag.Bool("acp", false, "run in ACP protocol mode (stdio, for editors and other clients)")
	showVersion := flag.Bool("version", false, "print version and exit")
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
