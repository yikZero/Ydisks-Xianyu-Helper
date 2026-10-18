// Package publishimage 读取和下载商品发布图片：本地文件限制在指定目录内，远程地址遵循 netguard 网络策略，
// 两者都限制大小并按内容识别图片类型。
package publishimage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xianyu-go/internal/netguard"
)

// ReadFile 从 dir 读取一张受路径边界保护的发布图片，返回内容、媒体类型和文件名。
// reference 必须是 dir 内的相对路径，拒绝绝对路径、越界路径、超过 10 MiB 或非图片的文件。
func ReadFile(dir, reference string) ([]byte, string, string, error) {
	// relativePath 是上传目录内的清理后相对图片路径，绝不允许越界到宿主文件系统。
	relativePath := filepath.Clean(strings.TrimSpace(reference))
	if relativePath == "." || filepath.IsAbs(relativePath) || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return nil, "", "", fmt.Errorf("图片路径无效")
	}
	// root、openErr 分别是受限上传目录句柄及其打开错误。
	root, openErr := os.OpenRoot(dir)
	if openErr != nil {
		return nil, "", "", fmt.Errorf("打开图片目录失败")
	}
	defer root.Close()
	// file、fileErr 分别是受目录边界保护的图片文件及其读取错误。
	file, fileErr := root.Open(relativePath)
	if fileErr != nil {
		return nil, "", "", fmt.Errorf("读取图片失败: %s", reference)
	}
	defer file.Close()
	// data、readErr 分别是受 10 MiB 限制的图片字节及其读取错误。
	data, readErr := io.ReadAll(io.LimitReader(file, (10<<20)+1))
	if readErr != nil || len(data) == 0 || len(data) > 10<<20 {
		return nil, "", "", fmt.Errorf("读取图片失败或文件过大: %s", reference)
	}
	// contentType 是按实际字节识别的媒体类型，用于拒绝伪装的非图片文件。
	contentType := http.DetectContentType(data)
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", "", fmt.Errorf("不是有效图片: %s", reference)
	}
	return data, contentType, filepath.Base(relativePath), nil
}

// Download 按运行时网络策略下载一张不超过 10 MiB 的发布图片，返回内容和媒体类型。
func Download(ctx context.Context, rawURL string) ([]byte, string, error) {
	// request、requestErr 分别是受调用方取消控制的公网图片下载请求及其构造错误。
	request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if requestErr != nil || (request.URL.Scheme != "http" && request.URL.Scheme != "https") || request.URL.Hostname() == "" {
		return nil, "", fmt.Errorf("图片 URL 无效: %s", rawURL)
	}
	// client 是限制内网访问的公网 HTTP 客户端；response、responseErr 是下载响应及其网络错误。
	client := netguard.ConfiguredHTTPClient(30 * time.Second)
	// response、responseErr 分别是远端图片响应及网络失败；正文必须在本函数返回前关闭。
	response, responseErr := client.Do(request)
	if responseErr != nil {
		return nil, "", fmt.Errorf("下载图片失败: %s", rawURL)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, "", fmt.Errorf("下载图片失败: %s HTTP %d", rawURL, response.StatusCode)
	}
	// data、readErr 分别是受大小限制的远端图片字节及其读取错误。
	data, readErr := io.ReadAll(io.LimitReader(response.Body, (10<<20)+1))
	if readErr != nil || len(data) > 10<<20 {
		return nil, "", fmt.Errorf("读取远程图片失败: %s", rawURL)
	}
	// contentType 是响应声明或字节探测得到的媒体类型。
	contentType := strings.Split(response.Header.Get("Content-Type"), ";")[0]
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("远程文件不是图片: %s", rawURL)
	}
	return data, contentType, nil
}
