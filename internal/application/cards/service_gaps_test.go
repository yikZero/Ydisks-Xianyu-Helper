package cards

import (
	"context"
	"errors"
	"testing"
)

// TestServiceUpdateRejectsInvalidAPIConfig 验证 API 卡券更新时无法规范化的配置被转换为校验错误。
func TestServiceUpdateRejectsInvalidAPIConfig(t *testing.T) {
	// repository 持有一张既有 API 卡券。
	repository := &cardRepositoryStub{card: Card{ID: 5, UserID: 7, Name: "接口卡", Type: "api", Enabled: true}}
	// service 是绑定可观测仓储的卡券应用服务。
	service := NewService(repository)
	// err 是提交非 JSON 对象配置时的更新错误。
	err := service.Update(context.Background(), 7, 5, Draft{Name: "接口卡", Type: "api", APIConfig: "[1]"})
	// validationErr 用于确认错误类型是稳定的校验错误。
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("err=%v", err)
	}
}

// TestServiceUpdateDataMetadataValidation 验证仅更新数据卡元数据时仍校验名称与延时范围。
func TestServiceUpdateDataMetadataValidation(t *testing.T) {
	// repository 持有一张既有数据卡。
	repository := &cardRepositoryStub{card: Card{ID: 6, UserID: 7, Name: "库存卡", Type: "data", DataContent: "x", Enabled: true}}
	// service 是绑定可观测仓储的卡券应用服务。
	service := NewService(repository)
	// drafts 是两个不合法的元数据草稿：名称为空、延时越界。
	drafts := []Draft{
		{Name: "", Type: "data"},
		{Name: "库存卡", Type: "data", DelaySeconds: 3601},
		{Name: "库存卡", Type: "data", DelaySeconds: -1},
	}
	// draft 是当前不合法草稿。
	for _, draft := range drafts {
		err := service.Update(context.Background(), 7, 6, draft)
		// validationErr 用于确认错误类型。
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) {
			t.Fatalf("draft=%+v err=%v", draft, err)
		}
	}
	// validateDataMetadataDraft 对非 data 类型同样拒绝。
	if validateDataMetadataDraft(Draft{Name: "x", Type: "text"}) == nil {
		t.Fatal("非 data 类型应被拒绝")
	}
}
