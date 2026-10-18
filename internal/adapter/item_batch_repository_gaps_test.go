package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	itemapp "xianyu-go/internal/application/items"
)

// batchRepositoryCalls 对批量发布仓储的每个方法发起一次调用并返回各自错误，用于未装配与数据库故障的统一断言。
func batchRepositoryCalls(ctx context.Context, repository *ItemBatchRepository) map[string]error {
	// errs 按方法名保存调用返回的错误。
	errs := map[string]error{}
	errs["CreateBatch"] = repository.CreateBatch(ctx, itemapp.BatchPreviewPersistenceBatch{ID: "b"}, []itemapp.BatchPreviewRow{{RowNo: 1}})
	_, errs["PendingRows"] = repository.PendingRows(ctx, "b", false)
	_, errs["RenewBatchLease"] = repository.RenewBatchLease(ctx, "b", "w", 1)
	_, errs["ReservePublishSlot"] = repository.ReservePublishSlot(ctx, "b", "w", 0, 1)
	_, errs["GetBatch"] = repository.GetBatch(ctx, 1, "b")
	_, errs["ListBatchesForUser"] = repository.ListBatchesForUser(ctx, 1, 10)
	_, errs["ListBatchRows"] = repository.ListBatchRows(ctx, "b")
	_, errs["ExpiredUploadBatches"] = repository.ExpiredUploadBatches(ctx, "2000-01-01", 10)
	_, _, errs["RequestCancel"] = repository.RequestCancel(ctx, "b")
	errs["DeleteBatch"] = repository.DeleteBatch(ctx, 1, "b")
	errs["ResetFailed"] = repository.ResetFailed(ctx, "b")
	_, errs["FailClaimedBatch"] = repository.FailClaimedBatch(ctx, "b", "w")
	_, errs["RecoverableBatches"] = repository.RecoverableBatches(ctx, 1, 10)
	_, errs["FinalizeExpiredCancellation"] = repository.FinalizeExpiredCancellation(ctx, "b", 1)
	_, errs["ClaimBatch"] = repository.ClaimBatch(ctx, "b", "w", 1)
	errs["ResetInterrupted"] = repository.ResetInterrupted(ctx, "b")
	_, errs["ClaimRow"] = repository.ClaimRow(ctx, 1, "w")
	_, errs["BatchStatus"] = repository.BatchStatus(ctx, "b")
	_, errs["MarkClaimedRowFailed"] = repository.MarkClaimedRowFailed(ctx, 1, "w", "m", "k")
	_, errs["MarkClaimedRowSuccess"] = repository.MarkClaimedRowSuccess(ctx, 1, "w", "i", "u", "{}")
	errs["RecountBatch"] = repository.RecountBatch(ctx, "b")
	_, _, errs["FinalizeBatch"] = repository.FinalizeBatch(ctx, "b", "w")
	_, errs["FinalizeCanceled"] = repository.FinalizeCanceled(ctx, "b", "w")
	_, _, errs["FinalizeInterrupted"] = repository.FinalizeInterrupted(ctx, "b", "w", "m")
	errs["DeleteUpload"] = repository.DeleteUpload(ctx, "b", filepath.Join(os.TempDir(), "adapter-gap-nonexistent-upload"))
	return errs
}

// TestItemBatchRepositoryRejectsUnconfiguredAdapter 验证未装配的批量发布仓储在每个入口都返回稳定错误而不 panic。
func TestItemBatchRepositoryRejectsUnconfiguredAdapter(t *testing.T) {
	// nilRepository 是空指针适配器，nilStore 是缺少数据库入口的适配器。
	var nilRepository *ItemBatchRepository
	// repository 遍历两种未装配形态。
	for _, repository := range []*ItemBatchRepository{nilRepository, NewItemBatchRepository(nil)} {
		// err、name 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；当前被检查的调用或用例名称，用于失败时定位。
		for name, err := range batchRepositoryCalls(context.Background(), repository) {
			if err == nil {
				t.Fatalf("%s 未装配时应返回错误", name)
			}
		}
	}
}

// TestItemBatchRepositoryPropagatesDatabaseFailures 验证数据库不可用时各仓储入口把错误向上传播。
func TestItemBatchRepositoryPropagatesDatabaseFailures(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	cleanup()
	// repository 绑定已关闭的数据库。
	repository := NewItemBatchRepository(store)
	// err、name 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；当前被检查的调用或用例名称，用于失败时定位。
	for name, err := range batchRepositoryCalls(context.Background(), repository) {
		if err == nil {
			t.Fatalf("%s 数据库关闭后应返回错误", name)
		}
	}
}

// TestItemBatchRepositoryCreateBatchRejectsUnserializableRaw 验证原始行包含无法序列化的值时拒绝写入。
func TestItemBatchRepositoryCreateBatchRejectsUnserializableRaw(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// err 是含函数值的原始行序列化失败错误。
	err := NewItemBatchRepository(store).CreateBatch(context.Background(), itemapp.BatchPreviewPersistenceBatch{ID: "b"}, []itemapp.BatchPreviewRow{{RowNo: 1, Raw: map[string]any{"bad": func() {}}}})
	if err == nil {
		t.Fatal("不可序列化的原始行应失败")
	}
}
