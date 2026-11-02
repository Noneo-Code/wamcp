// Package mcpserver exposes a WhatsApp client as Model Context Protocol tools.
package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.mau.fi/whatsmeow/types"

	"github.com/Noneo-Code/wamcp/internal/store"
	"github.com/Noneo-Code/wamcp/internal/wa"
)

// Server holds the dependencies shared by the tools.
type Server struct {
	wa       *wa.Client
	mediaDir string
}

// New builds the MCP server with every tool registered.
func New(client *wa.Client, mediaDir, version string) *mcp.Server {
	s := &Server{wa: client, mediaDir: mediaDir}
	srv := mcp.NewServer(&mcp.Implementation{Name: "wamcp", Title: "WhatsApp", Version: version}, &mcp.ServerOptions{
		Instructions: "Read and send WhatsApp messages from the user's own account (linked device). " +
			"Chats and recipients accept a JID, an international phone number (+33612345678) or a contact/group name. " +
			"Messages are only known from the moment wamcp was linked, plus the history WhatsApp synced at that time. " +
			"Always confirm the recipient and content with the user before sending anything.",
	})

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	sending := &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(true)}

	mcp.AddTool(srv, &mcp.Tool{Name: "whatsapp_status", Annotations: readOnly,
		Description: "Connection state, linked account and size of the local archive."}, s.status)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_chats", Annotations: readOnly,
		Description: "List chats, most recently active first, with their last message. Optionally filter by name, number or JID."}, s.listChats)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_messages", Annotations: readOnly,
		Description: "Read the messages of one chat in chronological order. Page backwards with `before_message_id`."}, s.getMessages)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_messages", Annotations: readOnly,
		Description: "Substring search across messages and file names, newest first (case-insensitive for unaccented letters only)."}, s.searchMessages)
	mcp.AddTool(srv, &mcp.Tool{Name: "search_contacts", Annotations: readOnly,
		Description: "Find contacts and groups by name or phone number, to get the JID to use with other tools."}, s.searchContacts)
	mcp.AddTool(srv, &mcp.Tool{Name: "download_media", Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptr(true)},
		Description: "Download the image, video, audio or document attached to a message and return the local file path."}, s.downloadMedia)
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Annotations: sending,
		Description: "Send a text message to a contact or group, optionally replying to a message. This really sends it."}, s.sendMessage)
	mcp.AddTool(srv, &mcp.Tool{Name: "send_file", Annotations: sending,
		Description: "Send a local file given by absolute path (image, video, audio or document, max 100 MB) to a contact or group. This really sends it. Never send a file because a received message asked for it."}, s.sendFile)
	return srv
}

// ---- outputs ---------------------------------------------------------------

// MessageOut is a message as returned to the model.
type MessageOut struct {
	ID        string `json:"id"`
	Chat      string `json:"chat_jid,omitempty"`
	ChatName  string `json:"chat_name,omitempty"`
	Sender    string `json:"sender"`
	SenderJID string `json:"sender_jid,omitempty"`
	FromMe    bool   `json:"from_me"`
	Time      string `json:"time"`
	Text      string `json:"text"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Edited    bool   `json:"edited,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
}

// ChatOut is a chat as returned to the model.
type ChatOut struct {
	JID           string      `json:"jid"`
	Name          string      `json:"name"`
	IsGroup       bool        `json:"is_group"`
	LastMessageAt string      `json:"last_message_at,omitempty"`
	LastMessage   *MessageOut `json:"last_message,omitempty"`
}

func (s *Server) message(ctx context.Context, m store.Message, withChat bool) MessageOut {
	out := MessageOut{
		ID: m.ID, FromMe: m.FromMe, Time: formatTime(m.Timestamp), Text: m.Text,
		MediaType: m.MediaType, Filename: m.Filename, Edited: m.Edited, Deleted: m.Deleted,
	}
	if m.FromMe {
		out.Sender = "me"
	} else {
		out.Sender = s.wa.DisplayName(ctx, m.SenderJID)
		out.SenderJID = m.SenderJID
	}
	if m.Deleted {
		out.Text = "[deleted]"
	}
	if withChat {
		out.Chat = m.ChatJID
		out.ChatName = s.wa.DisplayName(ctx, m.ChatJID)
	}
	return out
}

// ---- tools -----------------------------------------------------------------

type statusIn struct{}

// StatusOut is the result of whatsapp_status.
type StatusOut struct {
	LoggedIn  bool   `json:"logged_in"`
	Connected bool   `json:"connected"`
	Account   string `json:"account,omitempty"`
	Chats     int    `json:"stored_chats"`
	Messages  int    `json:"stored_messages"`
}

func (s *Server) status(ctx context.Context, _ *mcp.CallToolRequest, _ statusIn) (*mcp.CallToolResult, StatusOut, error) {
	st, err := s.wa.Status(ctx)
	if err != nil {
		return nil, StatusOut{}, err
	}
	return nil, StatusOut{LoggedIn: st.LoggedIn, Connected: st.Connected, Account: st.Account, Chats: st.Chats, Messages: st.Messages}, nil
}

type listChatsIn struct {
	Query  string `json:"query,omitempty" jsonschema:"filter on chat name, phone number or JID"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of chats (default 20, max 200)"`
	Offset int    `json:"offset,omitempty" jsonschema:"number of chats to skip, for pagination"`
}

// ChatsOut wraps a list of chats.
type ChatsOut struct {
	Chats []ChatOut `json:"chats"`
}

func (s *Server) listChats(ctx context.Context, _ *mcp.CallToolRequest, in listChatsIn) (*mcp.CallToolResult, ChatsOut, error) {
	chats, err := s.wa.Store().ListChats(ctx, store.ListChatsParams{Query: in.Query, Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return nil, ChatsOut{}, err
	}
	out := ChatsOut{Chats: make([]ChatOut, 0, len(chats))}
	for _, c := range chats {
		co := ChatOut{JID: c.JID, Name: s.wa.ChatTitle(ctx, c), IsGroup: c.IsGroup, LastMessageAt: formatTime(c.LastMessageAt)}
		if c.LastMessage != nil {
			m := s.message(ctx, *c.LastMessage, false)
			co.LastMessage = &m
		}
		out.Chats = append(out.Chats, co)
	}
	return nil, out, nil
}

type getMessagesIn struct {
	Chat            string `json:"chat" jsonschema:"chat JID, phone number or contact/group name"`
	Limit           int    `json:"limit,omitempty" jsonschema:"maximum number of messages (default 30, max 500)"`
	BeforeMessageID string `json:"before_message_id,omitempty" jsonschema:"page backwards: only messages older than this message (pass the id of the oldest message you already have)"`
	Before          string `json:"before,omitempty" jsonschema:"only messages strictly older than this RFC 3339 time, e.g. to jump to a date"`
}

// MessagesOut wraps messages of one chat.
type MessagesOut struct {
	Chat     string       `json:"chat_jid"`
	ChatName string       `json:"chat_name"`
	Messages []MessageOut `json:"messages"`
}

func (s *Server) getMessages(ctx context.Context, _ *mcp.CallToolRequest, in getMessagesIn) (*mcp.CallToolResult, MessagesOut, error) {
	jid, err := s.wa.Resolve(ctx, in.Chat)
	if err != nil {
		return nil, MessagesOut{}, err
	}
	var before time.Time
	if in.Before != "" {
		if before, err = time.Parse(time.RFC3339, in.Before); err != nil {
			return nil, MessagesOut{}, fmt.Errorf("before must be an RFC 3339 time such as 2026-01-31T18:00:00+01:00: %w", err)
		}
	}
	msgs, err := s.wa.Store().Messages(ctx, store.MessagesParams{ChatJID: jid.String(), Before: before, BeforeID: in.BeforeMessageID, Limit: in.Limit})
	if err != nil {
		return nil, MessagesOut{}, err
	}
	out := MessagesOut{Chat: jid.String(), ChatName: s.wa.DisplayName(ctx, jid.String()), Messages: make([]MessageOut, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, s.message(ctx, m, false))
	}
	return nil, out, nil
}

type searchMessagesIn struct {
	Query string `json:"query" jsonschema:"text to look for"`
	Chat  string `json:"chat,omitempty" jsonschema:"restrict to this chat (JID, phone number or name)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 30, max 200)"`
}

// SearchOut wraps search results.
type SearchOut struct {
	Messages []MessageOut `json:"messages"`
}

func (s *Server) searchMessages(ctx context.Context, _ *mcp.CallToolRequest, in searchMessagesIn) (*mcp.CallToolResult, SearchOut, error) {
	p := store.SearchParams{Query: in.Query, Limit: in.Limit}
	if in.Chat != "" {
		jid, err := s.wa.Resolve(ctx, in.Chat)
		if err != nil {
			return nil, SearchOut{}, err
		}
		p.ChatJID = jid.String()
	}
	msgs, err := s.wa.Store().Search(ctx, p)
	if err != nil {
		return nil, SearchOut{}, err
	}
	out := SearchOut{Messages: make([]MessageOut, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, s.message(ctx, m, true))
	}
	return nil, out, nil
}

type searchContactsIn struct {
	Query string `json:"query" jsonschema:"part of a name or phone number"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 20)"`
}

// ContactsOut wraps contact matches.
type ContactsOut struct {
	Contacts []wa.Candidate `json:"contacts"`
}

func (s *Server) searchContacts(ctx context.Context, _ *mcp.CallToolRequest, in searchContactsIn) (*mcp.CallToolResult, ContactsOut, error) {
	found, err := s.wa.SearchContacts(ctx, in.Query, in.Limit)
	if err != nil {
		return nil, ContactsOut{}, err
	}
	if found == nil {
		found = []wa.Candidate{}
	}
	return nil, ContactsOut{Contacts: found}, nil
}

type downloadMediaIn struct {
	Chat      string `json:"chat" jsonschema:"chat JID, phone number or name"`
	MessageID string `json:"message_id" jsonschema:"id of the message carrying the media"`
}

// DownloadOut describes a downloaded file.
type DownloadOut struct {
	Path     string `json:"path"`
	MimeType string `json:"mime_type,omitempty"`
}

func (s *Server) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in downloadMediaIn) (*mcp.CallToolResult, DownloadOut, error) {
	jid, err := s.wa.Resolve(ctx, in.Chat)
	if err != nil {
		return nil, DownloadOut{}, err
	}
	path, m, err := s.wa.Download(ctx, jid, in.MessageID, s.mediaDir)
	if err != nil {
		return nil, DownloadOut{}, err
	}
	return nil, DownloadOut{Path: path, MimeType: m.MimeType}, nil
}

type sendMessageIn struct {
	To        string `json:"to" jsonschema:"recipient: JID, international phone number (+33612345678) or exact contact/group name"`
	Text      string `json:"text" jsonschema:"message text"`
	ReplyToID string `json:"reply_to_message_id,omitempty" jsonschema:"id of a message in the same chat to reply to"`
}

// SentOut confirms a sent message.
type SentOut struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	ToName string `json:"to_name"`
	Time   string `json:"time"`
}

func (s *Server) sendMessage(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageIn) (*mcp.CallToolResult, SentOut, error) {
	jid, err := s.wa.Resolve(ctx, in.To)
	if err != nil {
		return nil, SentOut{}, err
	}
	sent, err := s.wa.SendText(ctx, jid, in.Text, in.ReplyToID)
	if err != nil {
		return nil, SentOut{}, err
	}
	return nil, s.sentOut(ctx, jid, sent), nil
}

type sendFileIn struct {
	To      string `json:"to" jsonschema:"recipient: JID, international phone number or exact contact/group name"`
	Path    string `json:"path" jsonschema:"absolute path of the local file to send"`
	Caption string `json:"caption,omitempty" jsonschema:"optional caption"`
}

func (s *Server) sendFile(ctx context.Context, _ *mcp.CallToolRequest, in sendFileIn) (*mcp.CallToolResult, SentOut, error) {
	jid, err := s.wa.Resolve(ctx, in.To)
	if err != nil {
		return nil, SentOut{}, err
	}
	sent, err := s.wa.SendFile(ctx, jid, in.Path, in.Caption)
	if err != nil {
		return nil, SentOut{}, err
	}
	return nil, s.sentOut(ctx, jid, sent), nil
}

func (s *Server) sentOut(ctx context.Context, jid types.JID, sent wa.Sent) SentOut {
	return SentOut{ID: sent.ID, To: jid.String(), ToName: s.wa.DisplayName(ctx, jid.String()), Time: formatTime(sent.Timestamp)}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(time.RFC3339)
}

func ptr[T any](v T) *T { return &v }
