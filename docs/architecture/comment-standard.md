# 注释规范

## 1. 原则

注释写给下一个读代码的人，只写代码本身表达不出来的信息：

- 业务含义或业务规则（为什么这样做，而不是做了什么）；
- 输入、输出、错误语义，以及单位、格式、默认值；
- 生命周期、取消、并发保护、锁顺序、goroutine 归属；
- 敏感数据的用途和禁止传播范围；
- 外部协议约束、兼容来源和删除条件；
- 采用非直观实现的原因。

不要为了“有注释”而写注释。复述标识符或语法的注释（如“err 表示错误”“x 用于本次流程后续判断的 x”）
比没有注释更糟，会被门禁拒绝。修改代码语义时同步更新相关注释，错误注释视为缺陷。

中文、英文均可，同一文件内尽量保持一致。注释中不得出现真实 Cookie、Token、密码、SMTP 凭证或生产密钥。

## 2. 必须写文档注释的声明

只要求对外可见的 API：

- Go 生产代码：导出的顶层函数、导出类型、导出类型上的导出方法。
  `Error`、`Unwrap`、`String`、`RoundTrip`、`ServeHTTP`、`MarshalJSON` 等标准接口方法可省略。
- TypeScript/TSX 生产代码：顶层 `export function`、`export class`、`export const`。

局部变量、结构体字段、循环变量、回调参数、未导出函数和测试代码不强制注释，按需说明。
含锁或 goroutine 的类型仍应说明锁保护哪些字段、goroutine 由谁启动和等待。

## 3. 自动检查

```bash
make comments
# 等价于：
go run ./tools/commentlint -mode check -root .
npm --prefix frontend run comments:check
```

检查内容：

1. 第 2 节列出的声明紧邻处存在非空注释；
2. 全仓库不存在已知的机械占位句式（见 `tools/commentlint/main.go` 与
   `frontend/scripts/check-comments.mjs` 中的 `templateCommentPatterns`）。

自动检查只能证明注释存在，语义是否准确由作者和评审负责。

生成代码、第三方代码、`internal/webui/static` 构建产物以及冻结的滑块/CAPTCHA 文件
（见 `docs/slider-captcha-frozen-spec.md`）不在检查范围内。
