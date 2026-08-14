// Package version 提供构建时注入的版本信息。
package version

var (
	// Version 是 zlite 的语义化版本号，发版时手动更新（见 docs/dev.md）。
	// 不被 ldflags 注入，任何构建方式（make build / go build）均生效。
	Version = "0.1.0"
	// Commit 是构建时的 git commit。
	Commit = "none"
	// BuildTime 是构建时间（UTC RFC3339）。
	BuildTime = "unknown"
)

// String 返回完整版本字符串。
func String() string {
	s := Version
	if Commit != "" && Commit != "none" {
		s += " (" + Commit + ")"
	}
	if BuildTime != "" && BuildTime != "unknown" {
		s += " built " + BuildTime
	}
	return s
}
