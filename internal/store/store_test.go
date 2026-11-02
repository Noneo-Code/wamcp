package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func at(minute int) time.Time { return time.Date(2026, 1, 1, 12, minute, 0, 0, time.UTC) }

func mustSave(t *testing.T, s *Store, m Message) {
	t.Helper()
	if err := s.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("SaveMessage(%s): %v", m.ID, err)
	}
}

func TestListChatsOrderAndLastMessage(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustSave(t, s, Message{ChatJID: "a@s.whatsapp.net", ID: "1", Timestamp: at(1), Text: "old"})
	mustSave(t, s, Message{ChatJID: "b@s.whatsapp.net", ID: "2", Timestamp: at(2), Text: "middle"})
	mustSave(t, s, Message{ChatJID: "a@s.whatsapp.net", ID: "3", Timestamp: at(3), Text: "newest", FromMe: true})
	if err := s.UpsertChat(ctx, "a@s.whatsapp.net", "Alice", false, time.Time{}); err != nil {
		t.Fatal(err)
	}

	chats, err := s.ListChats(ctx, ListChatsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 2 || chats[0].JID != "a@s.whatsapp.net" || chats[1].JID != "b@s.whatsapp.net" {
		t.Fatalf("unexpected order: %+v", chats)
	}
	if chats[0].Name != "Alice" {
		t.Errorf("name = %q, want Alice", chats[0].Name)
	}
	if lm := chats[0].LastMessage; lm == nil || lm.Text != "newest" || !lm.FromMe {
		t.Errorf("last message = %+v", lm)
	}
	if !chats[0].LastMessageAt.Equal(at(3)) {
		t.Errorf("last_message_at = %v", chats[0].LastMessageAt)
	}
}

func TestUpsertChatKeepsKnownName(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.UpsertChat(ctx, "g@g.us", "Team", true, at(5)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertChat(ctx, "g@g.us", "", false, at(1)); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetChat(ctx, "g@g.us")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Team" || !c.IsGroup || !c.LastMessageAt.Equal(at(5)) {
		t.Errorf("chat = %+v", c)
	}
}

func TestListChatsQueryEscapesWildcards(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	_ = s.UpsertChat(ctx, "1@s.whatsapp.net", "100% Bio", false, at(1))
	_ = s.UpsertChat(ctx, "2@s.whatsapp.net", "1000 Bio", false, at(2))

	chats, err := s.ListChats(ctx, ListChatsParams{Query: "0%"})
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 1 || chats[0].Name != "100% Bio" {
		t.Fatalf("got %+v", chats)
	}
}

func TestMessagesPaginationIsChronological(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for i := 1; i <= 5; i++ {
		mustSave(t, s, Message{ChatJID: "c@s.whatsapp.net", ID: string(rune('0' + i)), Timestamp: at(i), Text: "m"})
	}
	page, err := s.Messages(ctx, MessagesParams{ChatJID: "c@s.whatsapp.net", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != "4" || page[1].ID != "5" {
		t.Fatalf("latest page = %+v", ids(page))
	}
	older, err := s.Messages(ctx, MessagesParams{ChatJID: "c@s.whatsapp.net", Limit: 2, Before: page[0].Timestamp})
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 2 || older[0].ID != "2" || older[1].ID != "3" {
		t.Fatalf("older page = %+v", ids(older))
	}
}

func TestEditAndDeleteSurviveResync(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	orig := Message{ChatJID: "c@s.whatsapp.net", ID: "x", Timestamp: at(1), Text: "typo", Raw: []byte{1}}
	mustSave(t, s, orig)
	if err := s.EditMessage(ctx, orig.ChatJID, "x", orig.SenderJID, "fixed"); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s, orig) // history sync delivers the original again
	m, err := s.GetMessage(ctx, orig.ChatJID, "x")
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "fixed" || !m.Edited {
		t.Errorf("after edit+resync: %+v", m)
	}

	if _, err := s.DeleteMessage(ctx, orig.ChatJID, "x", orig.SenderJID, false); err != nil {
		t.Fatal(err)
	}
	mustSave(t, s, orig)
	m, _ = s.GetMessage(ctx, orig.ChatJID, "x")
	if !m.Deleted || m.Text != "" || m.Raw != nil {
		t.Errorf("after delete+resync: %+v", m)
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustSave(t, s, Message{ChatJID: "a@s.whatsapp.net", ID: "1", Timestamp: at(1), Text: "Facture de mars"})
	mustSave(t, s, Message{ChatJID: "b@s.whatsapp.net", ID: "2", Timestamp: at(2), Text: "la FACTURE est payée"})
	mustSave(t, s, Message{ChatJID: "b@s.whatsapp.net", ID: "3", Timestamp: at(3), MediaType: "document", Filename: "facture.pdf"})
	mustSave(t, s, Message{ChatJID: "b@s.whatsapp.net", ID: "4", Timestamp: at(4), Text: "rien"})

	all, err := s.Search(ctx, SearchParams{Query: "facture"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); len(got) != 3 || got[0] != "3" {
		t.Fatalf("search all = %v", got)
	}
	inB, _ := s.Search(ctx, SearchParams{Query: "facture", ChatJID: "b@s.whatsapp.net"})
	if len(inB) != 2 {
		t.Fatalf("search in chat = %v", ids(inB))
	}
	if _, err := s.Search(ctx, SearchParams{Query: "  "}); err == nil {
		t.Error("expected error for empty query")
	}
}

func TestGetMessageNotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetMessage(context.Background(), "c", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetChat(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func ids(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func TestMessagesCursorWithinSameSecond(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		mustSave(t, s, Message{ChatJID: "c@s.whatsapp.net", ID: id, Timestamp: at(1), Text: id})
	}
	page, err := s.Messages(ctx, MessagesParams{ChatJID: "c@s.whatsapp.net", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(page); len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Fatalf("first page = %v", got)
	}
	older, err := s.Messages(ctx, MessagesParams{ChatJID: "c@s.whatsapp.net", Limit: 10, BeforeID: page[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(older); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("older page = %v (same-second messages must not be skipped)", got)
	}
	if _, err := s.Messages(ctx, MessagesParams{ChatJID: "c@s.whatsapp.net", BeforeID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cursor: err = %v", err)
	}
}

func TestRenameChatDoesNotCreateChats(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if err := s.RenameChat(ctx, "stranger@s.whatsapp.net", "Stranger"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetChat(ctx, "stranger@s.whatsapp.net"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RenameChat created a chat: %v", err)
	}
	mustSave(t, s, Message{ChatJID: "friend@s.whatsapp.net", ID: "1", Timestamp: at(1)})
	_ = s.RenameChat(ctx, "friend@s.whatsapp.net", "Friend")
	if c, _ := s.GetChat(ctx, "friend@s.whatsapp.net"); c.Name != "Friend" {
		t.Errorf("name = %q", c.Name)
	}
}

func TestOnlySenderCanOverwriteEditOrDelete(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	const chat = "g@g.us"
	orig := Message{ChatJID: chat, ID: "id1", SenderJID: "alice@s.whatsapp.net", Timestamp: at(1), Text: "rendez-vous à 18h"}
	mustSave(t, s, orig)

	// Mallory reuses Alice's message ID.
	mustSave(t, s, Message{ChatJID: chat, ID: "id1", SenderJID: "mallory@s.whatsapp.net", Timestamp: at(2), Text: "rendez-vous annulé"})
	if m, _ := s.GetMessage(ctx, chat, "id1"); m.Text != orig.Text || m.SenderJID != orig.SenderJID {
		t.Fatalf("message overwritten by another sender: %+v", m)
	}
	// Mallory edits and deletes it.
	_ = s.EditMessage(ctx, chat, "id1", "mallory@s.whatsapp.net", "hacked")
	if deleted, _ := s.DeleteMessage(ctx, chat, "id1", "mallory@s.whatsapp.net", false); deleted {
		t.Fatal("non-sender deleted the message")
	}
	if m, _ := s.GetMessage(ctx, chat, "id1"); m.Text != orig.Text || m.Deleted {
		t.Fatalf("message altered by another sender: %+v", m)
	}
	// Alice can; a verified admin can delete.
	_ = s.EditMessage(ctx, chat, "id1", "alice@s.whatsapp.net", "rendez-vous à 19h")
	if m, _ := s.GetMessage(ctx, chat, "id1"); m.Text != "rendez-vous à 19h" {
		t.Fatalf("sender edit not applied: %+v", m)
	}
	if deleted, _ := s.DeleteMessage(ctx, chat, "id1", "admin@s.whatsapp.net", true); !deleted {
		t.Fatal("admin delete not applied")
	}
}
