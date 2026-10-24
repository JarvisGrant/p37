// Package buildinfo 保存由链接器注入的构建信息。
//
// 这些值必须是 var（不能是 const），否则 `go build -ldflags "-X ..."`
// 会静默失效——p37 早期就踩过这个坑：Makefile 明明传了 -X main.version，
// 二进制里仍然是源码里的常量。
package buildinfo

import "runtime"

var (
	// Version 是发布版本号（git tag / git describe）。
	Version = "0.3.2"
	// Commit 是构建时的 git commit。
	Commit = "unknown"
	// BuildTime 是 UTC RFC3339 构建时间。
	BuildTime = "unknown"
	// GoVersion 是构建所用 Go 工具链版本。
	GoVersion = runtime.Version()
)
