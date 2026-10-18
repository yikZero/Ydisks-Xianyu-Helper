// Package main 是本机 MCP 命令行：SSH 登录服务所在机器后调用 /mcp 工具，或作为 MCP stdio 服务供客户端经 ssh 启动。
//
// 用法见 usage 常量与 docs/mcp.md。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xianyu-go/internal/mcp"
)

// usage 是命令帮助，也是 AI 通过 SSH 第一次接触本命令时看到的说明。
const usage = `用法：
  xianyu-mcp tools                列出可用工具及参数
  xianyu-mcp call <工具> [JSON]    调用工具；JSON 省略时为 {}，写 - 时从标准输入读取
  xianyu-mcp stdio                作为 MCP stdio 服务运行（MCP 客户端经 ssh 启动）

示例：
  ssh frybox xianyu-mcp tools
  echo '{"keyword":"夸克"}' | ssh frybox xianyu-mcp call list_items -

选项：
  -url URL          MCP 端点，默认读取 XIANYU_MCP_URL，否则 http://127.0.0.1:59188/mcp
  -token-file PATH  API Token 文件，默认 ~/.config/ydisks-xianyu-helper/api-token；
                    设置了 XIANYU_API_TOKEN 环境变量时优先使用环境变量
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run 解析参数并执行子命令，返回进程退出码：0 成功，1 调用失败或工具返回业务错误，2 用法错误。
func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("xianyu-mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, usage) }
	endpoint := flags.String("url", "", "")
	tokenFile := flags.String("token-file", "", "")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() == 0 {
		flags.Usage()
		return 2
	}
	client, err := newClient(*endpoint, *tokenFile, getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	switch command := flags.Arg(0); command {
	case "tools":
		err = printTools(ctx, client, stdout)
	case "call":
		if flags.NArg() < 2 {
			flags.Usage()
			return 2
		}
		var isError bool
		isError, err = callTool(ctx, client, flags.Arg(1), flags.Arg(2), stdin, stdout)
		if err == nil && isError {
			return 1
		}
	case "stdio":
		err = client.ServeStdio(ctx, stdin, stdout)
	default:
		_, _ = fmt.Fprintf(stderr, "未知子命令 %q\n\n", command)
		flags.Usage()
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// newClient 按“参数 > 环境变量 > 默认值”解析端点和 Token。
func newClient(endpoint, tokenFile string, getenv func(string) string) (*mcp.Client, error) {
	endpoint = firstNonEmpty(endpoint, getenv("XIANYU_MCP_URL"), "http://127.0.0.1:59188/mcp")
	token := strings.TrimSpace(getenv("XIANYU_API_TOKEN"))
	if token == "" {
		if tokenFile == "" {
			home := getenv("HOME")
			if home == "" {
				return nil, errors.New("无法确定 Token 文件位置：请设置 XIANYU_API_TOKEN 或 -token-file")
			}
			tokenFile = filepath.Join(home, ".config", "ydisks-xianyu-helper", "api-token")
		}
		content, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("读取 API Token 文件失败（用 xianyu-server -api-token-create 创建后写入该文件）: %w", err)
		}
		token = strings.TrimSpace(string(content))
	}
	if token == "" {
		return nil, errors.New("API Token 为空")
	}
	return &mcp.Client{URL: endpoint, Token: token, HTTP: &http.Client{Timeout: 5 * time.Minute}}, nil
}

// firstNonEmpty 返回第一个去空白后非空的值。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// printTools 以便于阅读的文本列出工具、权限和参数。
func printTools(ctx context.Context, client *mcp.Client, out io.Writer) error {
	tools, err := client.ListTools(ctx)
	if err != nil {
		return err
	}
	var text strings.Builder
	for _, tool := range tools {
		access := "写"
		if tool.Annotations.ReadOnlyHint {
			access = "只读"
		}
		fmt.Fprintf(&text, "%s [%s]\n  %s\n", tool.Name, access, tool.Description)
		required := map[string]bool{}
		for _, name := range tool.InputSchema.Required {
			required[name] = true
		}
		names := make([]string, 0, len(tool.InputSchema.Properties))
		for name := range tool.InputSchema.Properties {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			if required[names[i]] != required[names[j]] {
				return required[names[i]]
			}
			return names[i] < names[j]
		})
		for _, name := range names {
			property := tool.InputSchema.Properties[name]
			need := "可选"
			if required[name] {
				need = "必填"
			}
			fmt.Fprintf(&text, "  - %s (%s, %s) %s\n", name, property.Type, need, property.Description)
		}
	}
	_, err = io.WriteString(out, text.String())
	return err
}

// callTool 调用工具并把文本结果写到 out；argument 为 - 时从 stdin 读取 JSON 参数。
func callTool(ctx context.Context, client *mcp.Client, name, argument string, stdin io.Reader, out io.Writer) (bool, error) {
	if argument == "-" {
		content, err := io.ReadAll(stdin)
		if err != nil {
			return false, err
		}
		argument = string(content)
	}
	if strings.TrimSpace(argument) == "" {
		argument = "{}"
	}
	if !json.Valid([]byte(argument)) {
		return false, errors.New("工具参数必须是 JSON 对象，例如 '{\"account_id\":\"123\"}'")
	}
	text, isError, err := client.CallTool(ctx, name, json.RawMessage(argument))
	if err != nil {
		return false, err
	}
	_, err = io.WriteString(out, text+"\n")
	return isError, err
}
