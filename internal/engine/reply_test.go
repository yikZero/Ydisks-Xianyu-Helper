package engine

import (
	"context"
	"path/filepath"
	"testing"

	"xianyu-go/internal/db"
)

func newReplyStore(t *testing.T) (*db.Store, func()) {
	t.Helper()
	d, _, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := db.NewStore(d, db.DialectSQLite)
	s.Users.Create(context.Background(), "admin", "a@e.com", "pw")
	admin, _ := s.Users.GetByUsername(context.Background(), "admin")
	s.Cookies.Save(context.Background(), "cid", "unb=123; _m_h5_tk=tk_1;", admin.ID)
	return s, func() { d.Close() }
}

func chatMsg(text, itemID, chatID string) ChatMessage {
	return ChatMessage{
		AccountID: "cid", CookieStr: "",
		ChatID: chatID, SenderUserID: "buyer1", SenderName: "买家",
		Text: text, ItemID: itemID,
	}
}

// TestKeywordReply_MatchAndVariableSubst 关键词匹配 + 变量替换。
func TestKeywordReply_MatchAndVariableSubst(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	// 关键词 "在吗" → "你好{send_user_name}，在的，请问要{send_message}吗"
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,type) VALUES ('cid','在吗','你好{send_user_name}，在的','text')`)

	r := NewReplyService("cid", s, nil, nil, nil, nil)
	res := r.resolve(ctx, chatMsg("老板在吗", "item1", "chat1"))
	if res == nil || res.Source != "关键词" {
		t.Fatalf("应命中关键词，got %+v", res)
	}
	if res.Text != "你好买家，在的" {
		t.Errorf("变量替换: got %q want 你好买家，在的", res.Text)
	}
}

// TestKeywordReply_AnyKeywordTermMatches 验证一条规则中的任一候选词均可触发同一回复。
func TestKeywordReply_AnyKeywordTermMatches(t *testing.T) {
	// s、cleanup 分别提供隔离的 SQLite 仓储及测试结束后的资源释放。
	s, cleanup := newReplyStore(t)
	defer cleanup()
	// ctx 是关键词持久化与回复解析共享的可取消请求上下文。
	ctx := context.Background()
	// insertErr 保存写入包含全部支持分隔符的 OR 关键词规则时产生的数据库错误。
	_, insertErr := s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,type) VALUES ('cid','在吗 | 在不在,有人吗，价格多少、怎么卖
什么价','统一回复','text')`)
	if insertErr != nil {
		t.Fatalf("insert keyword rule: %v", insertErr)
	}
	// r 是使用该账号关键词规则进行消息解析的回复服务。
	r := NewReplyService("cid", s, nil, nil, nil, nil)
	// messages 收集每种分隔符右侧的候选词，确保它们均以 OR 语义命中。
	messages := []string{"老板在吗", "在不在呀", "有人吗", "价格多少", "请问怎么卖", "什么价"}
	// message 是当前用于验证候选词匹配的买家消息。
	for _, message := range messages {
		// result 保存当前消息解析得到的关键词回复。
		result := r.resolve(ctx, chatMsg(message, "item1", "chat1"))
		if result == nil || result.Text != "统一回复" {
			t.Errorf("消息 %q 未命中任一关键词规则: %+v", message, result)
		}
	}
}

// TestKeywordMatchesIgnoresEmptyTerms 验证连续分隔符和空白不会意外匹配所有买家消息。
func TestKeywordMatchesIgnoresEmptyTerms(t *testing.T) {
	// matched 记录空片段被忽略后，非空候选词是否仍可按包含语义匹配。
	matched := keywordMatches("请问价格多少", " | , ， 、 \n 价格  ")
	if !matched {
		t.Fatal("包含有效候选词时应命中")
	}
	// emptyMatched 记录仅含分隔符的规则是否会错误地作为空字符串匹配。
	emptyMatched := keywordMatches("任意消息", " | , ， 、 \n ")
	if emptyMatched {
		t.Fatal("仅含分隔符的规则不得匹配任意消息")
	}
}

// TestKeywordReply_ItemIDPriority 商品ID关键词优先于通用关键词。
func TestKeywordReply_ItemIDPriority(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,item_id,type) VALUES
		('cid','价格','通用价格回复','','text'),
		('cid','价格','该商品专属价格回复','item1','text')`)

	r := NewReplyService("cid", s, nil, nil, nil, nil)
	res := r.resolve(ctx, chatMsg("价格多少", "item1", "chat1"))
	if res == nil || res.Text != "该商品专属价格回复" {
		t.Errorf("应优先商品ID关键词，got %+v", res)
	}
	// 无 item_id 时走通用。
	res2 := r.resolve(ctx, chatMsg("价格多少", "", "chat1"))
	if res2 == nil || res2.Text != "通用价格回复" {
		t.Errorf("无itemID应走通用，got %+v", res2)
	}
}

// TestKeywordReply_MultipleItemIDsInOneRule 一条规则关联多个商品时任一命中即生效。
func TestKeywordReply_MultipleItemIDsInOneRule(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,item_id,type) VALUES
		('cid','价格','多商品共用回复','item1,item2','text'),
		('cid','价格','通用价格回复','','text')`)

	r := NewReplyService("cid", s, nil, nil, nil, nil)
	// 命中共用规则中的第一个商品。
	first := r.resolve(ctx, chatMsg("价格多少", "item1", "chat1"))
	if first == nil || first.Text != "多商品共用回复" {
		t.Errorf("item1 应命中多商品规则，got %+v", first)
	}
	// 命中共用规则中的第二个商品。
	second := r.resolve(ctx, chatMsg("价格多少", "item2", "chat2"))
	if second == nil || second.Text != "多商品共用回复" {
		t.Errorf("item2 应命中多商品规则，got %+v", second)
	}
	// 未关联商品回退到账号级规则。
	fallback := r.resolve(ctx, chatMsg("价格多少", "item3", "chat3"))
	if fallback == nil || fallback.Text != "通用价格回复" {
		t.Errorf("未关联商品应回退账号级，got %+v", fallback)
	}
}

// TestKeywordReply_EmptyReplySkip 匹配到空回复 → Skip。
func TestKeywordReply_EmptyReplySkip(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,type) VALUES ('cid','静默','','text')`)
	r := NewReplyService("cid", s, nil, nil, nil, nil)
	res := r.resolve(ctx, chatMsg("静默一下", "item1", "chat1"))
	if res == nil || !res.Skip {
		t.Fatalf("空回复应 Skip=true，got %+v", res)
	}
}

// TestDefaultReply_ItemReplyFirst 指定商品回复优先于账号默认回复。
func TestDefaultReply_ItemReplyFirst(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO default_replies (cookie_id,enabled,reply_content,reply_once) VALUES ('cid',1,'账号默认回复',0)`)
	s.DB.ExecContext(ctx, `INSERT INTO item_replay (item_id,cookie_id,reply_content) VALUES ('item1','cid','该商品专属默认回复')`)

	r := NewReplyService("cid", s, nil, nil, nil, nil)
	res := r.resolve(ctx, chatMsg("任意消息", "item1", "chat1"))
	if res == nil || res.Text != "该商品专属默认回复" {
		t.Errorf("应优先指定商品回复，got %+v", res)
	}
}

// TestDefaultReply_ReplyOnce reply_once 防重复。
func TestDefaultReply_ReplyOnce(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO default_replies (cookie_id,enabled,reply_content,reply_once) VALUES ('cid',1,'默认问候',1)`)

	sender := &recordingSender{}
	r := NewReplyService("cid", s, sender, nil, nil, nil)
	if err := r.Handle(ctx, chatMsg("在吗", "item1", "chatX")); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(ctx, chatMsg("还在吗", "item1", "chatX")); err != nil {
		t.Fatal(err)
	}
	if err := r.Handle(ctx, chatMsg("在吗", "item1", "chatY")); err != nil {
		t.Fatal(err)
	}
	if len(sender.texts) != 2 || sender.texts[0].text != "默认问候" || sender.texts[1].chatID != "chatY" {
		t.Fatalf("reply_once 发送记录异常: %+v", sender.texts)
	}
}

// TestPriority_Order 四级优先级：关键词 > 默认（无API/AI时）。
func TestPriority_Order(t *testing.T) {
	s, cleanup := newReplyStore(t)
	defer cleanup()
	ctx := context.Background()
	s.DB.ExecContext(ctx, `INSERT INTO keywords (cookie_id,keyword,reply,type) VALUES ('cid','在吗','关键词回复','text')`)
	s.DB.ExecContext(ctx, `INSERT INTO default_replies (cookie_id,enabled,reply_content,reply_once) VALUES ('cid',1,'默认回复',0)`)

	r := NewReplyService("cid", s, nil, nil, nil, nil)
	// 命中关键词 → 关键词回复。
	res := r.resolve(ctx, chatMsg("在吗", "", "chat1"))
	if res == nil || res.Source != "关键词" {
		t.Errorf("应优先关键词，got %+v", res)
	}
	// 不命中关键词 → 默认回复。
	res2 := r.resolve(ctx, chatMsg("别的消息", "", "chat1"))
	if res2 == nil || res2.Source != "默认" {
		t.Errorf("无关键词应走默认，got %+v", res2)
	}
}

// TestFormatReply 变量替换。
func TestFormatReply(t *testing.T) {
	m := chatMsg("想买", "item1", "chat1")
	m.SenderName = "张三"
	got := formatReply("你好{send_user_name}，你说{send_message}", m)
	if got != "你好张三，你说想买" {
		t.Errorf("formatReply=%q", got)
	}
}

// TestParsePrice 价格解析。
func TestParsePrice(t *testing.T) {
	cases := map[string]float64{
		"￥99.5": 99.5,
		"100元":  100,
		"":      0,
		"abc":   0,
		"12.34": 12.34,
	}
	for in, want := range cases {
		if got := parsePrice(in); got != want {
			t.Errorf("parsePrice(%q)=%v want %v", in, got, want)
		}
	}
}
