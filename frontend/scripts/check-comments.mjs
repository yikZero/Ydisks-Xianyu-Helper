// check-comments 检查 TypeScript/TSX 生产源码中导出的函数、组件、常量和类是否带有文档注释，
// 并拒绝批量生成的占位注释。局部变量、成员、回调和测试文件不强制注释，按需说明即可。
import fs from "node:fs";
import path from "node:path";
import process from "node:process";
import ts from "typescript";

// DOC_PATTERN 用于识别包含实际文字（中文或英文）的注释文本。
const DOC_PATTERN = /[\p{Script=Han}A-Za-z]/u;

// TEMPLATE_COMMENT_PATTERNS 是禁止出现的机械注释句式，命中即说明注释只是复述了标识符。
const TEMPLATE_COMMENT_PATTERNS = [
  { name: '后续判断占位', expression: /用于本次流程后续判断的/u },
  { name: '遍历变量占位', expression: /表示当前遍历过程中的/u },
  { name: '业务协调占位', expression: /封装.{0,120}业务协调/u },
  { name: '保存当前处理流程', expression: /保存.{0,80}供当前处理流程使用/u },
  { name: '负责相关处理', expression: /负责.{0,80}相关处理/u },
  { name: '泛化回调职责', expression: /回调函数负责当前业务流程/u },
  { name: '泛化错误说明', expression: /表示错误/u },
  { name: '泛化数量说明', expression: /表示数量/u },
];

// parseArguments 解析注释门禁所需的少量命令行参数。
function parseArguments(argv) {
  // options 保存命令行参数及其默认值。
  const options = {
    mode: "check",
    root: ".",
  };
  for (let index = 0; index < argv.length; index += 1) {
    // argument 是当前处理的命令行参数。
    const argument = argv[index];
    if (argument === "--mode" || argument === "--root") {
      // value 是当前选项紧随其后的值。
      const value = argv[index + 1];
      if (!value) {
        throw new Error(`${argument} 缺少参数值`);
      }
      options[argument.slice(2)] = value;
      index += 1;
    }
  }
  if (options.mode !== "check" && options.mode !== "template-audit") {
    throw new Error(`不支持的模式：${options.mode}`);
  }
  return options;
}

// collectTemplateFindings 逐行收集 TypeScript/TSX 中不具备实际语义的历史模板注释。
function collectTemplateFindings(rootDirectory) {
  // findings 保存模板注释的相对路径、行号与命中规则，便于按文件替换。
  const findings = [];
  // filePath 是当前待审计的 TypeScript 或 TSX 源码文件。
  for (const filePath of collectSourceFiles(rootDirectory)) {
    // relativePath 是跨平台稳定的源码相对路径。
    const relativePath = path.relative(rootDirectory, filePath).split(path.sep).join("/");
    // lines 是保留原始行号的源码文本行集合。
    const lines = fs.readFileSync(filePath, "utf8").split(/\r?\n/u);
    // sourceLine、lineIndex 是当前待匹配的源码文本及其零基行号。
    for (const [lineIndex, sourceLine] of lines.entries()) {
      // pattern 是当前禁止模板句式的审计规则。
      for (const pattern of TEMPLATE_COMMENT_PATTERNS) {
        if (pattern.expression.test(sourceLine)) {
          findings.push({ file: relativePath, line: lineIndex + 1, kind: pattern.name, name: "template-comment" });
        }
      }
    }
  }
  return sortFindings(findings);
}

// collectSourceFiles 递归找到需要检查的 TypeScript 和 TSX 源文件。
function collectSourceFiles(rootDirectory) {
  // sourceFiles 保存按路径排序后的源码文件。
  const sourceFiles = [];
  // visit 递归遍历单个目录并跳过依赖与构建产物。
  function visit(directory) {
    // entries 是当前目录的文件系统条目。
    const entries = fs.readdirSync(directory, { withFileTypes: true });
    for (const entry of entries) {
      // absolutePath 是当前条目的绝对路径。
      const absolutePath = path.join(directory, entry.name);
      if (entry.isDirectory()) {
        if (["node_modules", "dist", "coverage", "generated"].includes(entry.name)) {
          continue;
        }
        visit(absolutePath);
        continue;
      }
      if (entry.isFile() && /\.(ts|tsx)$/.test(entry.name) && !entry.name.endsWith(".d.ts")) {
        sourceFiles.push(absolutePath);
      }
    }
  }
  visit(rootDirectory);
  return sourceFiles.sort();
}

// hasDocComment 判断顶层声明正上方或首行行尾是否存在文档注释（中文或英文均可）。
// 变量声明以所在的 VariableStatement 为锚点，使 `// 说明\nexport const a = 1;` 的注释覆盖 a。
function hasDocComment(sourceFile, node) {
  // anchor 是真正承载注释的语句节点。
  const anchor = ts.isVariableDeclaration(node) && ts.isVariableStatement(node.parent?.parent) ? node.parent.parent : node;
  const sourceText = sourceFile.getFullText();
  // startLine 是声明起始位置所在的零基行号。
  const startLine = sourceFile.getLineAndCharacterOfPosition(anchor.getStart(sourceFile)).line;
  const leadingRanges = ts.getLeadingCommentRanges(sourceText, anchor.getFullStart()) ?? [];
  if (leadingRanges.some((range) => {
    const commentEndLine = sourceFile.getLineAndCharacterOfPosition(range.end).line;
    return commentEndLine >= startLine - 1 && commentEndLine <= startLine && DOC_PATTERN.test(sourceText.slice(range.pos, range.end));
  })) {
    return true;
  }
  // 首行行尾注释，例如 `export const LIMIT = 20; // 每页数量`。
  const lineEnd = sourceText.indexOf("\n", anchor.getStart(sourceFile));
  const firstLine = sourceText.slice(anchor.getStart(sourceFile), lineEnd === -1 ? undefined : lineEnd);
  const trailing = firstLine.match(/\/\/[^\n]*$|\/\*.*\*\/\s*$/);
  return Boolean(trailing && DOC_PATTERN.test(trailing[0]));
}

// isExported 判断顶层语句是否带 export 修饰符。
function isExported(statement) {
  return Boolean(ts.canHaveModifiers(statement) && ts.getModifiers(statement)?.some((modifier) => modifier.kind === ts.SyntaxKind.ExportKeyword));
}

// getNodeName 返回 AST 节点的可读名称。
function getNodeName(node) {
  if (node.name) {
    return node.name.getText();
  }
  if (ts.isParameter(node) && node.name) {
    return node.name.getText();
  }
  return "anonymous";
}

// findingFor 将一个 AST 节点转换成稳定的门禁记录。
function findingFor(relativePath, sourceFile, node, kind) {
  // line 是声明所在的一基行号。
  const line = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile)).line + 1;
  return { file: relativePath, line, kind, name: getNodeName(node) };
}

// inspectSourceFile 检查一个文件的顶层导出声明并收集缺少文档注释的项。
function inspectSourceFile(rootDirectory, filePath) {
  // sourceText 是待解析文件内容。
  const sourceText = fs.readFileSync(filePath, "utf8");
  // sourceFile 是开启父节点信息的 TypeScript AST。
  const sourceFile = ts.createSourceFile(filePath, sourceText, ts.ScriptTarget.Latest, true, filePath.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  // relativePath 使门禁输出在不同机器上保持稳定。
  const relativePath = path.relative(rootDirectory, filePath).split(path.sep).join("/");
  // findings 保存当前文件的注释问题。
  const findings = [];
  // 测试文件不强制注释。
  if (/\.test\.(ts|tsx)$/.test(filePath)) {
    return findings;
  }
  for (const statement of sourceFile.statements) {
    if (!isExported(statement)) {
      continue;
    }
    if (ts.isFunctionDeclaration(statement) || ts.isClassDeclaration(statement)) {
      if (!hasDocComment(sourceFile, statement)) {
        findings.push(findingFor(relativePath, sourceFile, statement, ts.isClassDeclaration(statement) ? "class" : "function"));
      }
    } else if (ts.isVariableStatement(statement)) {
      for (const declaration of statement.declarationList.declarations) {
        if (!hasDocComment(sourceFile, declaration)) {
          findings.push(findingFor(relativePath, sourceFile, declaration, "variable"));
        }
      }
    }
  }
  return findings;
}

// findingKey 生成排序和输出使用的稳定键。
function findingKey(finding) {
  return `${finding.file}:${finding.line}:${finding.kind}:${finding.name}`;
}

// sortFindings 按文件、行号、类别和名称对结果排序。
function sortFindings(findings) {
  return findings.sort((left, right) => findingKey(left).localeCompare(findingKey(right)));
}

// main 执行前端源码严格注释检查或输出模板注释审计结果。
function main() {
  // options 保存解析后的命令行选项。
  const options = parseArguments(process.argv.slice(2));
  // rootDirectory 是规范化后的源码根目录。
  const rootDirectory = path.resolve(options.root);
  if (options.mode === "template-audit") {
    // templateFindings 是按稳定顺序排列的前端模板注释记录。
    const templateFindings = collectTemplateFindings(rootDirectory);
    for (const finding of templateFindings) {
      console.log(`${finding.file}:${finding.line}: [${finding.kind}] 模板化注释`);
    }
    console.log(`commentlint: 发现 ${templateFindings.length} 条模板化注释`);
    return;
  }
  // findings 是当前所有 TypeScript/TSX 文件的检查结果。
  const findings = sortFindings(collectSourceFiles(rootDirectory).flatMap((filePath) => inspectSourceFile(rootDirectory, filePath)));
  for (const finding of findings) {
    console.log(`${finding.file}:${finding.line}: [${finding.kind}] ${finding.name} 缺少文档注释`);
  }
  // templateFindings 是不允许保留的机械注释位置。
  const templateFindings = collectTemplateFindings(rootDirectory);
  for (const finding of templateFindings) {
    console.log(`${finding.file}:${finding.line}: [${finding.kind}] 模板化注释`);
  }
  if (findings.length > 0 || templateFindings.length > 0) {
    throw new Error(`发现 ${findings.length} 个缺少文档注释项和 ${templateFindings.length} 条模板化注释`);
  }
  console.log("commentlint: 通过（无缺少文档注释或模板化注释）");
}

try {
  main();
} catch (error) {
  console.error(`commentlint: ${error instanceof Error ? error.message : String(error)}`);
  process.exitCode = 1;
}
