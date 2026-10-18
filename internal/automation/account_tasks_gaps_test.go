package automation

import (
	"context"
	"errors"
	"testing"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/mtop"
)

// scanGapRepo 在流程仓储基础上提供可控的启用任务列表与账号门禁结果，用于驱动 scanAccountTasks。
type scanGapRepo struct {
	// accountTaskFlowRepository 提供评价、擦亮与凭证读取的内存实现。
	*accountTaskFlowRepository
	// settings 是扫描时返回的启用账号任务配置。
	settings []db.AccountTaskSettings
	// pauseErr 是读取账号暂停状态时返回的错误，用于覆盖门禁读取失败。
	pauseErr error
}

// Enabled 返回测试预置的启用任务配置。
func (r scanGapRepo) Enabled(context.Context) ([]db.AccountTaskSettings, error) {
	return r.settings, nil
}

// IsPaused 返回账号未暂停，或预置的读取错误。
func (r scanGapRepo) IsPaused(context.Context, string) (bool, int64, error) {
	return false, 0, r.pauseErr
}

// Status 返回账号处于启用状态。
func (r scanGapRepo) Status(context.Context, string) (bool, error) {
	return true, nil
}

// sessionExpiredErr 构造平台明确的登录 Session 失效错误，用于触发凭证恢复路径。
func sessionExpiredErr() error {
	return &mtop.MTopResponseError{API: "账号任务", Kind: mtop.MTopErrorSessionExpired, Ret: []string{"FAIL_SYS_SESSION_EXPIRED::x"}}
}

// TestScanAccountTasksGapBranches 覆盖扫描器的门禁读取失败、Session 阻断读取失败与评价、擦亮失败日志分支。
func TestScanAccountTasksGapBranches(t *testing.T) {
	ctx := context.Background()
	// now 是与擦亮时间匹配的北京时间，使擦亮任务到期。
	now := beijingNow()
	// due 是同时开启评价与擦亮且擦亮已到期的账号配置。
	due := db.AccountTaskSettings{CookieID: "scan", AutoRateEnabled: true, AutoPolishEnabled: true, RateContent: "好评", PolishTime: now.Format("15:04")}
	// newRunner 创建带指定客户端与恢复器的扫描协调器。
	newRunner := func(repo scanGapRepo, client AccountTaskClient, recoverer CredentialRecoverer) *accountTaskCoordinator {
		// runner 是被测账号任务协调器，已注入内存仓储与平台客户端替身。
		runner := newAccountTaskFlowCoordinator(repo.accountTaskFlowRepository, client)
		runner.repository = repo
		runner.recoverer = func() CredentialRecoverer { return recoverer }
		return runner
	}
	// base 返回带有效凭证和一个待评价订单的流程仓储。
	base := func() *accountTaskFlowRepository {
		return &accountTaskFlowRepository{runtimeData: db.CookieRuntimeData{Value: "unb=1; _m_h5_tk=t_1;"}, dueOrderIDs: []string{"trade-1"}, claimed: true, immediateClaimed: true}
	}

	t.Run("账号状态读取失败", func(t *testing.T) {
		// repo 的暂停状态读取失败。
		repo := scanGapRepo{accountTaskFlowRepository: base(), settings: []db.AccountTaskSettings{due}, pauseErr: errors.New("状态不可读")}
		newRunner(repo, &accountTaskFlowClient{}, nil).scanAccountTasks(ctx)
	})
	t.Run("Session阻断指纹读取失败", func(t *testing.T) {
		// flow 的凭证读取失败，阻断检查无法比对指纹。
		flow := base()
		flow.runtimeDataErr = errors.New("凭证不可读")
		// runner 已记录该账号的失效指纹。
		runner := newRunner(scanGapRepo{accountTaskFlowRepository: flow, settings: []db.AccountTaskSettings{due}}, &accountTaskFlowClient{}, nil)
		runner.sessionExpired.Store("scan", "old-fingerprint")
		runner.scanAccountTasks(ctx)
	})
	t.Run("评价失败时记录告警且不执行擦亮后续", func(t *testing.T) {
		// flow 的评价候选读取失败。
		flow := base()
		flow.dueOrderIDsErr = errors.New("本地订单不可读")
		// onlyRate 只开启评价。
		onlyRate := due
		onlyRate.AutoPolishEnabled = false
		newRunner(scanGapRepo{accountTaskFlowRepository: flow, settings: []db.AccountTaskSettings{onlyRate}}, &accountTaskFlowClient{}, nil).scanAccountTasks(ctx)
	})
	t.Run("评价遇Session失效且续期成功", func(t *testing.T) {
		// client 的评价调用返回 Session 失效。
		client := &accountTaskFlowClient{rateErr: sessionExpiredErr()}
		// onlyRate 只开启评价。
		onlyRate := due
		onlyRate.AutoPolishEnabled = false
		newRunner(scanGapRepo{accountTaskFlowRepository: base(), settings: []db.AccountTaskSettings{onlyRate}}, client, accountTaskRecovererBoundary{success: true}).scanAccountTasks(ctx)
	})
	t.Run("评价Session失效未续期时擦亮被阻断", func(t *testing.T) {
		// client 的评价调用返回 Session 失效；恢复器拒绝续期。
		client := &accountTaskFlowClient{rateErr: sessionExpiredErr()}
		// flow 记录擦亮日期写入次数。
		flow := base()
		newRunner(scanGapRepo{accountTaskFlowRepository: flow, settings: []db.AccountTaskSettings{due}}, client, accountTaskRecovererBoundary{success: false}).scanAccountTasks(ctx)
		if flow.markPolishCalls != 0 {
			t.Fatalf("Session 阻断后不应执行擦亮: %d", flow.markPolishCalls)
		}
	})
	t.Run("擦亮遇Session失效并续期", func(t *testing.T) {
		// client 的商品列表返回 Session 失效。
		client := &accountTaskFlowClient{itemErr: sessionExpiredErr()}
		// onlyPolish 只开启擦亮。
		onlyPolish := due
		onlyPolish.AutoRateEnabled = false
		newRunner(scanGapRepo{accountTaskFlowRepository: base(), settings: []db.AccountTaskSettings{onlyPolish}}, client, accountTaskRecovererBoundary{success: true}).scanAccountTasks(ctx)
	})
	t.Run("擦亮普通失败记录告警", func(t *testing.T) {
		// client 的商品列表返回普通错误。
		client := &accountTaskFlowClient{itemErr: errors.New("平台繁忙")}
		// onlyPolish 只开启擦亮。
		onlyPolish := due
		onlyPolish.AutoRateEnabled = false
		newRunner(scanGapRepo{accountTaskFlowRepository: base(), settings: []db.AccountTaskSettings{onlyPolish}}, client, nil).scanAccountTasks(ctx)
	})
}

// TestAccountTaskRunGapBranches 覆盖评价与擦亮流程的存储缺失、扫描时间、终态写入与凭证读取失败。
func TestAccountTaskRunGapBranches(t *testing.T) {
	ctx := context.Background()
	// settings 是评价配置。
	settings := db.AccountTaskSettings{CookieID: "acct", RateContent: "好评"}
	// good 返回带有效凭证、一个待评价订单且可抢占的流程仓储。
	good := func() *accountTaskFlowRepository {
		return &accountTaskFlowRepository{runtimeData: db.CookieRuntimeData{Value: "unb=1; _m_h5_tk=t_1;"}, dueOrderIDs: []string{"trade-1"}, claimed: true, immediateClaimed: true}
	}

	t.Run("评价存储未初始化", func(t *testing.T) {
		// runner 没有仓储。
		runner := &accountTaskCoordinator{client: func() AccountTaskClient { return &accountTaskFlowClient{} }}
		// rateErr 是自动评价流程在本场景下返回的错误，用于断言失败是否落库并停止批次。
		if _, rateErr := runner.runAutoRate(ctx, settings); rateErr == nil {
			t.Fatal("应报存储未初始化")
		}
	})
	t.Run("没有候选订单时记录扫描时间", func(t *testing.T) {
		// flow 没有候选订单。
		flow := good()
		flow.dueOrderIDs = nil
		// rateErr 是自动评价流程在本场景下返回的错误，用于断言失败是否落库并停止批次；summary 是账号任务流程返回的统计摘要（发现、成功、失败数量）。
		if summary, rateErr := newAccountTaskFlowCoordinator(flow, &accountTaskFlowClient{}).runAutoRate(ctx, settings); rateErr != nil || summary.Found != 0 {
			t.Fatalf("summary=%+v err=%v", summary, rateErr)
		}
	})
	t.Run("凭证读取失败", func(t *testing.T) {
		// flow 的凭证读取失败。
		flow := good()
		flow.runtimeDataErr = errors.New("凭证不可读")
		// rateErr 是自动评价流程在本场景下返回的错误，用于断言失败是否落库并停止批次。
		if _, rateErr := newAccountTaskFlowCoordinator(flow, &accountTaskFlowClient{}).runAutoRate(ctx, settings); rateErr == nil {
			t.Fatal("凭证读取失败应报错")
		}
	})
	t.Run("评价完成后扫描时间保存失败", func(t *testing.T) {
		// flow 的扫描时间写入失败。
		flow := good()
		flow.markRateErr = errors.New("写入失败")
		// rateErr 是自动评价流程在本场景下返回的错误，用于断言失败是否落库并停止批次。
		if _, rateErr := newAccountTaskFlowCoordinator(flow, &accountTaskFlowClient{}).runAutoRate(ctx, settings); rateErr == nil {
			t.Fatal("扫描时间保存失败应报错")
		}
	})
	t.Run("评价失败且失败终态写入失败", func(t *testing.T) {
		// flow 的失败终态写入失败，补偿隔离写入成功。
		flow := good()
		flow.finishErrors = []error{errors.New("终态写入失败")}
		// client 的评价调用返回普通错误。
		client := &accountTaskFlowClient{rateErr: errors.New("平台拒绝")}
		// rateErr 是自动评价流程在本场景下返回的错误，用于断言失败是否落库并停止批次。
		if _, rateErr := newAccountTaskFlowCoordinator(flow, client).runAutoRate(ctx, settings); rateErr == nil {
			t.Fatal("终态写入失败应报错")
		}
	})
	t.Run("人工核对状态写入失败", func(t *testing.T) {
		// flow 的人工核对终态写入失败。
		flow := good()
		flow.finishErrors = []error{errors.New("核对状态写入失败")}
		// quarantineErr 是把账号任务运行隔离到人工核对状态时的返回错误。
		if quarantineErr := newAccountTaskFlowCoordinator(flow, nil).quarantineAccountTaskRun(ctx, "k", 0, 0, errors.New("本地故障")); !errors.Is(quarantineErr, errAutomationNeedsReview) {
			t.Fatalf("err=%v", quarantineErr)
		}
	})
	t.Run("擦亮空列表完成状态写入失败", func(t *testing.T) {
		// flow 的运行终态写入失败。
		flow := good()
		flow.finishErrors = []error{errors.New("终态写入失败")}
		// settings 是擦亮配置。
		polish := db.AccountTaskSettings{CookieID: "acct", AutoPolishEnabled: true, PolishTime: "00:00"}
		// polishErr 是每日擦亮流程在本场景下返回的错误，用于断言失败批次的收口方式。
		if _, polishErr := newAccountTaskFlowCoordinator(flow, &accountTaskFlowClient{}).runAutoPolish(ctx, polish, beijingNow(), false); polishErr == nil {
			t.Fatal("终态写入失败应报错")
		}
	})
	t.Run("擦亮全部成功后终态写入失败", func(t *testing.T) {
		// flow 的运行终态写入失败。
		flow := good()
		flow.finishErrors = []error{errors.New("终态写入失败")}
		// client 返回一件在售商品。
		client := &accountTaskFlowClient{itemResult: &mtop.ItemListResult{Items: []mtop.ItemListItem{{ID: "i1"}}}}
		// polish 是擦亮配置。
		polish := db.AccountTaskSettings{CookieID: "acct", AutoPolishEnabled: true, PolishTime: "00:00"}
		// polishErr 是每日擦亮流程在本场景下返回的错误，用于断言失败批次的收口方式。
		if _, polishErr := newAccountTaskFlowCoordinator(flow, client).runAutoPolish(ctx, polish, beijingNow(), false); polishErr == nil {
			t.Fatal("终态写入失败应报错")
		}
	})
	t.Run("擦亮遇Session失效停止批次", func(t *testing.T) {
		// client 返回一件商品，擦亮调用返回 Session 失效。
		client := &accountTaskFlowClient{itemResult: &mtop.ItemListResult{Items: []mtop.ItemListItem{{ID: "i1"}, {ID: "i2"}}}, polishErr: sessionExpiredErr()}
		// polish 是擦亮配置。
		polish := db.AccountTaskSettings{CookieID: "acct", AutoPolishEnabled: true, PolishTime: "00:00"}
		// polishErr 是每日擦亮流程在本场景下返回的错误，用于断言失败批次的收口方式；summary 是账号任务流程返回的统计摘要（发现、成功、失败数量）。
		if summary, polishErr := newAccountTaskFlowCoordinator(good(), client).runAutoPolish(ctx, polish, beijingNow(), false); polishErr == nil || summary.Failed != 1 {
			t.Fatalf("summary=%+v err=%v", summary, polishErr)
		}
	})
	t.Run("擦亮时间格式非法回退凌晨三点", func(t *testing.T) {
		// at4 是北京时间凌晨 4 点，晚于回退的 03:00；at2 是凌晨 2 点，早于回退时间。
		at4 := time.Date(2026, 1, 2, 4, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*60*60))
		// at2 是北京时间凌晨 2 点的时刻，早于回退的 03:00 擦亮时间。
		at2 := at4.Add(-2 * time.Hour)
		if !polishDue(db.AccountTaskSettings{PolishTime: "bad"}, at4) || polishDue(db.AccountTaskSettings{PolishTime: "bad"}, at2) {
			t.Fatal("非法时间应按 03:00 判定是否到期")
		}
	})
}
