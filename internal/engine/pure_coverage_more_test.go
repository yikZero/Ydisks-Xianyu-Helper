package engine

import (
	"context"
	"strings"
	"testing"
)

// TestEngineAITextHelpersCoverOfferAndTruncation 验证模型报价标记和 Unicode 文本截断边界。
func TestEngineAITextHelpersCoverOfferAndTruncation(t *testing.T) {
	// visible、price、ok 保存无报价标记文本的解析结果。
	if visible, price, ok := extractExecutableOffer("普通回复"); ok || price != 0 || visible != "普通回复" {
		t.Fatalf("无报价标记结果=%q %v %v", visible, price, ok)
	}
	// visible、ok 保存非法报价标记的清理结果和可执行标记。
	if visible, _, ok := extractExecutableOffer("[[AUTO_PRICE:bad]]"); ok || visible != "" {
		t.Fatalf("非法报价标记结果=%q %v", visible, ok)
	}
	// got 保存短文本和超长 Unicode 文本的截断结果。
	if got := truncateAIContent("短文本"); got != "短文本" || len([]rune(truncateAIContent(strings.Repeat("中", 2001)))) != 2000 {
		t.Fatal("模型文本截断异常")
	}
	// multipleOfferVisible 保存多个报价标记被全部移除后的正文。
	multipleOfferVisible, multipleOfferPrice, multipleOfferOK := extractExecutableOffer("[[AUTO_PRICE:90.00]] [[AUTO_PRICE:89.00]]")
	if multipleOfferVisible != "" || multipleOfferPrice != 0 || multipleOfferOK {
		t.Fatalf("多个报价标记不应执行：visible=%q price=%v ok=%v", multipleOfferVisible, multipleOfferPrice, multipleOfferOK)
	}
	if replyContainsOfferedPrice("没有明确报价", 90) {
		t.Fatal("没有数字报价的正文不应匹配")
	}
	if replyContainsOfferedPrice("报价 90 元", 91) {
		t.Fatal("不同金额不应匹配")
	}
	if minimumAllowedPrice(0, 10, 10, true) != 0 || minimumAllowedPrice(100, 10, 10, false) != 100 {
		t.Fatal("最低价无效或禁用折扣时边界异常")
	}
	// zeroMinimumUnsafe 表示最低价为零时的越界判定结果。
	_, zeroMinimumUnsafe := unsafeOfferedPrice("报价 89 元", 0)
	if zeroMinimumUnsafe {
		t.Fatal("最低价为零时不应判定越界")
	}
	// equalMinimumUnsafe 表示报价等于最低价时的越界判定结果。
	_, equalMinimumUnsafe := unsafeOfferedPrice("报价 90 元", 90)
	if equalMinimumUnsafe {
		t.Fatal("等于最低价不应判定越界")
	}
}

// TestEngineConversationContextCoversIdentifierAndStorageBranches 验证 AI 对话上下文在缺少会话标识、正常查询和查询取消时的分支。
func TestEngineConversationContextCoversIdentifierAndStorageBranches(t *testing.T) {
	// store、cleanup 保存 AI 对话查询使用的本地数据库及关闭函数。
	store, cleanup := newAIStore(t)
	defer cleanup()
	// replier 保存绑定测试账号的 AI 回复实现。
	replier := NewAIReplier("cid", store, nil)
	// noIdentityHistory、noIdentityCount、noIdentityBargain、noIdentityErr 保存缺少会话标识时的上下文结果。
	noIdentityHistory, noIdentityCount, noIdentityBargain, noIdentityErr := replier.conversationContext(context.Background(), ChatMessage{Text: "能便宜点吗"})
	if noIdentityErr != nil || noIdentityHistory != nil || noIdentityCount != 0 || !noIdentityBargain {
		t.Fatalf("缺少会话标识结果异常：history=%v count=%d bargain=%v err=%v", noIdentityHistory, noIdentityCount, noIdentityBargain, noIdentityErr)
	}
	// normalHistory、normalCount、normalBargain、normalErr 保存本地查询成功时的上下文结果。
	normalHistory, normalCount, normalBargain, normalErr := replier.conversationContext(context.Background(), ChatMessage{ChatID: "chat", ItemID: "item", Text: "在吗"})
	if normalErr != nil || normalHistory == nil || normalCount != 0 || normalBargain {
		t.Fatalf("正常上下文结果异常：history=%v count=%d bargain=%v err=%v", normalHistory, normalCount, normalBargain, normalErr)
	}
	// canceledContext 表示数据库查询已被调用方取消的上下文。
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	// canceledHistory、canceledCount、canceledBargain、canceledErr 保存取消查询结果。
	canceledHistory, canceledCount, canceledBargain, canceledErr := replier.conversationContext(canceledContext, ChatMessage{ChatID: "chat", ItemID: "item", Text: "在吗"})
	if canceledErr == nil || canceledHistory != nil || canceledCount != 0 || canceledBargain {
		t.Fatalf("取消上下文结果异常：history=%v count=%d bargain=%v err=%v", canceledHistory, canceledCount, canceledBargain, canceledErr)
	}
}
