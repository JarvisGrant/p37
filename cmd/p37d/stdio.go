package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/mcpstdio"
)

// runStdio 连接同仓库已有 daemon 的 unix socket，把 stdin/stdout 的 MCP
// 流量代理过去。
//
// 这里**绝不取仓库排他锁、绝不启动 daemon**：它只是一座桥。
func runStdio(root string, timeout time.Duration, in io.Reader, out, errw io.Writer) error {
	cfg, err := config.Load(root)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	sock := config.SocketPath(root, cfg)
	return mcpstdio.Run(context.Background(), sock, in, out, errw, mcpstdio.Options{
		ConnectTimeout: timeout,
	})
}
