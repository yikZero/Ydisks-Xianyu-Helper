package server

// 订单导入解析：支持 JSON / CSV / TSV / XLSX 四种格式，统一输出 []map[string]any。
// 表头经 normalizeImportHeader 归一（中英文别名 → 标准字段名），供 order_handlers 上层消费。
//
// 抽离自 order_handlers.go，便于独立单测。

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ---- XLSX OOXML 内部结构 ----

type xlsxWorksheet struct {
	SheetData struct {
		Rows []xlsxRow `xml:"row"`
	} `xml:"sheetData"`
}

type xlsxRow struct {
	Cells []xlsxCell `xml:"c"`
}

type xlsxCell struct {
	Ref       string `xml:"r,attr"`
	Type      string `xml:"t,attr"`
	Value     string `xml:"v"`
	InlineStr string `xml:"is>t"`
}

type xlsxSST struct {
	Items []struct {
		Inner string `xml:",innerxml"`
	} `xml:"si"`
}

// xlsxSharedStrings 读取 xl/sharedStrings.xml（共享字符串表），供 cell type="s" 引用。
func xlsxSharedStrings(zr *zip.Reader) ([]string, error) {
	for _, f := range zr.File {
		if f.Name != "xl/sharedStrings.xml" {
			continue
		}
		raw, err := readXLSXPart(f)
		if err != nil {
			return nil, err
		}
		var sst xlsxSST
		if err := xmlUnmarshalXLSX(raw, &sst); err != nil {
			return nil, err
		}
		out := make([]string, 0, len(sst.Items))
		for _, item := range sst.Items {
			out = append(out, xmlCharData(item.Inner))
		}
		return out, nil
	}
	return nil, nil
}

func readXLSXPart(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readLimitedXLSXXML(rc)
}

func readLimitedXLSXXML(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxXLSXXMLPartBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxXLSXXMLPartBytes {
		return nil, fmt.Errorf("xlsx 内部 XML 超过 %d MiB", maxXLSXXMLPartBytes>>20)
	}
	return raw, nil
}

func xmlUnmarshalXLSX(raw []byte, v any) error {
	return xml.Unmarshal(raw, v)
}

// xlsxCellValue 按 cell 类型取值：s=共享字符串索引、inlineStr=内联字符串、其余=原始值。
func xlsxCellValue(cell xlsxCell, shared []string) string {
	switch cell.Type {
	case "s":
		idx, _ := strconv.Atoi(strings.TrimSpace(cell.Value))
		if idx >= 0 && idx < len(shared) {
			return shared[idx]
		}
	case "inlineStr":
		return strings.TrimSpace(cell.InlineStr)
	}
	return strings.TrimSpace(cell.Value)
}

// xlsxCellIndex 把 cell ref 的列部分（如 "C3" → "C"）转为 0-based 列索引。
func xlsxCellIndex(ref string) int {
	idx := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		idx = idx*26 + int(r-'A'+1)
	}
	if idx == 0 {
		return 0
	}
	return idx - 1
}

// xmlCharData 从 innerxml 片段中抽取所有字符数据（拼接富文本格式的单元格内容）。
func xmlCharData(inner string) string {
	dec := xml.NewDecoder(strings.NewReader("<x>" + inner + "</x>"))
	var parts []string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if data, ok := tok.(xml.CharData); ok {
			parts = append(parts, string(data))
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

// ---- 表头归一 ----

// firstImportString 从归一化后的 map 中按候选 key 顺序取首个非空字符串。
func firstImportString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			s := strings.TrimSpace(stringFromAny(v))
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// stringFromAny 把 any 安全转为字符串（nil→""，数字/布尔格式化）。
func stringFromAny(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprint(x)
	}
}
