package notifications

import (
	"context"
	"errors"
	"testing"
)

// TestGetChannelEditorGuardsAndLookupFailures 验证编辑器读取的参数守卫、仓储错误透传与越权（记录为空）拒绝。
func TestGetChannelEditorGuardsAndLookupFailures(t *testing.T) {
	ctx := context.Background()
	// nilService 是未初始化的渠道服务。
	var nilService *ChannelService
	if _, err := nilService.GetChannelEditor(ctx, 7, 1); !errors.Is(err, ErrChannelInvalidInput) {
		t.Fatalf("nil: %v", err)
	}
	// service 使用空仓储替身。
	service := NewChannelService(&channelRepositoryStub{}, nil)
	if _, err := service.GetChannelEditor(ctx, 0, 1); !errors.Is(err, ErrChannelInvalidInput) {
		t.Fatalf("userID: %v", err)
	}
	if _, err := service.GetChannelEditor(ctx, 7, 0); !errors.Is(err, ErrChannelInvalidInput) {
		t.Fatalf("channelID: %v", err)
	}
	// boom 是仓储查询失败。
	boom := errors.New("repository down")
	if _, err := NewChannelService(&channelRepositoryStub{err: boom}, nil).GetChannelEditor(ctx, 7, 1); !errors.Is(err, boom) {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := service.GetChannelEditor(ctx, 7, 1); !errors.Is(err, ErrChannelForbidden) {
		t.Fatalf("record nil: %v", err)
	}
}

// TestGetChannelEditorNonEmailAndRecipientFallback 验证非邮件渠道不解析配置，邮件渠道兼容 email 历史字段回退。
func TestGetChannelEditorNonEmailAndRecipientFallback(t *testing.T) {
	// webhook 渠道的配置含敏感地址，编辑视图不得解析它。
	webhook := &channelRepositoryStub{record: &ChannelRecord{ID: 1, Name: "hook", Type: "webhook", Config: `{"webhook_url":"https://fixture.invalid/x"}`}}
	// editor、err 是非邮件渠道的编辑视图。
	editor, err := NewChannelService(webhook, nil).GetChannelEditor(context.Background(), 7, 1)
	if err != nil || editor.ToEmail != "" || editor.Type != "webhook" {
		t.Fatalf("editor=%+v err=%v", editor, err)
	}
	// legacy 邮件渠道只含历史 email 字段。
	legacy := &channelRepositoryStub{record: &ChannelRecord{ID: 2, Name: "mail", Type: "email", Config: `{"email":" legacy@example.com "}`}}
	// legacyEditor、legacyErr 是回退后的编辑视图。
	legacyEditor, legacyErr := NewChannelService(legacy, nil).GetChannelEditor(context.Background(), 7, 2)
	if legacyErr != nil || legacyEditor.ToEmail != "legacy@example.com" {
		t.Fatalf("editor=%+v err=%v", legacyEditor, legacyErr)
	}
}
