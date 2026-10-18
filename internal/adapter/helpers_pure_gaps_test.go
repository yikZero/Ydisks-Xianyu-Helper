package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBatchPublishLocationAndCategoryParsing 覆盖批次发货地与默认类目配置的解析分支。
func TestBatchPublishLocationAndCategoryParsing(t *testing.T) {
	if _, err := batchPublishLocation("{"); err == nil {
		t.Fatal("损坏的发货地应失败")
	}
	// legacy 是历史 PascalCase 格式，应被兼容解析。
	legacy, legacyErr := batchPublishLocation(`{"DivisionID":"110000","City":"北京"}`)
	if legacyErr != nil || legacy.DivisionID != "110000" || legacy.City != "北京" {
		t.Fatalf("历史格式: %+v %v", legacy, legacyErr)
	}
	// 空配置按空对象处理，没有发货地时返回空结果。
	if empty, emptyErr := batchPublishLocation(" "); emptyErr != nil || empty != nil {
		t.Fatalf("空配置: %+v %v", empty, emptyErr)
	}
	// category、err 依次为：测试中的局部对象，承载被测流程的输入、依赖或输出；保存本步骤调用返回的错误，用于断言预期的成功或失败路径。
	if category, err := batchPublishCategory(`{}`); category != nil || err != nil {
		t.Fatalf("空对象: %+v %v", category, err)
	}
	// category、err 依次为：测试中的局部对象，承载被测流程的输入、依赖或输出；保存本步骤调用返回的错误，用于断言预期的成功或失败路径。
	if category, err := batchPublishCategory(`{"cat_id":"","cat_name":""}`); category != nil || err != nil {
		t.Fatalf("全空字段: %+v %v", category, err)
	}
	if _, err := batchPublishCategory("{"); err == nil {
		t.Fatal("损坏类目应失败")
	}
	if _, err := batchPublishCategory(`{"cat_id":"1"}`); err == nil {
		t.Fatal("不完整类目应失败")
	}
}

// TestParseBatchMoneyCentsAndSmallHelpers 覆盖金额解析、错误优先级、候选文本与状态上下文辅助函数。
func TestParseBatchMoneyCentsAndSmallHelpers(t *testing.T) {
	// cases 保存金额文本、期望分值与是否应失败。
	cases := []struct {
		/* 输入文本 */ raw string
		/* 期望分值 */ want int64
		/* 是否失败 */ fail bool
	}{
		{"", 0, false}, {"¥12", 1200, false}, {"-1.5", -150, false}, {"+3.05", 305, false}, {"1.2.3", 0, true},
		{"1.234", 0, true}, {"abc", 0, true}, {"1.x", 0, true},
	}
	// tc 当前表驱动用例，包含输入与期望输出。
	for _, tc := range cases {
		// got、err 是解析结果与错误。
		got, err := parseBatchMoneyCents(tc.raw)
		if (err != nil) != tc.fail || (!tc.fail && got != tc.want) {
			t.Fatalf("%q: %d %v", tc.raw, got, err)
		}
	}
	// base 是数据库错误，应优先于兜底错误。
	base := errors.New("db")
	if firstBatchError(base, io.EOF) != base || firstBatchError(nil, io.EOF) != io.EOF {
		t.Fatal("错误优先级错误")
	}
	if firstBatchNonEmpty(" ", "", "x", "y") != "x" || firstBatchNonEmpty(" ") != "" {
		t.Fatal("候选文本选择错误")
	}
	// ctx、cancel 分别来自有效父上下文与空父上下文。
	ctx, cancel := batchPublishStatusContext(context.Background())
	cancel()
	_ = ctx
	// done 是已取消的父上下文，应回退到独立的新上下文。
	done, doneCancel := context.WithCancel(context.Background())
	doneCancel()
	// fresh、freshCancel 依次为：测试中的局部对象，承载被测流程的输入、依赖或输出；测试中的局部对象，承载被测流程的输入、依赖或输出。
	fresh, freshCancel := batchPublishStatusContext(done)
	defer freshCancel()
	if fresh.Err() != nil {
		t.Fatal("已取消父上下文应得到独立新上下文")
	}
	// nilCancel、nilCtx 依次为：测试中的局部对象，承载被测流程的输入、依赖或输出；测试中的局部对象，承载被测流程的输入、依赖或输出。
	if nilCtx, nilCancel := batchPublishStatusContext(nilContext); nilCtx.Err() != nil {
		t.Fatal("空父上下文应得到有效上下文")
	} else {
		nilCancel()
	}
}

// TestRemoteCaptchaCallAndSolveEdgeCases 覆盖远程过滑块调用的编码/请求构造错误、响应超限与重取限制。
func TestRemoteCaptchaCallAndSolveEdgeCases(t *testing.T) {
	ctx := context.Background()
	// mode 控制本地远程服务的响应形状。
	mode := "big"
	// server 按 mode 返回超大正文、非法 JSON 或过期标记。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "big":
			_, _ = w.Write([]byte(strings.Repeat("x", remoteCaptchaResponseLimit+1)))
		case "bad":
			_, _ = w.Write([]byte("not json"))
		case "expired":
			_, _ = w.Write([]byte(`{"success":false,"data":{"url_expired":true}}`))
		}
	}))
	defer server.Close()
	// config 指向本地服务并开启 Cookie 透传。
	config := remoteCaptchaConfig{URL: server.URL, Secret: "s", PassCookies: true}
	if got := callRemoteCaptcha(ctx, http.DefaultClient, config, "a", "u", "c", "d"); got.status != remoteCaptchaFailed {
		t.Fatalf("超大响应: %+v", got)
	}
	mode = "bad"
	if got := callRemoteCaptcha(ctx, http.DefaultClient, config, "a", "u", "c", "d"); got.status != remoteCaptchaFailed {
		t.Fatalf("非法响应: %+v", got)
	}
	// 非法 URL 无法构造请求。
	if got := callRemoteCaptcha(ctx, http.DefaultClient, remoteCaptchaConfig{URL: "http://a\x7f"}, "a", "u", "c", "d"); got.status != remoteCaptchaFailed {
		t.Fatalf("非法 URL: %+v", got)
	}
	mode = "expired"
	// 过期且没有重取回调：直接失败。
	if _, handled, err := solveRemoteCaptcha(ctx, http.DefaultClient, config, "a", "u", "c", "d", nil); !handled || err == nil {
		t.Fatalf("无回调: %v %v", handled, err)
	}
	// 回调始终返回新链接但远程始终过期：超过两次重取后失败。
	refreshes := 0
	// provider 测试中的局部对象，承载被测流程的输入、依赖或输出。
	provider := func(context.Context, string) (string, bool, string, error) {
		refreshes++
		return "https://fresh", false, "", nil
	}
	// err、handled 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言。
	if _, handled, err := solveRemoteCaptcha(ctx, http.DefaultClient, config, "a", "u", "c", "d", provider); !handled || err == nil || refreshes != 2 {
		t.Fatalf("重取上限: %v %v refreshes=%d", handled, err, refreshes)
	}
	// 回调没有返回新链接也没有 token：失败。
	empty := func(context.Context, string) (string, bool, string, error) { return " ", false, "", nil }
	// err、handled 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；标记返回的会话变更是否已被处理，用于断言。
	if _, handled, err := solveRemoteCaptcha(ctx, http.DefaultClient, config, "a", "u", "c", "d", empty); !handled || err == nil {
		t.Fatalf("无新链接: %v %v", handled, err)
	}
	if hasX5Cookies(map[string]string{"a": "b", "x5sec": " "}) || !hasX5Cookies(map[string]string{"X5SEC": "v"}) {
		t.Fatal("x5 Cookie 判定错误")
	}
}

// TestLoadRemoteCaptchaConfigGuards 覆盖远程过滑块配置读取的存储缺失、密钥缺失与读取失败路径。
func TestLoadRemoteCaptchaConfigGuards(t *testing.T) {
	ctx := context.Background()
	if (&Adapter{}).loadRemoteCaptchaConfig(ctx, "cid") != nil {
		t.Fatal("缺存储应返回 nil")
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// adapter 绑定存储。
	adapter := New(store, nil, nil)
	if adapter.loadRemoteCaptchaConfig(ctx, "cid") != nil {
		t.Fatal("未配置应返回 nil")
	}
	if err := store.Settings.SetMany(ctx, map[string]string{"captcha.remote_service_url": "http://x", "captcha.remote_secret_key": "s"}); err != nil {
		t.Fatal(err)
	}
	// cfg 测试中的局部对象，承载被测流程的输入、依赖或输出。
	if cfg := adapter.loadRemoteCaptchaConfig(ctx, "cid"); cfg == nil || cfg.PassCookies {
		t.Fatalf("默认关闭 Cookie 透传: %+v", cfg)
	}
	// 账号不存在时敏感密钥读取失败，回退本机逻辑。
	if adapter.loadRemoteCaptchaConfig(ctx, "no-such") != nil {
		t.Fatal("密钥读取失败应返回 nil")
	}
	cleanup()
	if adapter.loadRemoteCaptchaConfig(ctx, "cid") != nil {
		t.Fatal("数据库故障应返回 nil")
	}
}

// nilContext 是刻意保持为 nil 的 Context，用于验证被测函数对空上下文的容错。
var nilContext context.Context
