// Command p37-mount 是 P36 引导用的最小挂载助手。
//
// 它刻意不放进 P37PAYLOAD：payload 还没挂载时，payload 里的二进制执行不到。
// 它是 L0 的稳定平台胶水，只做一件事：按 ext4 label 等待设备，并以只读方式
// 挂载到目标目录。用 mount(2) 而不是 setuid mount，因此 systemd 可以开
// NoNewPrivileges=yes，只保留 CAP_SYS_ADMIN。
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/chenyiwen/p37/internal/buildinfo"
)

var labelRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)

func main() {
	var (
		label   = flag.String("label", "P37PAYLOAD", "ext4 卷标")
		target  = flag.String("target", "/run/p37-payload", "只读挂载点")
		timeout = flag.Duration("timeout", 20*time.Second, "等待设备出现的时长")
		version = flag.Bool("version", false, "打印版本")
	)
	flag.Parse()

	if *version {
		fmt.Printf("p37-mount version=%s commit=%s buildTime=%s goVersion=%s\n",
			buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime, buildinfo.GoVersion)
		return
	}
	if !labelRE.MatchString(*label) {
		fmt.Fprintf(os.Stderr, "p37-mount: 非法卷标 %q\n", *label)
		os.Exit(2)
	}
	if *target == "" {
		fmt.Fprintln(os.Stderr, "p37-mount: --target 不能为空")
		os.Exit(2)
	}
	if err := mountPayload(*label, *target, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "p37-mount: %v\n", err)
		os.Exit(1)
	}
}
