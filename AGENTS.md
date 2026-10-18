# AGENTS.md

闲鱼多账号助手：Go 后端 + React/Vite 前端，前端构建产物嵌入 Go 二进制。
本文只保留改代码时必须知道的事实和不可破坏的规则；细节见 `docs/`。

## 目录

- `cmd/server/` — 服务入口、管理员初始化（`/initialize` 与 `-init-admin`）。`cmd` 只做配置、依赖构造和生命周期，不写业务。
- `cmd/init-admin/`、`cmd/dbverify/`、`cmd/tray/`、`cmd/browser-install/` — 运维与桌面辅助程序。
- `internal/server/` — chi HTTP API 与 SPA 静态资源（只做 transport）。
- `internal/mcp/` — `/mcp` 端点（MCP Streamable HTTP），API Token 认证后把工具调用转发到 `/api/v1`，见 `docs/mcp.md`。
- `internal/application/` — 应用服务：用例编排、鉴权、事务边界。
- `internal/adapter/` — 装配层，把 engine/automation 的接口接到数据库、浏览器、平台和通知。
- `internal/account/` — 已启用账号的运行时监管。
- `internal/engine/` — 单账号 WebSocket 运行时、关键词/AI 回复、消息分发。
- `internal/automation/` — 自动化中心：付款发货、砍价免拼、评价赠品、求评价、兜底调度。
- `internal/xianyu/` — 平台协议：MTOP 签名请求、WebSocket、扫码登录、Cookie。
- `internal/browser/` — playwright-go 驱动的进程内 Chromium（验证、订单详情兜底）。
- `internal/db/` — SQLite/MySQL/PostgreSQL 访问、仓储实现、按方言的 Goose 迁移。
- `frontend/` — React 源码；`internal/webui/static/` 是构建产物，不要手改。
- `api/openapi.yaml` — `/api/v1/**` 与 `/health` 的唯一 HTTP 契约。

## 常用命令

```bash
make build            # go build ./cmd/server
make test             # go test ./...
make vet / make lint  # go vet / golangci-lint
make comments         # 注释检查
make api-check        # OpenAPI 与路由、生成类型的一致性
make architecture     # 包依赖边界检查
make check            # 提交前全套：fmt + architecture + api-check + vet + lint + test + comments
make smoke            # 构建并启动真实二进制做 HTTP 冒烟（临时 SQLite）
make frontend         # 构建前端到 internal/webui/static

go test ./internal/server -run TestName -v   # 单个测试
```

运行（默认 SQLite，MySQL/Postgres 用 `-db-url` 或 `DATABASE_URL`）：

```bash
go run ./cmd/server -db data/xianyu_data.db -addr :59188
go run ./cmd/server -init-admin -db data/xianyu_data.db -admin-password '...'   # 重置管理员
```

新数据库启动后打开管理页面完成首次初始化。除非用户明确要求或确认 Chromium 不可用，不要加 `-no-browser`。

前端开发：`cd frontend && npm install && npm run dev`，Vite 代理后端到 `localhost:59188`。
改了前端源码后要 `npm run build`，让嵌入资源保持最新。

## 不可破坏的业务规则

### 商品列表协议

商品列表接口在账号没有在售商品时，成功响应可能省略 `cardList`。**缺少 `cardList` 按空列表处理，不是错误**，
全量同步应正常完成。只有 `cardList` 存在但类型不对，或其他字段明确表示失败时才算异常。相关测试必须覆盖“成功且省略 `cardList`”。

### 自动发货与砍价免拼顺序

自动化交易流程由 WebSocket 系统消息驱动；按订单状态扫描的调度器只是丢消息后的兜底，不能抢跑或推断 WebSocket 阶段。

四条发货分支：

1. **常规**：付款 WS → 拉取订单金额、数量、规格 → 匹配发卡/模板动作 → 发卡成功 → 按“自动确认发货”开关确认发货。
2. **多规格**：同上，但只执行与订单完整 SKU 组合完全匹配的动作，不按部分规格或金额猜测。
3. **二人小刀，未开自动免拼**：忽略“我已小刀，待刀成”；收到“我已成功小刀，待发货”后按常规顺序发卡、确认。
4. **二人小刀，开启自动免拼**：“待刀成”WS 只调用一次免拼接口，此时严禁发卡、发模板或确认发货；
   必须等“待发货”WS 到达后才发卡，发卡成功后才允许确认发货。

- 免拼不是确认发货的替代；确认发货不得因订单是砍价订单而改调免拼接口。
- 兜底任务只能依据已持久化的阶段事实补做未完成的动作，不得调用免拼接口。砍价订单只有已记录免拼成功或已收到“待发货”阶段后才可补发卡和确认。
- 恢复历史失败运行前必须重新核对：运行来源合法、订单仍是 `pending_ship`、订单归属账号未变、账号仍开启自动发货。
  订单已取消/完成/发货则取消旧运行；事实缺失、快照不一致或外部结果不确定时停止自动重放，并发送“需要人工处理”通知。
- 账号开关命名：`auto_confirm` = 付款后**自动发货**总开关；`auto_consign` = 发卡成功后**自动确认发货**；`auto_bargain` = 自动免拼。

## 冻结的滑块验证码逻辑

滑块/CAPTCHA 实现已在生产冻结，行为定义见 `docs/slider-captcha-frozen-spec.md`。除非用户在当前任务中明确要求，
不要修改（包括重构、格式化、改选择器/轨迹/时序/重试/Cookie 合并/浏览器参数，或通过修改调用方间接改变其行为）：

- `internal/browser/slider.go`、`slider_test.go`
- `internal/browser/token_captcha.go`、`token_captcha_test.go`
- `internal/browser/token_captcha_fallback.go`、`token_captcha_fallback_integration_test.go`
- `internal/browser/token_captcha_orchestrator_test.go`

获得授权时，实现、测试和冻结规范一起更新。一次授权不延续到后续任务。

## 安全与敏感数据

- Cookie、Token、密码、SMTP 凭证等不得出现在日志、通知、API 错误、HTTP 响应、前端状态或测试失败输出中。
- 账号摘要和归属校验不得读取或解密凭证；凭证使用专用模型和专用仓储方法。
- 不要在持有凭证锁时做慢速外部 I/O。
- 唯一已批准例外：`GET /api/v1/cards/{card_id}/api-editor` 可向卡券所有者返回已保存的 API 卡请求模板（含第三方 API Key），
  要求登录、所有权与 `type=api` 校验、`Cache-Control: no-store`，界面默认遮罩敏感输入，关闭或切换时清空草稿。
  其他返回已存密钥的接口都需要单独获得批准。

## 架构约定

依赖方向以 `docs/architecture/dependency-rules.md` 为准，`make architecture` 会检查。要点：

- `internal/server` 只做 HTTP；新用例写在应用服务里，handler 不直接访问 `Store.DB`、开事务、调 MTOP/浏览器。
- `internal/db` 负责 SQL、方言、迁移和加密，不依赖上层。`internal/xianyu`、`internal/browser` 不写业务数据、不决定自动化规则。
- `internal/engine`、`internal/automation` 不依赖 server。
- 接口由使用方定义且保持最小；必需依赖通过构造函数传入，不用全局 service locator。

### HTTP API

- 新接口使用 `/api/v1` 前缀，请求/响应用具名 DTO，失败用统一错误信封和正确的 HTTP 状态码（不要 200 + `success:false`）。
- 新增或修改接口：先改 `api/openapi.yaml`，再 `make api-generate` 重新生成 `frontend/shared/api-contract/generated/schema.ts`（只读），
  并补一个真实 handler 的契约测试。`make api-check` 会检查漂移。
- 改路由前缀时同步 `frontend/vite.config.ts`、前端调用方和嵌入资源。

### 数据库

- 每个迁移在 `internal/db/migrations/{sqlite,mysql,postgres}` 三个方言中编号和最终 schema 保持一致。
- 不向上层暴露 `*sql.DB`；需要跨仓储原子操作时使用明确的 Unit of Work。
- 数据库行为变更要有 SQLite 测试；有条件时用 `make test-multidb` 或 `cmd/dbverify` 验证 MySQL/PostgreSQL。

### 前端

- 结构 `app -> features -> shared`：feature 之间不互相引用内部文件，shared 不引用 feature。
- 组件不直接 `fetch`/`axios`，通过 feature 的 API adapter 调用共享契约客户端；生成的类型不直接进入组件 state/props。
- 异步请求要处理取消或“只认最新一次”，避免过期响应覆盖当前状态。

### 并发与生命周期

- 后台 goroutine 要有明确的归属者、Context、取消路径和等待路径。
- 不在持有 mutex 时做网络、浏览器或等待用户的 I/O。
- 有并发访问的类型说明哪把锁保护哪些字段，以及加锁顺序。

## 注释与测试

- 注释规范见 `docs/architecture/comment-standard.md`：导出的函数/类型需要文档注释，其余按需；禁止复述标识符的占位注释。
- 新增或修改的确定性逻辑、分支和错误路径要有聚焦测试，优先用注入依赖、`httptest`、内存数据库。
- 不要为了通过而删除、跳过或弱化测试。只有确实依赖真实账号或外部平台的行为可以跳过。
- 不要顺手重排、重命名或格式化无关代码，也不要升级无关依赖；保留工作区中与任务无关的改动。

## 打包与部署

默认端口 `59188`。桌面包把服务绑定到 `127.0.0.1:59188`，服务与托盘是两个进程：

- Windows：`YdisksXianyuHelper` 服务 + `xianyu-tray.exe`，安装脚本 `packaging/windows/installer.iss`。
- macOS：两个 LaunchAgent（`com.ydisks.xianyu-helper.server` / `.tray`），用 `packaging/macos/build-pkg.sh` 打包，
  不要手工拷贝 Chromium/Playwright；runtime 不完整时脚本会调用 `prepare-runtime.sh`。
- Linux：按架构分别打 tar 包，`install.sh` 需 root，数据在 `/var/lib/ydisks-xianyu-helper`。
- Docker：基础镜像 `node:24-trixie-slim`，复用 CI 准备的 Playwright runtime。

所有桌面包都自带匹配的 Playwright driver、Chromium 和 headless shell，安装时不再下载浏览器。
CI 中各架构在原生 runner 上构建（不用 QEMU），在 Go/前端测试、真实 Chromium 启动和打包后健康检查都通过前不得发布 Docker manifest。
版本标签 `v*.*.*` 触发 `release.yml` 生成 GitHub Release。

## 历史文档

`docs/architecture/refactoring-master-plan.md` 与 `refactoring-progress.md` 记录已完成的六阶段重构及证据，仅作历史参考，
后续修改不再按阶段拆分提交。`tools/architecturecheck` 仍读取该计划的阶段表以启用全部门禁，不要删除其中的状态表。
