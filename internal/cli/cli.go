// Package cli 是 appkit CLI 的子命令注册表与公共设施。
//
// 每个子命令一个文件，经 init 里的 register 挂载——新增命令不改本文件。
package cli

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"sync"
)

// Command 是一个子命令。Run 收到的 args 已去掉命令名本身。
type Command struct {
	Name    string
	Summary string
	Run     func(args []string) error
}

var (
	mu       sync.Mutex
	commands = map[string]Command{}
)

func register(c Command) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := commands[c.Name]; dup {
		panic("cli: 子命令重复注册: " + c.Name)
	}
	commands[c.Name] = c
}

// Main 分发子命令，返回进程退出码。
func Main(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		return 0
	}
	if args[0] == "version" {
		fmt.Println(Version())
		return 0
	}
	mu.Lock()
	c, ok := commands[args[0]]
	mu.Unlock()
	if !ok {
		fmt.Fprintf(os.Stderr, "appkit: 未知子命令 %q\n\n", args[0])
		usage()
		return 2
	}
	if err := c.Run(args[1:]); err != nil {
		var agentErr *agentExit
		if errors.As(err, &agentErr) {
			return agentErr.code
		}
		fmt.Fprintf(os.Stderr, "appkit %s: %v\n", c.Name, err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Println("appkit — forgeplex 后端框架 CLI")
	fmt.Println()
	fmt.Println("用法: appkit <command> [flags]")
	fmt.Println()
	mu.Lock()
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	mu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %-12s %s\n", n, commands[n].Summary)
	}
	fmt.Printf("  %-12s %s\n", "version", "打印 CLI 版本")
}

// Version 返回 CLI 的模块版本（go install 时为 tag，源码构建为 devel）。
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
