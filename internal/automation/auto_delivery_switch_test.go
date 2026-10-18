package automation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// TestAutoDeliveryEnabledReadsAutoConfirmMasterSwitch 验证 autoDeliveryEnabled 读取的是自动发货总开关 auto_confirm，
// 与发货后确认开关 auto_consign 相互独立；账号缺失时返回带“自动发货设置”语义的错误，不失败开放。
func TestAutoDeliveryEnabledReadsAutoConfirmMasterSwitch(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	ctx := context.Background()
	// admin、adminErr 保存账号归属管理员及读取错误。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// center 是仅使用仓储读取开关的最小自动化中心。
	center := &Center{store: store}
	// on、off 是写入账号设置的开关取值。
	on, off := true, false
	// _、settingsErr 保存“只开确认发货、不开自动发货”的设置写入结果。
	if _, settingsErr := store.Cookies.UpdateSettings(ctx, "cid", db.AccountSettingsUpdate{UserID: admin.ID, AutoConfirm: &off, AutoConsign: &on}); settingsErr != nil {
		t.Fatal(settingsErr)
	}
	// enabled、err 保存只开确认发货时的总开关读取结果，必须为关闭。
	if enabled, err := center.autoDeliveryEnabled(ctx, "cid"); err != nil || enabled {
		t.Fatalf("auto_consign 不应被当作自动发货总开关 enabled=%v err=%v", enabled, err)
	}
	// _、settingsErr 保存“只开自动发货、不开确认发货”的设置写入结果。
	if _, settingsErr := store.Cookies.UpdateSettings(ctx, "cid", db.AccountSettingsUpdate{UserID: admin.ID, AutoConfirm: &on, AutoConsign: &off}); settingsErr != nil {
		t.Fatal(settingsErr)
	}
	// enabled、err 保存只开自动发货时的总开关读取结果，必须为开启。
	if enabled, err := center.autoDeliveryEnabled(ctx, "cid"); err != nil || !enabled {
		t.Fatalf("auto_confirm 开启时应允许自动发货 enabled=%v err=%v", enabled, err)
	}
	// enabled、err 保存不存在账号的读取结果，必须返回错误且保持关闭。
	enabled, err := center.autoDeliveryEnabled(ctx, "missing-account")
	if enabled || !errors.Is(err, db.ErrNotFound) || !strings.Contains(err.Error(), "读取自动发货设置") {
		t.Fatalf("缺失账号应返回自动发货设置读取错误 enabled=%v err=%v", enabled, err)
	}
}
