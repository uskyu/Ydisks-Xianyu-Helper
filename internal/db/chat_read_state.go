package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// MarkRead 仅对当前用户拥有账号的非系统入站消息标记已读，并同步归零会话红点。
func (s *ChatStore) MarkRead(ctx context.Context, userID int64, cookieID, chatID string) error {
	// now 是同一批消息和会话状态使用的统一 UTC 时间，避免页面显示先后矛盾。
	now := time.Now().UTC()
	// err 保存批量更新非系统入站消息的错误；失败时不得清空会话红点以避免状态不一致。
	if _, err := s.DB.ExecContext(ctx, `UPDATE chat_messages SET read_status=2,read_at=?
		WHERE cookie_id=? AND chat_id=? AND direction='incoming' AND message_type<>'system' AND read_status<>2`,
		now.UnixMilli(), cookieID, chatID); err != nil {
		return err
	}
	// err 保存归零会话红点的错误，该更新通过用户归属子查询阻止越权修改。
	_, err := s.DB.ExecContext(ctx, `UPDATE chat_sessions SET unread_count=0,updated_at=?
		WHERE cookie_id=? AND chat_id=? AND EXISTS(SELECT 1 FROM cookies c WHERE c.id=chat_sessions.cookie_id AND c.user_id=?)`,
		now.Unix(), cookieID, chatID, userID)
	return err
}

// CountUnreadUserMessages 返回界面红点使用的入站真实用户未读数，系统消息永不计入。
func (s *ChatStore) CountUnreadUserMessages(ctx context.Context, cookieID, chatID string) (int, error) {
	// count 保存符合当前账号、会话及未读条件的消息总数。
	var count int
	// err 保存聚合查询错误，调用方可在平台响应缺失时退回官方红点值。
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_messages
		WHERE cookie_id=? AND chat_id=? AND direction='incoming' AND message_type<>'system' AND read_status<>2`, cookieID, chatID).Scan(&count)
	return count, err
}

// UpdateMessageStatus 更新消息状态。
func (s *ChatStore) UpdateMessageStatus(ctx context.Context, cookieID, key, status string) (*ChatMessage, error) {
	if // err 用于本次流程后续判断的err
	_, err := s.DB.ExecContext(ctx, `UPDATE chat_messages SET status=? WHERE cookie_id=? AND message_key=?`, status, cookieID, key); err != nil {
		return nil, err
	}
	return s.GetMessageByKey(ctx, cookieID, key)
}

// MarkMessageRead 按平台回执把目标出站消息及同会话中更早的出站消息标记为已读，并返回目标消息。
// 回执若对应本地入站消息则原样返回且不写入，由上层识别为无需处理的跨端状态同步。
func (s *ChatStore) MarkMessageRead(ctx context.Context, cookieID, key string, readAt int64) (*ChatMessage, error) {
	// message 保存平台回执对应的本地消息；只有出站方向的会话和发送时间可界定本次批量确认范围。
	message, err := s.GetMessageByKey(ctx, cookieID, key)
	if err != nil {
		return nil, err
	}
	if message.Direction != "outgoing" {
		return message, nil
	}
	// readAt 保存平台已读时间；缺失时使用本机 UTC 时间作为展示回退。
	if readAt <= 0 {
		readAt = time.Now().UTC().UnixMilli()
	}
	// err 保存按回执水位更新同会话出站历史的错误；对方读到目标消息时更早消息也已被阅读。
	if _, err = s.DB.ExecContext(ctx, `UPDATE chat_messages SET read_status=2,
		read_at=CASE WHEN read_status=2 AND read_at>0 THEN read_at ELSE ? END
		WHERE cookie_id=? AND chat_id=? AND direction='outgoing' AND sent_at<=?`, readAt, cookieID, message.ChatID, message.SentAt); err != nil {
		return nil, err
	}
	return s.GetMessageByKey(ctx, cookieID, key)
}

// MarkLatestOutgoingRead 在回执未带消息键时回退标记会话中最近待确认的出站消息。
func (s *ChatStore) MarkLatestOutgoingRead(ctx context.Context, cookieID, chatID string, readAt int64) (*ChatMessage, error) {
	// readAt 保存平台已读时间；缺失时使用本机 UTC 时间作为展示回退。
	if readAt <= 0 {
		readAt = time.Now().UTC().UnixMilli()
	}
	// key 保存最近一条已发送且未标记已读的消息幂等键。
	var key string
	// err 保存查询错误；没有可更新消息时返回统一 ErrNotFound。
	err := s.DB.QueryRowContext(ctx, `SELECT message_key FROM chat_messages WHERE cookie_id=? AND chat_id=? AND direction='outgoing' AND status='sent' AND read_status<>2 ORDER BY sent_at DESC,id DESC LIMIT 1`, cookieID, chatID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.MarkMessageRead(ctx, cookieID, key, readAt)
}

// UnreadAccountSummary 是账号维度未读汇总的单条展示数据；只包含非敏感名称和计数，不携带凭证或消息正文。
type UnreadAccountSummary struct {
	// AccountID 是账号主键，用于稳定排序和展示名称回退。
	AccountID string
	// AccountName 是用户备注、平台昵称或账号主键依次回退后的可展示名称。
	AccountName string
	// UnreadCount 是该账号所有可见会话当前累计的入站用户未读消息数。
	UnreadCount int
}

// UnreadAccountSummaries 返回与 accountID 同属一个用户的所有账号未读汇总。
// 查询只读取 cookies 的 id、remark、nickname 和 chat_sessions.unread_count，不读取、不解密 Cookie、Token 或消息内容。
// 结果只包含未读数大于零的账号，并按未读数降序、账号主键升序排列，便于通知稳定展示。
func (s *ChatStore) UnreadAccountSummaries(ctx context.Context, accountID string) ([]UnreadAccountSummary, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("聊天存储未初始化")
	}
	// accountKey 是去除首尾空白后的账号标识；空值无法定位归属用户，直接返回空汇总。
	accountKey := strings.TrimSpace(accountID)
	if accountKey == "" {
		return nil, nil
	}
	// rows、err 保存归属用户全部账号的未读聚合查询结果及数据库错误。
	rows, err := s.DB.QueryContext(ctx, `SELECT c.id, COALESCE(c.remark,''), COALESCE(c.nickname,''), COALESCE(SUM(s.unread_count),0)
		FROM cookies c JOIN chat_sessions s ON s.cookie_id=c.id
		WHERE c.user_id=(SELECT user_id FROM cookies WHERE id=?)
		GROUP BY c.id,c.remark,c.nickname
		HAVING SUM(s.unread_count)>0
		ORDER BY COALESCE(SUM(s.unread_count),0) DESC, c.id ASC`, accountKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// summaries 保存按数据库排序返回的非敏感未读汇总，容量按常见少量账号预估。
	summaries := make([]UnreadAccountSummary, 0, 4)
	for rows.Next() {
		// id、remark、nickname 分别是账号主键、用户备注和平台昵称缓存。
		var id, remark, nickname string
		// count 是该账号所有会话累计的未读消息数。
		var count int
		// scanErr 保存当前汇总行扫描失败的原因。
		if scanErr := rows.Scan(&id, &remark, &nickname, &count); scanErr != nil {
			return nil, scanErr
		}
		// displayName 按备注、昵称、账号 ID 的顺序回退，语义与通知器单账号展示保持一致。
		displayName := strings.TrimSpace(remark)
		if displayName == "" {
			displayName = strings.TrimSpace(nickname)
		}
		if displayName == "" {
			displayName = id
		}
		summaries = append(summaries, UnreadAccountSummary{AccountID: id, AccountName: displayName, UnreadCount: count})
	}
	// iterationErr 保存游标遍历结束时的数据库错误，避免静默返回不完整汇总。
	if iterationErr := rows.Err(); iterationErr != nil {
		return nil, iterationErr
	}
	return summaries, nil
}
