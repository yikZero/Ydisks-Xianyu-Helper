package items

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// capturingPublisher 记录平台端口实际收到的发布输入，用于确认应用层汇总的库存。
type capturingPublisher struct {
	// received 是最近一次收到的发布输入。
	received PublishInput
}

// Publish 记录输入并返回无远端商品的空结果。
func (p *capturingPublisher) Publish(_ context.Context, input PublishInput) (PublishOutcome, error) {
	p.received = input
	return PublishOutcome{}, nil
}

// TestPublishSingleAggregatesSKUQuantity 验证多规格发布时顶层库存由各 SKU 库存求和得出，而不是信任表单顶层字段。
func TestPublishSingleAggregatesSKUQuantity(t *testing.T) {
	// publisher 记录平台端口收到的输入。
	publisher := &capturingPublisher{}
	// service、err 是待测应用服务。
	service, err := NewService(publisher, &fakeItemRepository{})
	if err != nil {
		t.Fatal(err)
	}
	// publishErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, publishErr := service.PublishSingle(context.Background(), PublishInput{Quantity: 99, SKUs: []PublishSKU{{Quantity: 2}, {Quantity: 3}}}); publishErr != nil {
		t.Fatal(publishErr)
	}
	if publisher.received.Quantity != 5 {
		t.Fatalf("quantity=%d", publisher.received.Quantity)
	}
}

// TestItemAISettingsGetReturnsStoredSettings 验证归属和商品存在校验通过后返回仓储中的配置。
func TestItemAISettingsGetReturnsStoredSettings(t *testing.T) {
	// repository 允许访问并持有一份配置。
	repository := &itemAISettingsRepositoryFake{owned: true, exists: true, settings: ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideEnabled}}
	// service、err 是待测应用服务。
	service, err := NewItemAISettingsService(repository)
	if err != nil {
		t.Fatal(err)
	}
	// got、getErr 是查询结果。
	got, getErr := service.Get(context.Background(), 1, " account ", " item ")
	if getErr != nil || got.Override != ItemAIOverrideEnabled {
		t.Fatalf("got=%+v err=%v", got, getErr)
	}
}

// finishGapRepository 在完成态仓储替身上叠加取消收口与批次启动明细的可控结果。
type finishGapRepository struct {
	// batchFinishCoverageRepository 提供批次收口记录与基础行为。
	*batchFinishCoverageRepository
	// cancelErr 是 FinalizeCanceled 返回的错误。
	cancelErr error
}

// FinalizeCanceled 返回预置的取消收口结果与错误。
func (r *finishGapRepository) FinalizeCanceled(context.Context, string, string) (bool, error) {
	r.canceledCalls++
	return r.canceledApplied, r.cancelErr
}

// newFinishGapRunner 构造使用 repository 的无等待批次 worker。
func newFinishGapRunner(t *testing.T, repository BatchRepository) *BatchRunner {
	t.Helper()
	// runner、err 是批次 worker 构造结果。
	runner, err := NewBatchRunner(repository, &batchRunnerPublisher{}, BatchRunOptions{Wait: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// newFinishGapRepository 构造指定状态的批次收口仓储替身。
func newFinishGapRepository(status string, configure func(*batchRunBranchRepository)) *finishGapRepository {
	// base 是基础批次状态仓储。
	base := &batchRunBranchRepository{batchRunnerRepository: &batchRunnerRepository{batch: BatchInfo{ID: "b", UserID: 7, Status: status, WorkerToken: "worker", UploadDir: "dir"}}}
	if configure != nil {
		configure(base)
	}
	return &finishGapRepository{batchFinishCoverageRepository: &batchFinishCoverageRepository{batchRunBranchRepository: base}}
}

// TestBatchRunnerFinishInterruptedErrorBranches 覆盖中断收口时取消写入失败、取消未生效、中断收口失败与租约丢失。
func TestBatchRunnerFinishInterruptedErrorBranches(t *testing.T) {
	ctx := context.Background()
	// boom 是仓储写入失败。
	boom := errors.New("write failed")
	// cancelFail 的取消终态写入失败。
	cancelFail := newFinishGapRepository("canceling", nil)
	cancelFail.cancelErr = boom
	if err := newFinishGapRunner(t, cancelFail).finishInterrupted(ctx, 7, "b", "worker"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// cancelLost 的取消终态未命中当前租约。
	cancelLost := newFinishGapRepository("canceling", nil)
	if err := newFinishGapRunner(t, cancelLost).finishInterrupted(ctx, 7, "b", "worker"); !errors.Is(err, ErrBatchLeaseLost) {
		t.Fatalf("err=%v", err)
	}
	// interruptFail 的中断终态写入失败。
	interruptFail := newFinishGapRepository("running", func(r *batchRunBranchRepository) { r.interruptedErr = boom })
	if err := newFinishGapRunner(t, interruptFail).finishInterrupted(ctx, 7, "b", "worker"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// interruptLost 的中断终态未命中当前租约。
	interruptLost := newFinishGapRepository("running", nil)
	if err := newFinishGapRunner(t, interruptLost).finishInterrupted(ctx, 7, "b", "worker"); !errors.Is(err, ErrBatchLeaseLost) {
		t.Fatalf("err=%v", err)
	}
}

// TestBatchRunnerFinishErrorBranches 覆盖正常收口时读取失败、取消写入失败、取消未生效、终态未命中与上传目录清理失败。
func TestBatchRunnerFinishErrorBranches(t *testing.T) {
	ctx := context.Background()
	// boom 是仓储写入失败。
	boom := errors.New("write failed")
	// readFail 的批次状态读取失败，错误链应保留原因。
	readFail := newFinishGapRepository("running", func(r *batchRunBranchRepository) { r.batchErr = boom })
	if err := newFinishGapRunner(t, readFail).finish(ctx, 7, "b", "worker"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// cancelFail 的取消终态写入失败。
	cancelFail := newFinishGapRepository("canceling", func(r *batchRunBranchRepository) { r.interruptedApplied = true })
	cancelFail.cancelErr = boom
	if err := newFinishGapRunner(t, cancelFail).finish(ctx, 7, "b", "worker"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// cancelLost 的取消终态未命中当前租约。
	cancelLost := newFinishGapRepository("canceling", nil)
	if err := newFinishGapRunner(t, cancelLost).finish(ctx, 7, "b", "worker"); !errors.Is(err, ErrBatchLeaseLost) {
		t.Fatalf("err=%v", err)
	}
	// finalizeLost 的正常终态未命中当前租约。
	finalizeLost := newFinishGapRepository("running", func(r *batchRunBranchRepository) { r.finalizeStatus = "completed" })
	if err := newFinishGapRunner(t, finalizeLost).finish(ctx, 7, "b", "worker"); !errors.Is(err, ErrBatchLeaseLost) {
		t.Fatalf("err=%v", err)
	}
	// deleteFail 的终态已写入但上传目录清理失败。
	deleteFail := newFinishGapRepository("running", func(r *batchRunBranchRepository) { r.deleteUploadErr = boom })
	if err := newFinishGapRunner(t, deleteFail).finish(ctx, 7, "b", "worker"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}

// emptyFinalizeManagementRepository 让空批次终态收口返回“未命中租约”，覆盖租约丢失分支。
type emptyFinalizeManagementRepository struct {
	// batchManagementRepositoryFake 提供批次管理基础行为。
	*batchManagementRepositoryFake
}

// FinalizeBatch 返回终态未命中当前租约。
func (r *emptyFinalizeManagementRepository) FinalizeBatch(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// TestBatchManagementEmptyBatchFinalizeFailures 覆盖启动与重试入口在空批次收口失败（含释放租约失败）与租约丢失时的返回。
func TestBatchManagementEmptyBatchFinalizeFailures(t *testing.T) {
	ctx := context.Background()
	// finalizeErr、releaseErr 分别是收口与释放租约的失败。
	finalizeErr, releaseErr := errors.New("finalize failed"), errors.New("release failed")
	// start 覆盖 StartBatch 入口。
	t.Run("start", func(t *testing.T) {
		// repository 的批次可启动但没有待处理行，且收口、释放都失败。
		repository := &batchManagementRepositoryFake{batch: BatchInfo{ID: "b", Status: "preview"}, claimed: true, finalizeErr: finalizeErr, failClaimErr: releaseErr}
		// service 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
		service := newBatchManagementServiceForTest(t, repository, &batchManagementRuntimeFake{})
		if _, err := service.StartBatch(ctx, 7, "b", time.Minute); !errors.Is(err, finalizeErr) || !errors.Is(err, releaseErr) {
			t.Fatalf("err=%v", err)
		}
		// lostService 的空批次终态未命中租约。
		lost := &emptyFinalizeManagementRepository{batchManagementRepositoryFake: &batchManagementRepositoryFake{batch: BatchInfo{ID: "b", Status: "preview"}, claimed: true}}
		// err、lostService 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
		lostService, err := NewBatchManagementService(lost, &batchManagementRuntimeFake{})
		if err != nil {
			t.Fatal(err)
		}
		// startErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
		if _, startErr := lostService.StartBatch(ctx, 7, "b", time.Minute); !errors.Is(startErr, ErrBatchLeaseLost) {
			t.Fatalf("err=%v", startErr)
		}
	})
	// retry 覆盖 RetryFailedBatch 入口。
	t.Run("retry", func(t *testing.T) {
		// repository 的失败批次重置后没有待处理行，且收口、释放都失败。
		repository := &batchManagementRepositoryFake{batch: BatchInfo{ID: "b", Status: "completed"}, claimed: true, finalizeErr: finalizeErr, failClaimErr: releaseErr}
		// service 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
		service := newBatchManagementServiceForTest(t, repository, &batchManagementRuntimeFake{})
		if _, err := service.RetryFailedBatch(ctx, 7, "b", time.Minute); err == nil || !strings.Contains(err.Error(), "收口重试空批次") {
			t.Fatalf("err=%v", err)
		}
		// lost 的空批次终态未命中租约。
		lost := &emptyFinalizeManagementRepository{batchManagementRepositoryFake: &batchManagementRepositoryFake{batch: BatchInfo{ID: "b", Status: "completed"}, claimed: true}}
		// err、lostService 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
		lostService, err := NewBatchManagementService(lost, &batchManagementRuntimeFake{})
		if err != nil {
			t.Fatal(err)
		}
		// retryErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
		if _, retryErr := lostService.RetryFailedBatch(ctx, 7, "b", time.Minute); !errors.Is(retryErr, ErrBatchLeaseLost) {
			t.Fatalf("err=%v", retryErr)
		}
	})
}

// lostFinalizeRecoveryRepository 让恢复服务的空批次终态收口返回“未命中租约”。
type lostFinalizeRecoveryRepository struct {
	// batchRecoveryRepositoryFake 提供恢复仓储基础行为。
	*batchRecoveryRepositoryFake
}

// FinalizeBatch 返回终态未命中当前租约。
func (r *lostFinalizeRecoveryRepository) FinalizeBatch(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}

// TestBatchRecoveryReportsLostLeaseOnEmptyBatch 验证恢复扫描遇到空批次终态未命中租约时汇总错误而不中断扫描。
func TestBatchRecoveryReportsLostLeaseOnEmptyBatch(t *testing.T) {
	// repository 持有一个无待处理明细的恢复批次。
	repository := &lostFinalizeRecoveryRepository{batchRecoveryRepositoryFake: &batchRecoveryRepositoryFake{batches: []BatchInfo{{ID: "empty", UserID: 1, Status: "running"}}}}
	// service、err 是恢复服务。
	service, err := NewBatchRecoveryService(repository, BatchRecoveryOptions{NewWorkerToken: func() string { return "w" }})
	if err != nil {
		t.Fatal(err)
	}
	// runErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if runErr := service.RecoverWithStarter(context.Background(), func(context.Context, int64, string, string) error { return nil }); !errors.Is(runErr, ErrBatchLeaseLost) {
		t.Fatalf("err=%v", runErr)
	}
}

// TestBatchRunnerFinishLeavesCanceledBatchUntouched 验证批次已是终态取消时正常收口直接返回，不再写入任何状态。
func TestBatchRunnerFinishLeavesCanceledBatchUntouched(t *testing.T) {
	// repository 的批次已经取消。
	repository := newFinishGapRepository("canceled", nil)
	if err := newFinishGapRunner(t, repository).finish(context.Background(), 7, "b", "worker"); err != nil {
		t.Fatalf("err=%v", err)
	}
	if repository.canceledCalls != 0 || repository.deleteCalls != 0 {
		t.Fatalf("不应再次收口: %+v", repository.batchFinishCoverageRepository)
	}
}
