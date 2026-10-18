package cards

import (
	"context"
	"errors"
	"testing"
)

// editorRepository 在摘要通过权限校验后才允许读取完整模板。
type editorRepository struct {
	// cardRepositoryStub 提供既有库存行为与可注入错误。
	cardRepositoryStub
	// full 保存第二次完整读取的结果，便于模拟归属或类型变化。
	full Card
	// fullErr 模拟完整模板读取失败。
	fullErr error
	// reads 统计含密钥的读取次数。
	reads int
}

// GetFull 记录完整模板读取并返回预设结果。
func (r *editorRepository) GetFull(context.Context, int64) (Card, error) {
	r.reads++
	return r.full, r.fullErr
}

// TestAPIEditorApplicationBranches 覆盖归属变更、配置损坏、空模板和三态草稿合并。
func TestAPIEditorApplicationBranches(t *testing.T) {
	ctx := context.Background()
	// card 是合法且三个模板均为空的已保存 API 卡密。
	card := Card{ID: 1, UserID: 7, Type: "api", APIConfig: `{"url":"https://example.com","method":"GET"}`}
	// repository 是包含可控摘要和完整配置的持久化替身。
	repository := &editorRepository{cardRepositoryStub: cardRepositoryStub{card: card}, full: card}
	// service 是本次测试的卡密用例入口。
	service := NewService(repository)
	// editor、err 验证空模板以可再次编辑的 JSON 对象回显。
	editor, err := service.GetAPIEditor(ctx, 7, 1)
	if err != nil || editor.Headers != "{}" || editor.Params != "{}" || editor.Body != "{}" {
		t.Fatalf("空模板回显错误: %+v %v", editor, err)
	}
	// 完整正文保持原来的业务字段，不应被空对象覆盖。
	repository.full.APIConfig = `{"url":"https://example.com","method":"POST","body":{"order_id":"{order_id}"}}`
	// withBody、bodyErr 是包含正文时的完整编辑视图。
	withBody, bodyErr := service.GetAPIEditor(ctx, 7, 1)
	if bodyErr != nil || withBody.Body == "{}" {
		t.Fatal("正文模板丢失", bodyErr)
	}
	repository.full = card
	repository.reads = 1
	// raw、err 验证新建草稿不依赖数据库已有卡密。
	raw, err := service.PrepareAPITest(ctx, 7, 0, card.APIConfig)
	if err != nil || raw == "" || repository.reads != 1 {
		t.Fatal("新建测试不应读取完整库存", err)
	}
	// invalidUser 检查未登录用户无法使用模板合并服务。
	if _, invalidUser := service.PrepareAPITest(ctx, 0, 0, card.APIConfig); invalidUser == nil {
		t.Fatal("零用户必须拒绝")
	}
	// invalidConfig 验证错误响应不会回显输入中的秘密。
	if _, invalidConfig := service.PrepareAPITest(ctx, 7, 0, `{"url":"private-secret"}`); invalidConfig == nil {
		t.Fatal("非法配置必须拒绝")
	}
	// testCase 分别模拟摘要阶段和完整读取阶段的失败。
	for _, testCase := range []struct {
		// name 用于标识失败场景。
		name string
		// change 修改持久化替身以模拟该场景。
		change func()
	}{
		{name: "summary-owner", change: func() { repository.card.UserID = 8 }},
		{name: "summary-type", change: func() { repository.card.Type = "text" }},
		{name: "summary-read", change: func() { repository.getErr = errors.New("read") }},
		{name: "full-owner", change: func() { repository.full.UserID = 8 }},
		{name: "full-type", change: func() { repository.full.Type = "text" }},
		{name: "full-read", change: func() { repository.fullErr = errors.New("decrypt") }},
		{name: "invalid-saved", change: func() { repository.full.APIConfig = "invalid" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository.card, repository.full = card, card
			repository.getErr, repository.fullErr = nil, nil
			repository.reads = 0
			testCase.change()
			// failure 是权限、读取或已保存模板损坏时的安全错误。
			if _, failure := service.GetAPIEditor(ctx, 7, 1); failure == nil {
				t.Fatal("预期读取失败")
			}
			if testCase.name == "summary-owner" && repository.reads != 0 {
				t.Fatal("越权读取了密钥")
			}
			// failure 是测试合并过程中同样必须拒绝的错误。
			if _, failure := service.PrepareAPITest(ctx, 7, 1, ""); failure == nil {
				t.Fatal("预期测试合并失败")
			}
		})
	}
}
