package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/chenyiwen/p37/internal/config"
	"github.com/chenyiwen/p37/internal/hub"
)

// cmdRepo 是多仓库维护入口：
//
//	p37 --root /srv/repos repo list [--json]
//	p37 --root /srv/repos repo create <name> [--default-line main] [--json]
//	p37 --root /srv/repos repo info <id|name> [--json]
//	p37 --root /srv/repos repo delete <id|name> [--force]
func cmdRepo(root string, args []string, out, errw io.Writer) int {
	if root == "" {
		fmt.Fprintln(errw, "repo 命令需要 --root <root>")
		return 2
	}
	if len(args) == 0 {
		fmt.Fprintln(errw, "用法: p37 --root <root> repo <list|create|info|delete|register|unregister|scan> ...")
		return 2
	}

	sub := args[0]
	rest := args[1:]
	switch sub {
	case "register":
		return cmdRepoRegister(root, rest, out, errw)
	case "unregister":
		return cmdRepoUnregister(root, rest, out, errw)
	case "scan":
		return cmdRepoScan(root, rest, out, errw)
	}

	cfg, err := config.Load(root)
	if err != nil {
		return fail(errw, err)
	}
	h, err := hub.New(root, cfg)
	if err != nil {
		return fail(errw, err)
	}
	defer func() { _ = h.Close() }()

	switch sub {
	case "list":
		jsonOut := hasFlag(rest, "--json")
		es, err := h.List()
		if err != nil {
			return fail(errw, err)
		}
		if jsonOut {
			return writeJSON(out, errw, es)
		}
		for _, e := range es {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", e.ID, e.Name, e.Path, e.DefaultLine)
		}
		return 0
	case "create":
		var (
			name, defLine, remoteKey, remotePrefix string
			jsonOut                                bool
		)
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			switch {
			case a == "--json":
				jsonOut = true
			case a == "--default-line" && i+1 < len(rest):
				defLine = rest[i+1]
				i++
			case strings.HasPrefix(a, "--default-line="):
				defLine = strings.TrimPrefix(a, "--default-line=")
			case a == "--remote-key" && i+1 < len(rest):
				remoteKey = rest[i+1]
				i++
			case strings.HasPrefix(a, "--remote-key="):
				remoteKey = strings.TrimPrefix(a, "--remote-key=")
			case a == "--remote-prefix" && i+1 < len(rest):
				remotePrefix = rest[i+1]
				i++
			case strings.HasPrefix(a, "--remote-prefix="):
				remotePrefix = strings.TrimPrefix(a, "--remote-prefix=")
			case strings.HasPrefix(a, "-"):
				fmt.Fprintf(errw, "未知参数 %q\n", a)
				return 2
			default:
				if name != "" {
					fmt.Fprintf(errw, "repo create 只接受一个 name，多余参数 %q\n", a)
					return 2
				}
				name = a
			}
		}
		if name == "" {
			fmt.Fprintln(errw, "repo create 需要 <name>")
			return 2
		}
		e, err := h.CreateEx(name, defLine, remoteKey, remotePrefix)
		if err != nil {
			return fail(errw, err)
		}
		if jsonOut {
			return writeJSON(out, errw, e)
		}
		fmt.Fprintf(out, "created %s (%s) path=%s\n", e.Name, e.ID, e.Path)
		return 0
	case "info":
		jsonOut := hasFlag(rest, "--json")
		var ref string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				ref = a
				break
			}
		}
		if ref == "" {
			fmt.Fprintln(errw, "repo info 需要 <id|name>")
			return 2
		}
		e, err := h.Get(ref)
		if err != nil {
			return fail(errw, err)
		}
		if jsonOut {
			return writeJSON(out, errw, e)
		}
		fmt.Fprintf(out, "id:           %s\nname:         %s\npath:         %s\ndefault_line: %s\n",
			e.ID, e.Name, e.Path, e.DefaultLine)
		if e.RemoteKey != "" {
			fmt.Fprintf(out, "remote_key:   %s\nremote_prefix: %s\n", e.RemoteKey, e.RemotePrefix)
		}
		return 0
	case "delete":
		force := hasFlag(rest, "--force")
		var ref string
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				ref = a
				break
			}
		}
		if ref == "" {
			fmt.Fprintln(errw, "repo delete 需要 <id|name>")
			return 2
		}
		e, err := h.Delete(ref, force)
		if err != nil {
			return fail(errw, err)
		}
		fmt.Fprintf(out, "deleted %s (%s)\n", e.Name, e.ID)
		return 0
	default:
		fmt.Fprintf(errw, "未知 repo 子命令 %q\n", sub)
		return 2
	}
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func writeJSON(out, errw io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fail(errw, err)
	}
	fmt.Fprintln(out, string(b))
	return 0
}
