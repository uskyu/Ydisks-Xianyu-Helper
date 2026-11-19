package db

import (
	"context"
	"testing"
)

// TestChatStoreUnreadAccountSummaries 验证同一用户多账号未读汇总的聚合、排序、名称回退与已读刷新。
func TestChatStoreUnreadAccountSummaries(t *testing.T) {
	// store、cleanup 分别保存隔离的 SQLite 存储和关闭函数。
	store, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 是本测试数据库调用共用的上下文。
	ctx := context.Background()
	// ok、createErr 保存创建汇总测试用户的结果及错误。
	if ok, createErr := store.Users.Create(ctx, "summary-owner", "summary-owner@example.com", "pw"); createErr != nil || !ok {
		t.Fatalf("创建汇总测试用户: ok=%v err=%v", ok, createErr)
	}
	// owner、ownerErr 保存汇总测试用户及其查询错误。
	owner, ownerErr := store.Users.GetByUsername(ctx, "summary-owner")
	if ownerErr != nil || owner == nil {
		t.Fatalf("读取汇总测试用户: owner=%+v err=%v", owner, ownerErr)
	}
	// otherOK、otherCreateErr 保存创建另一用户的结果及错误，用于验证跨用户账号不进入汇总。
	if otherOK, otherCreateErr := store.Users.Create(ctx, "summary-other", "summary-other@example.com", "pw"); otherCreateErr != nil || !otherOK {
		t.Fatalf("创建另一用户: ok=%v err=%v", otherOK, otherCreateErr)
	}
	// other、otherErr 保存另一用户及其查询错误。
	other, otherErr := store.Users.GetByUsername(ctx, "summary-other")
	if otherErr != nil || other == nil {
		t.Fatalf("读取另一用户: other=%+v err=%v", other, otherErr)
	}

	// createAccount 创建一个归属用户并写入备注/昵称的测试账号。
	createAccount := func(id, remark, nickname string, ownerID int64) {
		t.Helper()
		// createErr 保存加密创建测试账号的错误。
		if createErr := store.Cookies.CreateOwned(ctx, id, "test-cookie-"+id, ownerID); createErr != nil {
			t.Fatalf("创建测试账号 %s: %v", id, createErr)
		}
		// updateErr 保存写入非敏感展示名称的错误。
		if _, updateErr := store.DB.ExecContext(ctx, `UPDATE cookies SET remark=?, nickname=? WHERE id=?`, remark, nickname, id); updateErr != nil {
			t.Fatalf("写入账号 %s 展示名称: %v", id, updateErr)
		}
	}

	// accA、accB、accC 是同一用户下未读分布不同的三个账号，accD 属于另一用户。
	createAccount("accA", "店铺A", "", owner.ID)
	createAccount("accB", "", "商城B", owner.ID)
	createAccount("accC", "", "", owner.ID)
	createAccount("accD", "其他用户店铺", "", other.ID)

	// upsertSession 创建带未读数的会话，验证未读汇总来自 chat_sessions.unread_count。
	upsertSession := func(cookieID, chatID string, unread int) {
		t.Helper()
		// sessionErr 保存创建测试会话的错误。
		if sessionErr := store.Chats.UpsertSession(ctx, ChatSession{CookieID: cookieID, ChatID: chatID, BuyerID: "buyer", UnreadCount: unread}); sessionErr != nil {
			t.Fatalf("创建会话 %s/%s: %v", cookieID, chatID, sessionErr)
		}
	}

	upsertSession("accA", "chat-a1", 2)
	upsertSession("accA", "chat-a2", 3)
	upsertSession("accB", "chat-b1", 1)
	upsertSession("accC", "chat-c1", 2)
	upsertSession("accD", "chat-d1", 10)

	// summaries、summaryErr 保存同一用户账号的未读汇总及查询错误。
	summaries, summaryErr := store.Chats.UnreadAccountSummaries(ctx, "accA")
	if summaryErr != nil {
		t.Fatalf("读取未读汇总: %v", summaryErr)
	}
	if len(summaries) != 3 {
		t.Fatalf("未读汇总数量=%d want=3 summaries=%+v", len(summaries), summaries)
	}
	// want 保存按未读数降序、账号主键升序排列的期望结果；accB 使用昵称回退，accC 使用账号 ID 回退。
	want := []UnreadAccountSummary{
		{AccountID: "accA", AccountName: "店铺A", UnreadCount: 5},
		{AccountID: "accC", AccountName: "accC", UnreadCount: 2},
		{AccountID: "accB", AccountName: "商城B", UnreadCount: 1},
	}
	// index、expected 表示当前期望汇总的下标和值。
	for index, expected := range want {
		if summaries[index] != expected {
			t.Fatalf("汇总[%d]=%+v want=%+v", index, summaries[index], expected)
		}
	}

	// readErr 保存把 accB 会话标记已读的错误，用于验证汇总会随未读数归零而移除账号。
	if readErr := store.Chats.MarkRead(ctx, owner.ID, "accB", "chat-b1"); readErr != nil {
		t.Fatalf("标记 accB 已读: %v", readErr)
	}
	// refreshed、refreshedErr 保存已读后的未读汇总及查询错误。
	refreshed, refreshedErr := store.Chats.UnreadAccountSummaries(ctx, "accA")
	if refreshedErr != nil {
		t.Fatalf("读取已读后汇总: %v", refreshedErr)
	}
	if len(refreshed) != 2 || refreshed[0].AccountID != "accA" || refreshed[1].AccountID != "accC" {
		t.Fatalf("已读后应移除零未读账号: %+v", refreshed)
	}

	// unknown、unknownErr 验证不存在的账号无法解析归属用户时返回空汇总。
	unknown, unknownErr := store.Chats.UnreadAccountSummaries(ctx, "missing-account")
	if unknownErr != nil || len(unknown) != 0 {
		t.Fatalf("未知账号汇总=%+v err=%v", unknown, unknownErr)
	}
	// blank、blankErr 验证空白账号标识直接返回空汇总且不查询数据库。
	blank, blankErr := store.Chats.UnreadAccountSummaries(ctx, "   ")
	if blankErr != nil || len(blank) != 0 {
		t.Fatalf("空白账号汇总=%+v err=%v", blank, blankErr)
	}
	// emptyStore 和 initErr 验证未初始化仓储返回明确错误。
	var emptyStore *ChatStore
	// initErr 保存未初始化仓储查询未读汇总返回的错误；必须非空。
	_, initErr := emptyStore.UnreadAccountSummaries(ctx, "accA")
	if initErr == nil {
		t.Fatal("未初始化聊天仓储应返回错误")
	}
}
