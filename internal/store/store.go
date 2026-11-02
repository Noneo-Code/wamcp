// Package store is the local SQLite archive of WhatsApp chats and messages.
//
// WhatsApp only delivers messages once; wamcp keeps them here so they can be
// listed and searched later. The archive is independent from the whatsmeow
// session database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// ErrNotFound is returned when a chat or message does not exist.
var ErrNotFound = errors.New("not found")

// Chat is a conversation (direct or group).
type Chat struct {
	JID           string
	Name          string
	IsGroup       bool
	LastMessageAt time.Time
	LastMessage   *Message // nil when the chat has no stored message
}

// Message is a single stored message.
type Message struct {
	ChatJID   string
	ID        string
	SenderJID string
	FromMe    bool
	Timestamp time.Time
	Text      string
	MediaType string // "", "image", "video", "audio", "document", "sticker"
	Filename  string
	MimeType  string
	Raw       []byte // serialized waE2E.Message, kept only for media (needed to download it)
	Edited    bool
	Deleted   bool
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS chats (
	jid             TEXT PRIMARY KEY,
	name            TEXT NOT NULL DEFAULT '',
	is_group        INTEGER NOT NULL DEFAULT 0,
	last_message_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS messages (
	chat_jid   TEXT NOT NULL REFERENCES chats(jid) ON DELETE CASCADE,
	id         TEXT NOT NULL,
	sender_jid TEXT NOT NULL DEFAULT '',
	from_me    INTEGER NOT NULL DEFAULT 0,
	ts         INTEGER NOT NULL,
	text       TEXT NOT NULL DEFAULT '',
	media_type TEXT NOT NULL DEFAULT '',
	filename   TEXT NOT NULL DEFAULT '',
	mimetype   TEXT NOT NULL DEFAULT '',
	raw        BLOB,
	edited     INTEGER NOT NULL DEFAULT 0,
	deleted    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat_jid, id)
);
CREATE INDEX IF NOT EXISTS messages_chat_ts ON messages(chat_jid, ts);
CREATE INDEX IF NOT EXISTS messages_ts ON messages(ts);
`

// Open opens (and creates if needed) the archive at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open message store: %w", err)
	}
	// SQLite allows a single writer; serialising through one connection avoids
	// SQLITE_BUSY between the event handler and tool calls.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialise message store: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertChat creates the chat or updates it. An empty name never overwrites a
// known one, and last_message_at only moves forward.
func (s *Store) UpsertChat(ctx context.Context, jid, name string, isGroup bool, lastMessageAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO chats (jid, name, is_group, last_message_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
			is_group = excluded.is_group OR chats.is_group,
			last_message_at = MAX(chats.last_message_at, excluded.last_message_at)`,
		jid, name, isGroup, unix(lastMessageAt))
	if err != nil {
		return fmt.Errorf("upsert chat %s: %w", jid, err)
	}
	return nil
}

// RenameChat sets the name of an existing chat; unknown chats are left alone
// (so seeing someone's name in a group does not create a chat with them).
func (s *Store) RenameChat(ctx context.Context, jid, name string) error {
	if name == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE chats SET name = ? WHERE jid = ? AND is_group = 0`, name, jid)
	if err != nil {
		return fmt.Errorf("rename chat %s: %w", jid, err)
	}
	return nil
}

// SaveMessage stores m and bumps its chat. Re-saving an existing message (e.g.
// from a later history sync) refreshes its content.
func (s *Store) SaveMessage(ctx context.Context, m Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chats (jid, last_message_at) VALUES (?, ?)
		ON CONFLICT(jid) DO UPDATE SET last_message_at = MAX(chats.last_message_at, excluded.last_message_at)`,
		m.ChatJID, unix(m.Timestamp)); err != nil {
		return fmt.Errorf("bump chat %s: %w", m.ChatJID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (chat_jid, id, sender_jid, from_me, ts, text, media_type, filename, mimetype, raw, edited, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, id) DO UPDATE SET
			sender_jid = excluded.sender_jid,
			from_me = excluded.from_me,
			ts = excluded.ts,
			text = CASE WHEN messages.edited = 1 OR messages.deleted = 1 THEN messages.text ELSE excluded.text END,
			media_type = excluded.media_type,
			filename = excluded.filename,
			mimetype = excluded.mimetype,
			raw = CASE WHEN messages.deleted = 1 THEN NULL ELSE COALESCE(excluded.raw, messages.raw) END,
			edited = MAX(messages.edited, excluded.edited),
			deleted = MAX(messages.deleted, excluded.deleted)
		-- Message IDs are chosen by the sending client: only the original sender
		-- may refresh a message, so nobody can overwrite someone else's by reusing its ID.
		WHERE messages.sender_jid = excluded.sender_jid AND messages.from_me = excluded.from_me`,
		m.ChatJID, m.ID, m.SenderJID, m.FromMe, unix(m.Timestamp), m.Text,
		m.MediaType, m.Filename, m.MimeType, nilIfEmpty(m.Raw), m.Edited, m.Deleted); err != nil {
		return fmt.Errorf("save message %s: %w", m.ID, err)
	}
	return tx.Commit()
}

// EditMessage replaces the text of a stored message, only if editorJID is its
// sender (WhatsApp only lets authors edit). Unknown messages are ignored.
func (s *Store) EditMessage(ctx context.Context, chatJID, id, editorJID, text string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET text = ?, edited = 1 WHERE chat_jid = ? AND id = ? AND sender_jid = ?`,
		text, chatJID, id, editorJID)
	if err != nil {
		return fmt.Errorf("edit message %s: %w", id, err)
	}
	return nil
}

// DeleteMessage marks a message as deleted for everyone and drops its content.
// Unless anySender is set (a verified group admin), only the message's own
// sender can delete it. It reports whether a message was deleted.
func (s *Store) DeleteMessage(ctx context.Context, chatJID, id, revokerJID string, anySender bool) (bool, error) {
	q := `UPDATE messages SET text = '', raw = NULL, deleted = 1 WHERE chat_jid = ? AND id = ?`
	args := []any{chatJID, id}
	if !anySender {
		q += ` AND sender_jid = ?`
		args = append(args, revokerJID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, fmt.Errorf("delete message %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// MessageSender returns the sender of a stored message.
func (s *Store) MessageSender(ctx context.Context, chatJID, id string) (string, error) {
	var sender string
	err := s.db.QueryRowContext(ctx, `SELECT sender_jid FROM messages WHERE chat_jid = ? AND id = ?`, chatJID, id).Scan(&sender)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("message sender %s: %w", id, err)
	}
	return sender, nil
}

// ListChatsParams filters ListChats.
type ListChatsParams struct {
	Query  string // case-insensitive match on name or JID; empty matches all
	Limit  int
	Offset int
}

// ListChats returns chats ordered by most recent activity, each with its last message.
func (s *Store) ListChats(ctx context.Context, p ListChatsParams) ([]Chat, error) {
	q := `
		SELECT c.jid, c.name, c.is_group, c.last_message_at,
		       m.id, m.sender_jid, m.from_me, m.ts, m.text, m.media_type, m.filename, m.deleted
		FROM chats c
		LEFT JOIN messages m ON m.rowid = (
			SELECT rowid FROM messages WHERE chat_jid = c.jid ORDER BY ts DESC LIMIT 1)`
	var args []any
	if p.Query != "" {
		q += ` WHERE c.name LIKE ? ESCAPE '\' OR c.jid LIKE ? ESCAPE '\'`
		pattern := likePattern(p.Query)
		args = append(args, pattern, pattern)
	}
	q += ` ORDER BY c.last_message_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit(p.Limit, 20, 200), max(p.Offset, 0))

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var (
			c                           Chat
			lastAt                      int64
			mID, mSender, mText, mMedia sql.NullString
			mFile                       sql.NullString
			mFromMe, mDeleted           sql.NullBool
			mTS                         sql.NullInt64
		)
		if err := rows.Scan(&c.JID, &c.Name, &c.IsGroup, &lastAt,
			&mID, &mSender, &mFromMe, &mTS, &mText, &mMedia, &mFile, &mDeleted); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		c.LastMessageAt = fromUnix(lastAt)
		if mID.Valid {
			c.LastMessage = &Message{
				ChatJID: c.JID, ID: mID.String, SenderJID: mSender.String, FromMe: mFromMe.Bool,
				Timestamp: fromUnix(mTS.Int64), Text: mText.String, MediaType: mMedia.String,
				Filename: mFile.String, Deleted: mDeleted.Bool,
			}
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// GetChat returns a single chat without its last message.
func (s *Store) GetChat(ctx context.Context, jid string) (Chat, error) {
	var (
		c      Chat
		lastAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT jid, name, is_group, last_message_at FROM chats WHERE jid = ?`, jid).
		Scan(&c.JID, &c.Name, &c.IsGroup, &lastAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Chat{}, ErrNotFound
	}
	if err != nil {
		return Chat{}, fmt.Errorf("get chat %s: %w", jid, err)
	}
	c.LastMessageAt = fromUnix(lastAt)
	return c, nil
}

// AllChats returns every chat JID with its group flag (used to refresh names).
func (s *Store) AllChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT jid, name, is_group FROM chats`)
	if err != nil {
		return nil, fmt.Errorf("list all chats: %w", err)
	}
	defer rows.Close()
	var chats []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.JID, &c.Name, &c.IsGroup); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		chats = append(chats, c)
	}
	return chats, rows.Err()
}

// MessagesParams selects messages of one chat.
type MessagesParams struct {
	ChatJID  string
	Before   time.Time // only messages strictly older than this time; zero means "latest"
	BeforeID string    // only messages older than this message (pagination cursor, exact even within one second)
	Limit    int
}

// Messages returns the newest messages of a chat (older than Before), in chronological order.
func (s *Store) Messages(ctx context.Context, p MessagesParams) ([]Message, error) {
	// Cursor = (ts, rowid): timestamps have one-second resolution, so several
	// messages can share one and a time-only cursor would skip some of them.
	curTS, curRow := int64(1<<62), int64(1<<62)
	if p.BeforeID != "" {
		err := s.db.QueryRowContext(ctx, `SELECT ts, rowid FROM messages WHERE chat_jid = ? AND id = ?`,
			p.ChatJID, p.BeforeID).Scan(&curTS, &curRow)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("message %s in chat %s: %w", p.BeforeID, p.ChatJID, ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("locate cursor message: %w", err)
		}
	}
	if !p.Before.IsZero() && p.Before.Unix() <= curTS {
		curTS, curRow = p.Before.Unix(), -1 // strictly before that second
	}
	msgs, err := s.queryMessages(ctx, `WHERE chat_jid = ? AND (ts < ? OR (ts = ? AND rowid < ?)) ORDER BY ts DESC, rowid DESC LIMIT ?`,
		p.ChatJID, curTS, curTS, curRow, limit(p.Limit, 30, 500))
	if err != nil {
		return nil, err
	}
	reverse(msgs)
	return msgs, nil
}

// SearchParams filters Search.
type SearchParams struct {
	Query   string // substring of the text or filename (case-insensitive for ASCII letters)
	ChatJID string // optional
	Limit   int
}

// Search returns matching messages, newest first.
func (s *Store) Search(ctx context.Context, p SearchParams) ([]Message, error) {
	if strings.TrimSpace(p.Query) == "" {
		return nil, errors.New("search query is empty")
	}
	where := `WHERE deleted = 0 AND (text LIKE ? ESCAPE '\' OR filename LIKE ? ESCAPE '\')`
	pattern := likePattern(p.Query)
	args := []any{pattern, pattern}
	if p.ChatJID != "" {
		where += ` AND chat_jid = ?`
		args = append(args, p.ChatJID)
	}
	args = append(args, limit(p.Limit, 30, 200))
	return s.queryMessages(ctx, where+` ORDER BY ts DESC LIMIT ?`, args...)
}

// GetMessage returns one message, including its raw payload.
func (s *Store) GetMessage(ctx context.Context, chatJID, id string) (Message, error) {
	msgs, err := s.queryMessages(ctx, `WHERE chat_jid = ? AND id = ?`, chatJID, id)
	if err != nil {
		return Message{}, err
	}
	if len(msgs) == 0 {
		return Message{}, ErrNotFound
	}
	return msgs[0], nil
}

// Stats returns the number of stored chats and messages.
func (s *Store) Stats(ctx context.Context) (chats, messages int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM chats), (SELECT COUNT(*) FROM messages)`).Scan(&chats, &messages)
	if err != nil {
		return 0, 0, fmt.Errorf("stats: %w", err)
	}
	return chats, messages, nil
}

func (s *Store) queryMessages(ctx context.Context, tail string, args ...any) ([]Message, error) {
	const selectMessages = `SELECT chat_jid, id, sender_jid, from_me, ts, text, media_type, filename, mimetype, raw, edited, deleted FROM messages `
	// tail is always one of this file's constant clauses; values go through args.
	rows, err := s.db.QueryContext(ctx, selectMessages+tail, args...) //nolint:gosec // see above
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	var msgs []Message
	for rows.Next() {
		var (
			m  Message
			ts int64
		)
		if err := rows.Scan(&m.ChatJID, &m.ID, &m.SenderJID, &m.FromMe, &ts, &m.Text,
			&m.MediaType, &m.Filename, &m.MimeType, &m.Raw, &m.Edited, &m.Deleted); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.Timestamp = fromUnix(ts)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func limit(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
