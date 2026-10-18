package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"xianyu-go/internal/mcp"
)

// mcpUsage 是 mcp 子命令帮助，也是 AI 通过 SSH 第一次接触本命令时看到的说明。
const mcpUsage = `用法：
  xianyu-server mcp tools                列出可用工具及参数
  xianyu-server mcp call <工具> [JSON]    调用工具；JSON 写 - 时从标准输入读取，省略表示无参数
  xianyu-server mcp stdio                作为 MCP stdio 服务运行（MCP 客户端经 ssh 启动）

示例：
  ssh frybox xianyu-server mcp tools
  echo '{"keyword":"夸克"}' | ssh frybox xianyu-server mcp call list_items -

选项：
  -url URL          服务地址，默认读取 XIANYU_SERVICE_URL，否则 http://192.168.31.5:59188
  -token-file PATH  API Token 文件，默认是 -api-token-save 写入的位置；
                    设置了 XIANYU_API_TOKEN 环境变量时优先使用环境变量
`

// defaultAPITokenFile 返回 -api-token-save 写入、mcp 子命令默认读取的 Token 文件路径。
func defaultAPITokenFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法确定 Token 文件位置，请使用 -token-file 或 XIANYU_API_TOKEN: %w", err)
	}
	return filepath.Join(dir, "ydisks-xianyu-helper", "api-token"), nil
}

// runMCPCommand 执行 mcp 子命令，返回进程退出码：0 成功，1 调用失败或工具返回业务错误，2 用法错误。
func runMCPCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("xianyu-server mcp", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { _, _ = io.WriteString(stderr, mcpUsage) }
	serviceURL := flags.String("url", cmp.Or(os.Getenv("XIANYU_SERVICE_URL"), "http://192.168.31.5:59188"), "")
	tokenFile := flags.String("token-file", "", "")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	command := flags.Arg(0)
	if !slices.Contains([]string{"tools", "call", "stdio"}, command) || (command == "call" && flags.NArg() < 2) {
		flags.Usage()
		return 2
	}
	client, err := newMCPClient(*serviceURL, *tokenFile)
	if err == nil {
		switch command {
		case "tools":
			err = printMCPTools(ctx, client, stdout)
		case "call":
			var isError bool
			isError, err = callMCPTool(ctx, client, flags.Arg(1), flags.Arg(2), stdin, stdout)
			if err == nil && isError {
				return 1
			}
		case "stdio":
			err = client.ServeStdio(ctx, stdin, stdout)
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// newMCPClient 解析 Token（环境变量优先，其次 Token 文件）并构造指向 serviceURL/mcp 的客户端。
func newMCPClient(serviceURL, tokenFile string) (*mcp.Client, error) {
	token := strings.TrimSpace(os.Getenv("XIANYU_API_TOKEN"))
	if token == "" {
		if tokenFile == "" {
			var err error
			if tokenFile, err = defaultAPITokenFile(); err != nil {
				return nil, err
			}
		}
		content, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("读取 API Token 文件失败（用 -api-token-create 名称 -api-token-save 创建）: %w", err)
		}
		token = strings.TrimSpace(string(content))
	}
	if token == "" {
		return nil, errors.New("API Token 为空")
	}
	// 超时略长于服务端 2 分钟的写超时，超过后服务端也无法再写回响应。
	return &mcp.Client{URL: strings.TrimRight(serviceURL, "/") + "/mcp", Token: token, HTTP: &http.Client{Timeout: 2*time.Minute + 15*time.Second}}, nil
}

// printMCPTools 先输出服务端使用说明，再以便于阅读的文本列出工具、权限和参数；必填参数在前。
func printMCPTools(ctx context.Context, client *mcp.Client, out io.Writer) error {
	instructions, err := client.Instructions(ctx)
	if err != nil {
		return err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		return err
	}
	var text strings.Builder
	text.WriteString(instructions + "\n\n")
	for _, tool := range tools {
		access := "写"
		if tool.Annotations.ReadOnlyHint {
			access = "只读"
		}
		fmt.Fprintf(&text, "%s [%s]\n  %s\n", tool.Name, access, tool.Description)
		optional := []string{}
		for name := range tool.InputSchema.Properties {
			if !slices.Contains(tool.InputSchema.Required, name) {
				optional = append(optional, name)
			}
		}
		sort.Strings(optional)
		for i, name := range append(slices.Clone(tool.InputSchema.Required), optional...) {
			need := "必填"
			if i >= len(tool.InputSchema.Required) {
				need = "可选"
			}
			property := tool.InputSchema.Properties[name]
			fmt.Fprintf(&text, "  - %s (%s, %s) %s\n", name, property.Type, need, property.Description)
		}
	}
	_, err = io.WriteString(out, text.String())
	return err
}

// callMCPTool 调用工具并把文本结果写到 out；argument 为 - 时从 stdin 读取 JSON 参数，为空时按无参数调用。
func callMCPTool(ctx context.Context, client *mcp.Client, name, argument string, stdin io.Reader, out io.Writer) (bool, error) {
	arguments := []byte(argument)
	if argument == "-" {
		var err error
		if arguments, err = io.ReadAll(stdin); err != nil {
			return false, err
		}
	}
	if len(bytes.TrimSpace(arguments)) == 0 {
		arguments = nil
	} else if !json.Valid(arguments) {
		return false, errors.New(`工具参数必须是 JSON 对象，例如 '{"account_id":"123"}'`)
	}
	text, isError, err := client.CallTool(ctx, name, arguments)
	if err != nil {
		return false, err
	}
	_, err = io.WriteString(out, text+"\n")
	return isError, err
}
