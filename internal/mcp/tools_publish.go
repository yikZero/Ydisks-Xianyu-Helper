package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"xianyu-go/internal/publishimage"
)

// maxPublishImages 是闲鱼单个商品允许的图片数量上限。
const maxPublishImages = 9

// publishArguments 是 publish_item 的参数。
type publishArguments struct {
	AccountID           string      `json:"account_id"`
	Title               string      `json:"title"`
	Description         string      `json:"description"`
	Price               json.Number `json:"price"`
	OriginalPrice       json.Number `json:"original_price"`
	Quantity            int         `json:"quantity"`
	Images              []string    `json:"images"`
	CardID              int64       `json:"card_id"`
	DeliveryCount       int         `json:"delivery_count"`
	AllowDuplicateTitle bool        `json:"allow_duplicate_title"`
	Confirm             bool        `json:"confirm"`
}

// publishResponse 是发布接口成功响应中工具需要的字段。
type publishResponse struct {
	ItemID       string `json:"item_id"`
	ItemURL      string `json:"item_url"`
	ItemTitle    string `json:"item_title"`
	ItemPrice    string `json:"item_price"`
	Quantity     int    `json:"quantity"`
	CategoryName string `json:"category_name"`
}

// runPublishItem 发布新商品。confirm 为 false 时只校验参数、加载图片并返回预览，不会发布；
// confirm 为 true 时发布，并在给出 card_id 时绑定付款后自动发货。
func runPublishItem(client apiClient, arguments json.RawMessage) (any, error) {
	var args publishArguments
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	accountID, err := requireText(args.AccountID, "account_id")
	if err != nil {
		return nil, err
	}
	title, err := requireText(args.Title, "title")
	if err != nil {
		return nil, err
	}
	description, err := requireText(args.Description, "description")
	if err != nil {
		return nil, err
	}
	if price, parseErr := strconv.ParseFloat(args.Price.String(), 64); parseErr != nil || price <= 0 {
		return nil, errors.New("price 必须是大于 0 的金额，例如 9.9")
	}
	if args.OriginalPrice != "" {
		if _, parseErr := strconv.ParseFloat(args.OriginalPrice.String(), 64); parseErr != nil {
			return nil, errors.New("original_price 格式错误")
		}
	}
	if args.Quantity <= 0 {
		args.Quantity = 1
	}
	if len(args.Images) == 0 || len(args.Images) > maxPublishImages {
		return nil, fmt.Errorf("images 需要 1 到 %d 张图片", maxPublishImages)
	}
	if err := checkPublishAccount(client, accountID, title, args.AllowDuplicateTitle); err != nil {
		return nil, err
	}
	images, err := loadPublishImages(client, args.Images)
	if err != nil {
		return nil, err
	}
	preview := map[string]any{"account_id": accountID, "title": title, "description": description, "price": args.Price.String(),
		"quantity": args.Quantity, "postage": "包邮", "category": "按标题自动识别"}
	if args.OriginalPrice != "" {
		preview["original_price"] = args.OriginalPrice.String()
	}
	imageSummaries := make([]map[string]any, 0, len(images))
	for i, image := range images {
		imageSummaries = append(imageSummaries, map[string]any{"image": args.Images[i], "type": image.contentType, "size_kb": (len(image.data) + 1023) / 1024})
	}
	preview["images"] = imageSummaries
	if args.CardID > 0 {
		var card cardRow
		if err := client.do(http.MethodGet, "/api/v1/cards/"+strconv.FormatInt(args.CardID, 10), nil, nil, &card); err != nil {
			return nil, err
		}
		if !card.Enabled {
			return nil, fmt.Errorf("卡密组「%s」已停用，请先在管理后台启用", card.Name)
		}
		preview["auto_delivery_card"] = cardSummary(card)
	}
	if !args.Confirm {
		preview["published"] = false
		preview["next_step"] = "以上是预览，尚未发布。请把预览内容发给用户确认；用户同意后，用完全相同的参数加 confirm=true 再调用一次才会发布。"
		return preview, nil
	}
	fields := map[string]string{"cookie_id": accountID, "title": title, "description": description, "price": args.Price.String(),
		"quantity": strconv.Itoa(args.Quantity), "postage_mode": "free"}
	if args.OriginalPrice != "" {
		fields["original_price"] = args.OriginalPrice.String()
	}
	var published publishResponse
	if err := client.postForm("/api/v1/items/publish", fields, images, &published); err != nil {
		return nil, err
	}
	result := map[string]any{"published": true, "item_id": published.ItemID, "item_url": published.ItemURL, "title": published.ItemTitle,
		"price": published.ItemPrice, "quantity": published.Quantity, "category": published.CategoryName}
	if args.CardID > 0 {
		delivery, bindErr := bindItemDelivery(client, bindArguments{AccountID: accountID, ItemID: published.ItemID, CardID: args.CardID, DeliveryCount: args.DeliveryCount})
		if bindErr != nil {
			result["bind_error"] = bindErr.Error() + "。商品已经发布成功，不要重新发布，请用 bind_item_delivery 重新绑定。"
		} else {
			result["delivery"] = delivery
		}
	}
	return result, nil
}

// checkPublishAccount 确认账号属于当前用户且已登录，并拒绝与该账号本地商品同标题的发布，防止重复上架。
func checkPublishAccount(client apiClient, accountID, title string, allowDuplicate bool) error {
	accounts, err := client.accounts()
	if err != nil {
		return err
	}
	var account *accountRow
	for i := range accounts {
		if accounts[i].ID == accountID {
			account = &accounts[i]
		}
	}
	if account == nil {
		return fmt.Errorf("账号 %s 不存在或不属于当前用户", accountID)
	}
	if !account.HasCookie {
		return fmt.Errorf("账号 %s 未登录，无法发布", accountID)
	}
	if allowDuplicate {
		return nil
	}
	items, err := client.items(accountID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if strings.TrimSpace(item.ItemTitle) == title {
			return fmt.Errorf("该账号已有同标题商品（item_id %s），可能已经发布过；确实要再发一个请设置 allow_duplicate_title=true", item.ItemID)
		}
	}
	return nil
}

// loadPublishImages 读取发布图片：http(s) 地址按网络策略下载，其余视为图片目录内的文件（相对路径或目录内的绝对路径）。
func loadPublishImages(client apiClient, references []string) ([]formFile, error) {
	images := make([]formFile, 0, len(references))
	for _, reference := range references {
		reference = strings.TrimSpace(reference)
		lower := strings.ToLower(reference)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			data, contentType, err := publishimage.Download(client.ctx, reference)
			if err != nil {
				return nil, err
			}
			name := filepath.Base(strings.SplitN(reference, "?", 2)[0])
			images = append(images, formFile{field: "images", filename: name, contentType: contentType, data: data})
			continue
		}
		if client.imageDir == "" {
			return nil, errors.New("服务未配置本地图片目录，只能使用图片 URL")
		}
		relative := reference
		if filepath.IsAbs(reference) {
			var err error
			if relative, err = filepath.Rel(client.imageDir, reference); err != nil {
				return nil, fmt.Errorf("图片必须放在 %s 目录内: %s", client.imageDir, reference)
			}
		}
		data, contentType, name, err := publishimage.ReadFile(client.imageDir, relative)
		if err != nil {
			return nil, fmt.Errorf("%v（本地图片需放在 %s 目录内）", err, client.imageDir)
		}
		images = append(images, formFile{field: "images", filename: name, contentType: contentType, data: data})
	}
	return images, nil
}
