package main

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// useServiceURL 临时把托盘探测的服务地址指向 url，并在测试结束后恢复原值。
func useServiceURL(t *testing.T, url string) {
	t.Helper()
	// original 是替换前的服务地址。
	original := serviceURL
	serviceURL = url
	t.Cleanup(func() { serviceURL = original })
}

// TestProbeHealthBranches 覆盖健康检查的成功、非 2xx、非法 JSON 与连接失败四种结果。
func TestProbeHealthBranches(t *testing.T) {
	// cases 描述服务端响应与期望的 (可达, 是否报错) 组合。
	cases := []struct {
		// name 是子测试名。
		name string
		// handler 模拟 /health 响应。
		handler http.HandlerFunc
		// reachable 表示期望端口可达。
		reachable bool
		// wantErr 表示期望返回错误。
		wantErr bool
	}{
		{"ok", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok","database":"ok","version":"1","commit":"c"}`))
		}, true, false},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }, true, true},
		{"bad json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not-json")) }, true, true},
	}
	// tc 是当前用例。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// server 是本地模拟的后台服务。
			server := httptest.NewServer(tc.handler)
			t.Cleanup(server.Close)
			useServiceURL(t, server.URL)
			// health、reachable、err 是探测结果。
			health, reachable, err := probeHealth(server.Client())
			if reachable != tc.reachable || (err != nil) != tc.wantErr {
				t.Fatalf("reachable=%v err=%v", reachable, err)
			}
			if tc.name == "ok" && (health.Status != "ok" || health.Version != "1" || health.Commit != "c") {
				t.Fatalf("health=%+v", health)
			}
			// readHealth 与 probeHealth 的错误语义一致。
			if _, readErr := readHealth(server.Client()); (readErr != nil) != tc.wantErr {
				t.Fatalf("readHealth err=%v", readErr)
			}
		})
	}
	// closed 先启动再关闭，得到一个必定拒绝连接的地址。
	closed := httptest.NewServer(http.NotFoundHandler())
	// closedURL 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	closedURL := closed.URL
	closed.Close()
	useServiceURL(t, closedURL)
	// err、reachable 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if _, reachable, err := probeHealth(&http.Client{Timeout: time.Second}); reachable || err == nil {
		t.Fatalf("关闭的服务应不可达: reachable=%v err=%v", reachable, err)
	}
}

// TestWaitForServiceTimeouts 验证等待启动/停止超时时返回各自的错误文案。
func TestWaitForServiceTimeouts(t *testing.T) {
	// degraded 始终返回异常状态但端口可达的服务。
	degraded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"degraded","database":"error"}`))
	}))
	t.Cleanup(degraded.Close)
	useServiceURL(t, degraded.URL)
	if err := waitForService(degraded.Client(), true, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "启动超时") {
		t.Fatalf("err=%v", err)
	}
	if err := waitForService(degraded.Client(), false, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "退出超时") {
		t.Fatalf("err=%v", err)
	}
}

// TestEnvOr 验证环境变量去空白后非空则优先，否则使用默认值。
func TestEnvOr(t *testing.T) {
	t.Setenv("XIANYU_TRAY_TEST_ENV", "  value  ")
	if got := envOr("XIANYU_TRAY_TEST_ENV", "fallback"); got != "value" {
		t.Fatalf("got=%q", got)
	}
	t.Setenv("XIANYU_TRAY_TEST_ENV", "   ")
	if got := envOr("XIANYU_TRAY_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("got=%q", got)
	}
}

// TestTrayIconBytesSelectsStateIcon 验证运行/未运行状态返回不同图标，且图标非空。
func TestTrayIconBytesSelectsStateIcon(t *testing.T) {
	// running、stopped 分别是运行与未运行状态图标。
	running, stopped := trayIconBytes(true), trayIconBytes(false)
	if len(running) == 0 || len(stopped) == 0 || bytes.Equal(running, stopped) {
		t.Fatalf("图标应非空且按状态区分: %d %d", len(running), len(stopped))
	}
}

// TestPNGToICOLayout 验证 ICO 头、目录项与 256/小尺寸的宽高编码。
func TestPNGToICOLayout(t *testing.T) {
	// png 是占位图像字节，仅用于验证容器封装。
	png := []byte{1, 2, 3, 4}
	// large 是 256 尺寸（宽高编码为 0）的 ICO。
	large := pngToICO(png, 256)
	if binary.LittleEndian.Uint16(large[2:4]) != 1 || binary.LittleEndian.Uint16(large[4:6]) != 1 || large[6] != 0 || large[7] != 0 {
		t.Fatalf("256 头部错误: %v", large[:8])
	}
	if binary.LittleEndian.Uint32(large[14:18]) != uint32(len(png)) || !bytes.Equal(large[22:], png) {
		t.Fatalf("数据区错误: %v", large)
	}
	// small 是 32 尺寸，宽高应直接写入尺寸。
	small := pngToICO(png, 32)
	if small[6] != 32 || small[7] != 32 {
		t.Fatalf("32 尺寸编码错误: %v", small[6:8])
	}
}
