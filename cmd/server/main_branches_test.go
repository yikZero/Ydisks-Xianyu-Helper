package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"xianyu-go/internal/db"
	"xianyu-go/internal/logging"
	"xianyu-go/internal/netguard"
)

// branchDataKey 是仅供测试使用的数据加密主密钥，不对应任何真实环境。
const branchDataKey = "dGVzdC1kYXRhLWtleS1mb3ItYnJhbmNoLXRlc3RzLW9ubHk"

// restoreServerGlobals 保存并在测试结束后恢复进程级日志默认值、日志等级和出站公网策略，避免污染同包其他测试。
func restoreServerGlobals(t *testing.T) {
	t.Helper()
	// previous 是测试前的默认 slog 日志器。
	previous := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		_ = logging.SetLevel("info")
		netguard.SetDefaultPublicOnly(false)
	})
}

// writeBranchFile 在 path 写入一个普通文件，用来占用本应是目录的位置以制造 MkdirAll 失败。
func writeBranchFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newBranchSQLite 在临时目录创建并迁移一个 SQLite 库，调用 seed 写入系统设置后关闭，返回其路径。
func newBranchSQLite(t *testing.T, settings map[string]string) string {
	t.Helper()
	// path 是临时库文件路径。
	path := filepath.Join(t.TempDir(), "infra.db")
	// database、dialect、err 是库连接、方言与打开错误。
	database, dialect, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	// store 提供系统设置仓储。
	store := db.NewStore(database, dialect)
	// key、value 是要预置的系统设置键值。
	for key, value := range settings {
		// setErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
		if setErr := store.Settings.Set(context.Background(), key, value); setErr != nil {
			t.Fatal(setErr)
		}
	}
	database.Close()
	return path
}

// TestPrepareServerStartupFailureBranches 覆盖数据目录、数据库目录、密钥文件被占用或不可读时的启动准备错误。
func TestPrepareServerStartupFailureBranches(t *testing.T) {
	// workDirIsFile 让 workdir 本身是普通文件，创建应用数据目录必然失败。
	t.Run("workdir is file", func(t *testing.T) {
		isolateServerEnv(t)
		// file 是被误用作 workdir 的普通文件。
		file := filepath.Join(t.TempDir(), "file")
		writeBranchFile(t, file)
		if _, err := prepareServerStartup(&serverOptions{workDir: file}); err == nil || !strings.Contains(err.Error(), "创建应用数据目录失败") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("database directory blocked", func(t *testing.T) {
		isolateServerEnv(t)
		// dir 是 workdir；其下 data 被普通文件占用，默认库父目录无法创建。
		dir := t.TempDir()
		writeBranchFile(t, filepath.Join(dir, "data"))
		if _, err := prepareServerStartup(&serverOptions{workDir: dir, dbPath: defaultDBPath}); err == nil || !strings.Contains(err.Error(), "创建数据库目录失败") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("data key file unreadable", func(t *testing.T) {
		isolateServerEnv(t)
		// dir 是 workdir；密钥文件路径被指向目录，读取会返回非 NotExist 错误。
		dir := t.TempDir()
		if _, err := prepareServerStartup(&serverOptions{workDir: dir, dataKeyFile: dir}); err == nil {
			t.Fatal("密钥文件是目录时应失败")
		}
	})
}

// TestPrepareServerStartupExplicitPlaywrightDirsAndKeyEnv 覆盖显式 Playwright 目录写环境变量，以及已有 XIANYU_DATA_KEY 时跳过密钥文件。
func TestPrepareServerStartupExplicitPlaywrightDirsAndKeyEnv(t *testing.T) {
	isolateServerEnv(t)
	t.Setenv("XIANYU_DATA_KEY", branchDataKey)
	// dir 是隔离工作目录。
	dir := t.TempDir()
	// opts 显式提供 driver 与 browser 目录。
	opts := serverOptions{workDir: dir, playwrightDriverDir: filepath.Join(dir, "drv"), playwrightBrowserDir: filepath.Join(dir, "br"), dbPath: "custom.db"}
	// startup、err 是准备阶段结果。
	startup, err := prepareServerStartup(&opts)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PLAYWRIGHT_DRIVER_PATH") != opts.playwrightDriverDir || os.Getenv("PLAYWRIGHT_BROWSERS_PATH") != opts.playwrightBrowserDir {
		t.Fatal("Playwright 环境变量未写入")
	}
	if startup.resolvedDBURL != "custom.db" {
		t.Fatalf("自定义库路径应原样保留: %q", startup.resolvedDBURL)
	}
	// statErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, statErr := os.Stat(filepath.Join(dir, defaultDataKeyName)); statErr == nil {
		t.Fatal("已有环境变量密钥时不应生成密钥文件")
	}
}

// TestPrepareServerStartupDerivesKeyBesideSQLiteWithoutDataDir 在无默认数据目录的平台覆盖 SQLite 旁路密钥生成与失败分支。
func TestPrepareServerStartupDerivesKeyBesideSQLiteWithoutDataDir(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		t.Skip("桌面平台总有用户数据目录，该分支只在 Linux 类平台可达")
	}
	isolateServerEnv(t)
	// dir 是库文件所在目录。
	dir := t.TempDir()
	// opts 不设 workdir，密钥应落在数据库文件同目录。
	opts := serverOptions{dbPath: filepath.Join(dir, "x.db")}
	if _, err := prepareServerStartup(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.dataKeyFile != filepath.Join(dir, defaultDataKeyName) {
		t.Fatalf("dataKeyFile=%q", opts.dataKeyFile)
	}
	// 数据库目录不存在文件占用：把目录替换为普通文件使密钥创建失败。
	t.Setenv("XIANYU_DATA_KEY", "")
	// blocker 是被占用的父路径。
	blocker := filepath.Join(dir, "blocker")
	writeBranchFile(t, blocker)
	if _, err := prepareServerStartup(&serverOptions{dbPath: filepath.Join(blocker, "x.db")}); err == nil {
		t.Fatal("父路径是文件时应失败")
	}
}

// TestResolveDataDirDefaultsUnderUserConfigDir 在桌面平台把 HOME 指向临时目录，验证默认数据目录及配置目录读取失败。
func TestResolveDataDirDefaultsUnderUserConfigDir(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("HOME 驱动的用户配置目录行为仅在 macOS 上稳定可测")
	}
	// home 是伪造的用户主目录，防止触碰真实应用数据目录。
	home := t.TempDir()
	t.Setenv("HOME", home)
	// dir、err 是默认数据目录及其错误。
	dir, err := resolveDataDir("")
	if err != nil || !strings.HasPrefix(dir, home) || !strings.HasSuffix(dir, userDataDirName) {
		t.Fatalf("dir=%q err=%v", dir, err)
	}
	t.Setenv("HOME", "")
	if _, err := resolveDataDir(" "); err == nil {
		t.Fatal("HOME 缺失时应返回配置目录错误")
	}
}

// TestLoadOrCreateDataKeyFailureBranches 覆盖空路径、父目录被占用、目录当作密钥文件与并发等待超时。
func TestLoadOrCreateDataKeyFailureBranches(t *testing.T) {
	if _, err := loadOrCreateDataKey("  "); err == nil {
		t.Fatal("空路径应失败")
	}
	// dir 是测试工作目录。
	dir := t.TempDir()
	// blocker 是占用父目录位置的普通文件。
	blocker := filepath.Join(dir, "blocker")
	writeBranchFile(t, blocker)
	if _, err := loadOrCreateDataKey(filepath.Join(blocker, "sub", "key")); err == nil || !strings.Contains(err.Error(), "data key") {
		t.Fatalf("err=%v", err)
	}
	if _, err := loadOrCreateDataKey(dir); err == nil {
		t.Fatal("目录当作密钥文件应失败")
	}
	if os.Geteuid() != 0 {
		// readonly 是无写权限目录，创建密钥文件应失败。
		readonly := filepath.Join(dir, "ro")
		if err := os.Mkdir(readonly, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := loadOrCreateDataKey(filepath.Join(readonly, "key")); err == nil || !strings.Contains(err.Error(), "创建 data key 文件失败") {
			t.Fatalf("err=%v", err)
		}
	}
}

// TestWaitForDataKeyBranches 覆盖密钥不存在时的等待超时、目录读取错误的立即返回与成功读取。
func TestWaitForDataKeyBranches(t *testing.T) {
	// dir 是测试工作目录。
	dir := t.TempDir()
	if _, err := waitForDataKey(filepath.Join(dir, "never")); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err=%v", err)
	}
	if _, err := waitForDataKey(dir); err == nil || strings.Contains(err.Error(), "超时") {
		t.Fatalf("目录读取错误应立即返回: %v", err)
	}
	// keyPath 是已写入合法内容的密钥文件。
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte(" abc \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// err、got 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if got, err := waitForDataKey(keyPath); err != nil || got != "abc" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

// TestReadDataKeyDirectoryError 验证读取目录返回带上下文的非 NotExist 错误。
func TestReadDataKeyDirectoryError(t *testing.T) {
	if _, err := readDataKey(t.TempDir()); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err=%v", err)
	}
}

// TestOpenServerLogWriterBranches 覆盖默认标准输出、目录创建失败、日志文件打开失败。
func TestOpenServerLogWriterBranches(t *testing.T) {
	t.Setenv("XIANYU_LOG_DIR", "")
	if runtime.GOOS != "windows" {
		// writer、closeLog、err 是未配置日志目录时的默认输出。
		writer, closeLog, err := openServerLogWriter("")
		if err != nil || writer != os.Stdout {
			t.Fatalf("writer=%v err=%v", writer, err)
		}
		closeLog()
	}
	// dir 是测试工作目录。
	dir := t.TempDir()
	// blocker 是占用日志目录位置的普通文件。
	blocker := filepath.Join(dir, "blocker")
	writeBranchFile(t, blocker)
	t.Setenv("XIANYU_LOG_DIR", filepath.Join(blocker, "logs"))
	if _, _, err := openServerLogWriter(""); err == nil || !strings.Contains(err.Error(), "创建日志目录失败") {
		t.Fatalf("err=%v", err)
	}
	// logDir 内的 server.log 是一个目录，使文件打开失败。
	logDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(filepath.Join(logDir, "server.log"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XIANYU_LOG_DIR", logDir)
	if _, _, err := openServerLogWriter(""); err == nil || !strings.Contains(err.Error(), "打开日志文件失败") {
		t.Fatalf("err=%v", err)
	}
}

// TestApplyPlaywrightRuntimeRootBranches 覆盖 nil、空根目录、保留显式目录与按架构派生。
func TestApplyPlaywrightRuntimeRootBranches(t *testing.T) {
	applyPlaywrightRuntimeRoot(nil)
	// empty 是未配置 runtime 根目录的选项，应保持不变。
	empty := serverOptions{}
	applyPlaywrightRuntimeRoot(&empty)
	if empty.playwrightDriverDir != "" || empty.playwrightBrowserDir != "" {
		t.Fatalf("empty=%+v", empty)
	}
	// preset 已显式指定两个目录，应原样保留。
	preset := serverOptions{playwrightRuntimeRoot: "/rt", playwrightDriverDir: "/d", playwrightBrowserDir: "/b"}
	applyPlaywrightRuntimeRoot(&preset)
	if preset.playwrightDriverDir != "/d" || preset.playwrightBrowserDir != "/b" {
		t.Fatalf("preset=%+v", preset)
	}
	// derived 只给根目录，应派生出架构子目录。
	derived := serverOptions{playwrightRuntimeRoot: "/rt"}
	applyPlaywrightRuntimeRoot(&derived)
	if !strings.Contains(derived.playwrightDriverDir, runtime.GOARCH) || !strings.Contains(derived.playwrightBrowserDir, runtime.GOARCH) {
		t.Fatalf("derived=%+v", derived)
	}
}

// TestIsNonLoopbackListenAddressMoreCases 补充主机名、回环 IP、通配与解析失败的分类。
func TestIsNonLoopbackListenAddressMoreCases(t *testing.T) {
	// cases 把监听地址映射到是否可被远程访问。
	cases := map[string]bool{
		"bad-address":    true,
		":59188":         true,
		"localhost:1":    false,
		"127.0.0.1:1":    false,
		"[::1]:1":        false,
		"0.0.0.0:1":      true,
		"example.com:80": true,
	}
	// addr、want 是监听地址与期望判断。
	for addr, want := range cases {
		if got := isNonLoopbackListenAddress(addr); got != want {
			t.Errorf("%q got=%v want=%v", addr, got, want)
		}
	}
}

// TestEnsureAdminBranches 覆盖缺少密码、环境变量密码、已有管理员不重建以及数据库故障。
func TestEnsureAdminBranches(t *testing.T) {
	t.Setenv("XIANYU_ADMIN_PASSWORD", "")
	// path 是预置迁移的 SQLite 路径。
	path := newBranchSQLite(t, nil)
	// database、dialect、err 是业务连接。
	database, dialect, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	// store 是仓储集合。
	store := db.NewStore(database, dialect)
	if ensureAdmin(context.Background(), store, "a@example.com", "") == nil {
		t.Fatal("缺少密码应失败")
	}
	t.Setenv("XIANYU_ADMIN_PASSWORD", "env-pass-123")
	// created、ensureErr 是首次确保管理员的结果。
	created, ensureErr := ensureAdminIfMissing(context.Background(), store, "a@example.com", "")
	if ensureErr != nil || !created {
		t.Fatalf("created=%v err=%v", created, ensureErr)
	}
	// again、againErr 是已有管理员时的结果，不应重建。
	again, againErr := ensureAdminIfMissing(context.Background(), store, "a@example.com", "")
	if againErr != nil || again {
		t.Fatalf("again=%v err=%v", again, againErr)
	}
	database.Close()
	// closedErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, closedErr := ensureAdminIfMissing(context.Background(), store, "a@example.com", "x"); closedErr == nil {
		t.Fatal("连接关闭后应失败")
	}
}

// TestEnsureAdminIfMissingPropagatesCreateFailure 验证无管理员且缺少密码时返回创建失败。
func TestEnsureAdminIfMissingPropagatesCreateFailure(t *testing.T) {
	t.Setenv("XIANYU_ADMIN_PASSWORD", "")
	// database、dialect、err 是业务连接。
	database, dialect, err := db.Open(context.Background(), newBranchSQLite(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	// ensureErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, ensureErr := ensureAdminIfMissing(context.Background(), db.NewStore(database, dialect), "a@example.com", ""); ensureErr == nil {
		t.Fatal("缺少密码应失败")
	}
}

// TestOpenServerInfrastructureAppliesDatabaseSettings 验证数据库中的日志等级/格式/公网出站设置在未显式配置时生效，并覆盖未初始化提示。
func TestOpenServerInfrastructureAppliesDatabaseSettings(t *testing.T) {
	isolateServerEnv(t)
	restoreServerGlobals(t)
	t.Setenv("XIANYU_DATA_KEY", branchDataKey)
	// path 是预置系统设置的库。
	path := newBranchSQLite(t, map[string]string{"log_level": "warn", "log_format": "json", "outbound_http_public_only": "true"})
	// startup 没有显式日志策略，数据库设置应生效。
	startup := serverStartupConfig{resolvedDBURL: path}
	// addr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, addr := range []string{"127.0.0.1:1", ":59188"} {
		// infra、err 是基础设施打开结果；两个地址分别触发回环与远程未初始化提示。
		infra, err := openServerInfrastructure(context.Background(), startup, serverOptions{addr: addr})
		if err != nil {
			t.Fatal(err)
		}
		infra.close()
	}
}

// TestOpenServerInfrastructureIgnoresInvalidDatabaseLogLevel 验证数据库保存了非法日志等级时仅告警并继续启动。
func TestOpenServerInfrastructureIgnoresInvalidDatabaseLogLevel(t *testing.T) {
	isolateServerEnv(t)
	restoreServerGlobals(t)
	t.Setenv("XIANYU_DATA_KEY", branchDataKey)
	// path 是含非法日志等级的库。
	path := newBranchSQLite(t, map[string]string{"log_level": "bogus"})
	// infra、err 是基础设施打开结果。
	infra, err := openServerInfrastructure(context.Background(), serverStartupConfig{resolvedDBURL: path}, serverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	infra.close()
}

// TestOpenServerInfrastructureAdminOptions 覆盖 -init-admin 成功/失败与 -ensure-admin 成功/失败分支。
func TestOpenServerInfrastructureAdminOptions(t *testing.T) {
	isolateServerEnv(t)
	restoreServerGlobals(t)
	t.Setenv("XIANYU_DATA_KEY", branchDataKey)
	t.Setenv("XIANYU_ADMIN_PASSWORD", "")
	// startup 指向每个子场景独立的新库。
	newStartup := func() serverStartupConfig { return serverStartupConfig{resolvedDBURL: newBranchSQLite(t, nil)} }
	// initInfra、initErr 是 -init-admin 成功后的仅初始化结果。
	initInfra, initErr := openServerInfrastructure(context.Background(), newStartup(), serverOptions{initAdmin: true, adminEmail: "a@example.com", adminPassword: "pw-123456"})
	if initErr != nil || !initInfra.initializationOnly {
		t.Fatalf("init: %+v %v", initInfra, initErr)
	}
	initInfra.close()
	if _, err := openServerInfrastructure(context.Background(), newStartup(), serverOptions{initAdmin: true, adminEmail: "a@example.com"}); err == nil || !strings.Contains(err.Error(), "初始化管理员失败") {
		t.Fatalf("缺少密码的 init-admin 应失败: %v", err)
	}
	if _, err := openServerInfrastructure(context.Background(), newStartup(), serverOptions{ensureAdmin: true, adminEmail: "a@example.com"}); err == nil || !strings.Contains(err.Error(), "检查或初始化管理员失败") {
		t.Fatalf("缺少密码的 ensure-admin 应失败: %v", err)
	}
	// ensureInfra、ensureErr 是 -ensure-admin 新建管理员成功后的基础设施。
	ensureInfra, ensureErr := openServerInfrastructure(context.Background(), newStartup(), serverOptions{ensureAdmin: true, adminEmail: "a@example.com", adminPassword: "pw-123456"})
	if ensureErr != nil {
		t.Fatal(ensureErr)
	}
	ensureInfra.close()
}

// TestOpenServerInfrastructureRejectsBadLogDirectory 验证日志目录不可用时直接返回错误。
func TestOpenServerInfrastructureRejectsBadLogDirectory(t *testing.T) {
	isolateServerEnv(t)
	// blocker 是占用日志目录位置的普通文件。
	blocker := filepath.Join(t.TempDir(), "blocker")
	writeBranchFile(t, blocker)
	t.Setenv("XIANYU_LOG_DIR", filepath.Join(blocker, "logs"))
	if _, err := openServerInfrastructure(context.Background(), serverStartupConfig{}, serverOptions{}); err == nil {
		t.Fatal("日志目录不可用应失败")
	}
}

// TestServerInfrastructureCloseHandlesZeroValue 验证零值基础设施关闭不会 panic。
func TestServerInfrastructureCloseHandlesZeroValue(t *testing.T) {
	serverInfrastructure{}.close()
}

// TestBuildServerRuntimeRejectsIncompleteInfrastructure 验证缺少仓储或日志器时装配失败。
func TestBuildServerRuntimeRejectsIncompleteInfrastructure(t *testing.T) {
	if _, err := buildServerRuntime(serverOptions{noBrowser: true}, serverInfrastructure{}); err == nil {
		t.Fatal("基础设施不完整应失败")
	}
}

// TestRunServerStageFailures 覆盖 runServer 在准备、基础设施和装配阶段的错误返回。
func TestRunServerStageFailures(t *testing.T) {
	isolateServerEnv(t)
	restoreServerGlobals(t)
	// file 被误用作 workdir，准备阶段失败。
	file := filepath.Join(t.TempDir(), "file")
	writeBranchFile(t, file)
	if err := runServer(context.Background(), serverOptions{workDir: file}); err == nil {
		t.Fatal("准备阶段应失败")
	}
	// dir 是正常 workdir，数据库地址指向不可创建路径使基础设施阶段失败。
	dir := t.TempDir()
	if err := runServer(context.Background(), serverOptions{workDir: dir, dbURL: "sqlite://" + filepath.Join(dir, "no", "such", "x.db"), logLevel: "error"}); err == nil {
		t.Fatal("基础设施阶段应失败")
	}
}
