// Package version 提供构建时注入的版本信息。
package version

var (
	// Version 由 Makefile 通过 -ldflags "-X" 注入，默认 dev。
	Version = "dev"
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
