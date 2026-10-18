# 用 AI 管理卡密、商品和自动发货（MCP）

服务内置一个标准 MCP 端点（Streamable HTTP），AI 客户端用 API Token 连接后，可以查看账号、商品、订单，
维护卡密库存，并配置“付款后自动发货”规则。

## 1. 创建 API Token

在运行服务的机器上，用同一个数据库执行（服务运行中也可以执行）：

```bash
# 可修改数据的 Token（AI 能建卡密、补库存、绑定发货）
./xianyu-server -db data/xianyu_data.db -api-token-create grok -api-token-scope write

# 只读 Token（只能查看）
./xianyu-server -db data/xianyu_data.db -api-token-create grok-readonly -api-token-scope read

# 查看、吊销
./xianyu-server -db data/xianyu_data.db -api-token-list
./xianyu-server -db data/xianyu_data.db -api-token-revoke 1
```

明文 Token（`ydx_` 开头）只在创建时显示一次，数据库只保存摘要。Token 以创建它的用户（默认 `admin`）身份访问数据，
可以用 `-api-token-user` 指定其他用户。

## 2. 连接 AI 客户端

- 地址：`http://<服务地址>:59188/mcp`，例如 `http://192.168.31.5:59188/mcp`
- 传输方式：Streamable HTTP（只用 POST，返回 JSON）
- 认证：请求头 `Authorization: Bearer ydx_...`

通用 MCP 客户端配置示例：

```json
{
  "mcpServers": {
    "xianyu": {
      "type": "http",
      "url": "http://192.168.31.5:59188/mcp",
      "headers": { "Authorization": "Bearer ydx_替换成你的Token" }
    }
  }
}
```

注意：MCP 请求由 **MCP 客户端所在的机器** 发出。如果客户端跑在云端（例如由模型服务商的服务器代为调用远程 MCP），
它访问不到局域网地址，需要让客户端运行在局域网内，或自行做内网穿透并加 HTTPS。

## 3. 可用工具

| 工具 | 权限 | 作用 |
|---|---|---|
| `list_accounts` | 只读 | 账号列表及自动发货、自动确认发货、自动免拼开关 |
| `list_items` | 只读 | 本地商品列表，可按账号、标题关键词过滤 |
| `sync_items` | 写 | 从闲鱼重新同步某账号的在售商品 |
| `list_cards` | 只读 | 卡密组列表，data 类型显示剩余库存条数 |
| `get_card` | 只读 | 卡密组完整内容（库存最多返回 200 条） |
| `create_card` | 写 | 新建卡密组：text 固定文本 / data 逐条库存 / image 图片 |
| `append_card_stock` | 写 | 给 data 卡密组追加库存 |
| `list_delivery_rules` | 只读 | 付款发货规则及每个规格绑定的卡密组 |
| `preview_delivery` | 只读 | 预演某商品（某规格）付款后会发什么、库存够不够，不会真的发货 |
| `bind_item_delivery` | 写 | 把商品（可指定规格）绑定到卡密组；已有规则时替换同规格内容或追加新规格 |
| `set_delivery_rule_enabled` | 写 | 启用或停用一条发货规则 |
| `list_orders` | 只读 | 订单查询，不返回收货人姓名、电话和地址 |

工具没有删除能力，删除卡密组或规则请在管理后台操作。只读 Token 在 `tools/list` 中看不到写工具。

## 4. 实现说明

- 工具不直接访问数据库：每次调用都以 Token 所属用户身份，在进程内转发到现有 `/api/v1` 管理接口，
  复用其参数校验、归属检查和事务。
- 每次工具调用都会写一条审计日志（工具名、`mcp_client_id` 即 Token ID、用户、是否成功、耗时），不记录参数内容。
- `preview_delivery` 的规则选择与自动化中心一致：商品级规则优先，按优先级和 ID 取第一条；没有商品级规则时，
  只使用勾选了“适用于全部商品”的账号级规则。规格需与订单完全一致。
