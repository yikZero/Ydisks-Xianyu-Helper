// Package mtop: 商品列表域 — mtop.idle.web.xyh.item.list 调用、分页与解析。
package mtop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xianyu-go/internal/xianyu/protocol"
)

// FetchItemsPage 获取指定页卖家在售商品列表。
func (c *ClientImpl) FetchItemsPage(ctx context.Context, cookiesStr string, pageNumber, pageSize int) (*ItemListResult, error) {
	return c.fetchItemsPage(ctx, cookiesStr, pageNumber, pageSize)
}

// fetchItemsPage 执行商品列表分页请求；平台成功但缺少 cardList 的响应统一表示当前页没有商品。
func (c *ClientImpl) fetchItemsPage(ctx context.Context, cookiesStr string, pageNumber, pageSize int) (*ItemListResult, error) {
	if pageNumber < 1 {
		pageNumber = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	currentCookies := cookiesStr
	if session := cookieSessionFromContext(ctx); session != nil {
		currentCookies, _, _ = session.State()
	}
	var lastRet []string
	for attempt := 0; attempt < 4; attempt++ {
		res, ret, updatedCookies, err := c.fetchItemsPageOnce(ctx, currentCookies, pageNumber, pageSize)
		if err != nil {
			if !IsMTopTokenExpiredErr(err) {
				return nil, err
			}
			lastRet = mtopErrorRet(err)
		} else {
			lastRet = ret
			if res != nil {
				return res, nil
			}
			// failure 保存平台非成功响应的统一失败分类。
			failure := c.mtopResponseFailure("商品列表接口", http.StatusOK, ret, "")
			if !IsMTopTokenExpiredErr(failure) {
				return nil, failure
			}
		}
		if updatedCookies != "" {
			// tokenChanged 表示响应 Cookie 是否真的轮换了签名令牌；普通 Cookie 变化不能跳过主动刷新。
			tokenChanged := mtopTokenCookieChanged(currentCookies, updatedCookies)
			currentCookies = updatedCookies
			if tokenChanged {
				if err := sleepCtx(ctx, MTopRetryGap); err != nil {
					return nil, err
				}
				continue
			}
		}
		if err := sleepCtx(ctx, MTopRetryGap); err != nil {
			return nil, err
		}
		refreshed, err := c.RefreshTokenContext(ctx, currentCookies)
		if err != nil {
			return nil, fmt.Errorf("刷新 mtop token 失败: %w", err)
		}
		currentCookies = refreshed.UpdatedCookies
	}
	return nil, fmt.Errorf("商品列表接口 token 重试失败: %w", c.mtopResponseFailure("商品列表接口", http.StatusOK, lastRet, "重试次数已耗尽"))
}

// fetchItemsPageOnce 执行一次商品列表请求并校验响应；SUCCESS 且缺少 cardList 的平台约定表示空商品页。
func (c *ClientImpl) fetchItemsPageOnce(ctx context.Context, cookiesStr string, pageNumber, pageSize int) (*ItemListResult, []string, string, error) {
	hc := c.httpClient()
	signingCookies, requestCookies := mtopRequestCookies(ctx, cookiesStr, "https://www.goofish.com/", ItemListAPI)
	cookies := protocol.TransCookies(signingCookies)
	userID := cookies["unb"]
	if userID == "" {
		return nil, nil, cookiesStr, fmt.Errorf("cookie 缺少 unb 字段，无法获取商品列表")
	}

	data := map[string]any{
		"needGroupInfo": false,
		"pageNumber":    pageNumber,
		"pageSize":      pageSize,
		"groupName":     "在售",
		"groupId":       "58877261",
		"defaultGroup":  true,
		"userId":        userID,
	}
	rawData, _ := json.Marshal(data)
	dataVal := string(rawData)
	t := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sign := protocol.GenerateSign(t, protocol.SignToken(signingCookies), dataVal)
	query := buildItemListQuery(t, sign)
	body := "data=" + url.QueryEscape(dataVal)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ItemListAPI+"?"+query, strings.NewReader(body))
	if err != nil {
		return nil, nil, cookiesStr, err
	}
	setCommonHeaders(req, requestCookies)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, cookiesStr, fmt.Errorf("商品列表请求失败: %w", err)
	}
	defer resp.Body.Close()
	updated := absorbMTopResponseCookies(ctx, cookiesStr, resp)
	raw, err := readMTopBody(resp)
	if err != nil {
		return nil, nil, updated, c.mtopResponseFailureWithCause("商品列表接口", resp.StatusCode, nil, "读取响应失败", err)
	}

	var decoded struct {
		Ret  []string       `json:"ret"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, nil, updated, c.mtopResponseFailureWithCause("商品列表接口", resp.StatusCode, nil, "JSON 解析失败", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, decoded.Ret, updated, c.mtopResponseFailure("商品列表接口", resp.StatusCode, decoded.Ret, "HTTP 状态异常")
	}
	if !hasMTopSuccess(decoded.Ret) {
		return nil, decoded.Ret, updated, nil
	}

	// cards、cardsOK 保存平台返回的商品卡片及其数组类型；成功响应缺少该字段是账号无商品的正式协议形态。
	cards, cardsOK := decoded.Data["cardList"].([]any)
	// cardListPresent 区分平台省略字段与显式返回 null、对象等畸形类型，只有前者可归一为空页。
	_, cardListPresent := decoded.Data["cardList"]
	if !cardListPresent {
		cards = []any{}
		cardsOK = true
	}
	if !cardsOK {
		return nil, decoded.Ret, updated, fmt.Errorf("商品列表 cardList 类型无效或与平台总数不一致，未执行同步")
	}
	items := parseItemList(decoded.Data)
	if !validItemListCards(cards) {
		return nil, decoded.Ret, updated, fmt.Errorf("商品列表包含无法识别的商品，未执行同步")
	}
	totalCount, totalPages := itemListPagination(decoded.Data, pageNumber, pageSize)
	// reportedTotal 保留显式数组的完整性依据；成功省略数组时以空列表协议为准。
	reportedTotal := itemListReportedTotal(decoded.Data)
	if !cardListPresent {
		// 成功省略列表已经确认空页；不能再由 pageCount 推算出虚构商品数量或触发继续翻页。
		totalCount, totalPages, reportedTotal = 0, pageNumber, 0
	}
	return &ItemListResult{
		Items:              items,
		PageNumber:         pageNumber,
		PageSize:           pageSize,
		CurrentCount:       len(cards),
		TotalCount:         totalCount,
		ReportedTotalCount: reportedTotal,
		TotalPages:         totalPages,
		SavedCountHint:     len(items),
		UpdatedCookies:     updated,
	}, decoded.Ret, updated, nil
}

// FetchAllItems 自动分页获取卖家全部在售商品。maxPages <= 0 表示不限页。
func (c *ClientImpl) FetchAllItems(ctx context.Context, cookiesStr string, pageSize, maxPages int) (*ItemListResult, error) {
	return c.fetchAllItems(ctx, cookiesStr, pageSize, maxPages)
}

// fetchAllItems 执行商品全集查询；所有调用方对平台空商品页使用同一缺失 cardList 归一规则。
func (c *ClientImpl) fetchAllItems(ctx context.Context, cookiesStr string, pageSize, maxPages int) (*ItemListResult, error) {
	if pageSize <= 0 {
		pageSize = 20
	}
	currentCookies := cookiesStr
	if session := cookieSessionFromContext(ctx); session != nil {
		currentCookies, _, _ = session.State()
	}
	page := 1
	fetchedPages := 0
	var all []ItemListItem
	// seenIDs 检测重复页或跨页重复商品，无法证明全集完整时禁止本地 reconcile。
	seenIDs := make(map[string]struct{})
	// fetchedCount 是含平台占位卡的已取条数；expectedCount 仅使用平台明确给出的总量。
	fetchedCount, expectedCount := 0, 0
	for maxPages <= 0 || page <= maxPages {
		res, err := c.fetchItemsPage(ctx, currentCookies, page, pageSize)
		if err != nil {
			return nil, err
		}
		currentCookies = res.UpdatedCookies
		fetchedCount += res.CurrentCount
		if res.ReportedTotalCount > 0 {
			if expectedCount > 0 && expectedCount != res.ReportedTotalCount {
				return nil, fmt.Errorf("商品分页期间总数变化，请重新同步")
			}
			expectedCount = res.ReportedTotalCount
		}
		// item 是本页的一个远端商品；重复身份意味着分页快照不稳定。
		for _, item := range res.Items {
			// exists 表示此商品是否已经在先前页出现。
			if _, exists := seenIDs[item.ID]; exists {
				return nil, fmt.Errorf("商品分页重复，请重新同步")
			}
			seenIDs[item.ID] = struct{}{}
		}
		if res.CurrentCount == 0 && res.TotalPages > page {
			return nil, fmt.Errorf("商品分页提前返回空页，请重新同步")
		}
		all = append(all, res.Items...)
		fetchedPages = page
		if res.TotalPages > 0 && page >= res.TotalPages {
			break
		}
		if res.TotalPages <= 0 && res.CurrentCount < pageSize {
			break
		}
		if maxPages > 0 && page >= maxPages {
			return nil, fmt.Errorf("商品同步达到页数上限但尚未完成，请提高 max_pages 后重试")
		}
		page++
		if err := sleepCtx(ctx, ItemPageGap); err != nil {
			return nil, err
		}
	}
	if expectedCount > 0 && fetchedCount != expectedCount {
		return nil, fmt.Errorf("商品列表实际条数与平台总数不符，请重新同步")
	}
	return &ItemListResult{
		Items:          all,
		PageNumber:     1,
		PageSize:       pageSize,
		CurrentCount:   len(all),
		TotalCount:     len(all),
		TotalPages:     fetchedPages,
		SavedCountHint: len(all),
		UpdatedCookies: currentCookies,
	}, nil
}

// itemListReportedTotal 从 data 读取平台明确报告的非零商品总数，返回零表示未提供，不使用页数推算。
func itemListReportedTotal(data map[string]any) int {
	// key 是平台已知的总数字段别名。
	for _, key := range []string{"totalCount", "total_count", "total"} {
		// count 是此字段提供的商品总条数。
		if count := mtopInt(data[key]); count > 0 {
			return count
		}
	}
	return 0
}

// validItemListCards 校验 cards 每张卡片都包含可识别身份；auto_ 平台占位卡可过滤，但畸形商品不能假装不存在。
func validItemListCards(cards []any) bool {
	// rawCard 是本页待验证的卡片对象。
	for _, rawCard := range cards {
		// card、ok 保存外层卡片及其结构是否有效。
		card, ok := rawCard.(map[string]any)
		if !ok {
			return false
		}
		// data 保存平台商品正文；缺失时无法确认这是有效商品列表。
		data, _ := card["cardData"].(map[string]any)
		// params 保存平台详情跳转参数中的商品标识。
		params, _ := data["detailParams"].(map[string]any)
		if strings.TrimSpace(mtopString(params["itemId"])) == "" && strings.TrimSpace(mtopString(data["id"])) == "" {
			return false
		}
	}
	return true
}

func itemListPagination(data map[string]any, pageNumber, pageSize int) (totalCount, totalPages int) {
	for _, key := range []string{"totalCount", "total_count", "total"} {
		if value := mtopInt(data[key]); value > 0 {
			totalCount = value
			break
		}
	}
	for _, key := range []string{"pageCount", "page_count", "totalPages", "total_pages"} {
		if value := mtopInt(data[key]); value > 0 {
			totalPages = value
			break
		}
	}
	if totalPages == 0 && totalCount > 0 && pageSize > 0 {
		totalPages = (totalCount + pageSize - 1) / pageSize
	}
	if totalCount == 0 && totalPages > 0 && pageSize > 0 {
		totalCount = totalPages * pageSize
	}
	if totalPages > 0 && pageNumber > totalPages {
		totalPages = pageNumber
	}
	return totalCount, totalPages
}

func buildItemListQuery(t, sign string) string {
	parts := [][2]string{
		{"jsv", "2.7.2"},
		{"appKey", protocol.SignAppKey},
		{"t", t},
		{"sign", sign},
		{"v", "1.0"},
		{"type", "originaljson"},
		{"accountSite", "xianyu"},
		{"dataType", "json"},
		{"timeout", "20000"},
		{"api", "mtop.idle.web.xyh.item.list"},
		{"sessionOption", "AutoLoginOnly"},
		{"spm_cnt", "a21ybx.im.0.0"},
		{"spm_pre", "a21ybx.collection.menu.1.272b5141NafCNK"},
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p[0])
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}

func parseItemList(data map[string]any) []ItemListItem {
	cardList, _ := data["cardList"].([]any)
	items := make([]ItemListItem, 0, len(cardList))
	for _, rawCard := range cardList {
		card, ok := rawCard.(map[string]any)
		if !ok {
			continue
		}
		cardData, _ := card["cardData"].(map[string]any)
		if cardData == nil {
			continue
		}
		detailParams, _ := cardData["detailParams"].(map[string]any)
		itemID := mtopString(detailParams["itemId"])
		if itemID == "" {
			itemID = mtopString(cardData["id"])
		}
		if itemID == "" || strings.HasPrefix(itemID, "auto_") {
			continue
		}
		priceInfo, _ := cardData["priceInfo"].(map[string]any)
		price := mtopString(priceInfo["price"])
		priceText := mtopString(priceInfo["preText"]) + price
		picInfo, _ := cardData["picInfo"].(map[string]any)
		picURL := mtopString(picInfo["picUrl"])
		detailURL := mtopString(cardData["detailUrl"])
		detail := map[string]any{
			"title":           mtopString(cardData["title"]),
			"price":           price,
			"price_text":      priceText,
			"category_id":     mtopString(cardData["categoryId"]),
			"auction_type":    mtopString(cardData["auctionType"]),
			"item_status":     mtopInt(cardData["itemStatus"]),
			"detail_url":      detailURL,
			"web_url":         "https://www.goofish.com/item?id=" + itemID,
			"pic_info":        picInfo,
			"detail_params":   detailParams,
			"track_params":    cardData["trackParams"],
			"item_label_data": cardData["itemLabelDataVO"],
			"card_type":       mtopInt(card["cardType"]),
		}
		detailJSON, _ := json.Marshal(detail)
		items = append(items, ItemListItem{
			ID:          itemID,
			Title:       mtopString(cardData["title"]),
			Price:       price,
			PriceText:   priceText,
			CategoryID:  mtopString(cardData["categoryId"]),
			DetailURL:   detailURL,
			WebURL:      "https://www.goofish.com/item?id=" + itemID,
			PicURL:      picURL,
			ItemDetail:  string(detailJSON),
			AuctionType: mtopString(cardData["auctionType"]),
			ItemStatus:  mtopInt(cardData["itemStatus"]),
			IsMultiSpec: hasItemSKUField(detailParams),
		})
	}
	return items
}

// hasItemSKUField 判断商品列表卡片的 detailParams 是否明确标记为多规格。
// 平台以 isSKU 的真值表示多规格；字段缺失或显式 false 都表示普通单规格商品。
func hasItemSKUField(detailParams map[string]any) bool {
	if detailParams == nil {
		return false
	}
	// rawSKU 保存列表卡片携带的多规格标记；缺失键读取为 nil，由 mtopBool 归一化为 false。
	rawSKU := detailParams["isSKU"]
	return mtopBool(rawSKU)
}
