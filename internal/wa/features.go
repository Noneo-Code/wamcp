package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
)

// SendReaction reacts to a stored message with an emoji; an empty emoji
// removes your reaction.
func (c *Client) SendReaction(ctx context.Context, chat types.JID, messageID, emoji string) (Sent, error) {
	m, err := c.store.GetMessage(ctx, chat.String(), messageID)
	if err != nil {
		return Sent{}, fmt.Errorf("message %s: %w", messageID, err)
	}
	sender := c.OwnJID()
	if !m.FromMe {
		if sender, err = types.ParseJID(m.SenderJID); err != nil {
			return Sent{}, fmt.Errorf("sender of %s: %w", messageID, err)
		}
	}
	if !c.Connected() {
		return Sent{}, errors.New("not connected to WhatsApp")
	}
	resp, err := c.wm.SendMessage(ctx, chat, c.wm.BuildReaction(chat, sender, messageID, emoji))
	if err != nil {
		return Sent{}, fmt.Errorf("send reaction: %w", err)
	}
	c.logErr(c.store.SetReaction(ctx, chat.String(), messageID, c.OwnJID().String(), emoji, resp.Timestamp))
	return Sent{ID: resp.ID, ChatJID: chat.String(), Timestamp: resp.Timestamp}, nil
}

// DeleteForEveryone revokes one of your own messages for every participant.
func (c *Client) DeleteForEveryone(ctx context.Context, chat types.JID, messageID string) error {
	m, err := c.store.GetMessage(ctx, chat.String(), messageID)
	if err != nil {
		return fmt.Errorf("message %s: %w", messageID, err)
	}
	if !m.FromMe {
		return errors.New("only your own messages can be deleted for everyone")
	}
	if m.Deleted {
		return errors.New("message is already deleted")
	}
	if !c.Connected() {
		return errors.New("not connected to WhatsApp")
	}
	if _, err := c.wm.SendMessage(ctx, chat, c.wm.BuildRevoke(chat, types.EmptyJID, messageID)); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	_, err = c.store.DeleteMessage(ctx, chat.String(), messageID, c.OwnJID().String(), false)
	return err
}

// Participant is a group member.
type Participant struct {
	JID   string `json:"jid"`
	Name  string `json:"name"`
	Admin bool   `json:"admin,omitempty"`
	Owner bool   `json:"owner,omitempty"`
}

// Group describes a group chat.
type Group struct {
	JID          string        `json:"jid"`
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	Participants []Participant `json:"participants"`
}

// GroupInfo fetches a group's subject, description and members from WhatsApp.
func (c *Client) GroupInfo(ctx context.Context, chat types.JID) (Group, error) {
	if chat.Server != types.GroupServer {
		return Group{}, fmt.Errorf("%s is not a group", chat)
	}
	if !c.Connected() {
		return Group{}, errors.New("not connected to WhatsApp")
	}
	info, err := c.wm.GetGroupInfo(ctx, chat)
	if err != nil {
		return Group{}, fmt.Errorf("group info: %w", err)
	}
	g := Group{JID: chat.String(), Name: info.Name, Description: info.Topic}
	for _, p := range info.Participants {
		jid := p.PhoneNumber
		if jid.IsEmpty() {
			jid = c.normalize(ctx, p.JID, types.EmptyJID)
		}
		name := c.DisplayName(ctx, jid.String())
		if name == jid.String() && p.DisplayName != "" {
			name = p.DisplayName
		}
		g.Participants = append(g.Participants, Participant{
			JID: jid.String(), Name: name, Admin: p.IsAdmin || p.IsSuperAdmin, Owner: p.IsSuperAdmin,
		})
	}
	return g, nil
}

// backfillTimeout bounds the wait for the phone to answer a history request.
var backfillTimeout = 30 * time.Second

// LoadOlder asks the phone for up to count messages older than the oldest
// archived one in chat, and returns how many new messages were stored. The
// phone must be online; it can only send what it still has.
func (c *Client) LoadOlder(ctx context.Context, chat types.JID, count int) (int, error) {
	if count <= 0 {
		count = 50
	}
	count = min(count, 200)
	oldest, err := c.store.OldestMessage(ctx, chat.String())
	if err != nil {
		return 0, fmt.Errorf("no archived message in %s to start from (WhatsApp needs one): %w", chat, err)
	}
	if !c.Connected() {
		return 0, errors.New("not connected to WhatsApp")
	}
	before, err := c.store.CountMessages(ctx, chat.String())
	if err != nil {
		return 0, err
	}

	c.backfillMu.Lock()
	defer c.backfillMu.Unlock()
	// The phone files 1:1 chats under the LID or the phone number depending on
	// the account's migration state: try the most likely one first.
	candidates := []types.JID{c.peerChat(ctx, chat)}
	if candidates[0] != chat {
		candidates = append(candidates, chat)
	}
	answered := false
	for _, peer := range candidates {
		select { // drop a stale signal
		case <-c.onDemandSync:
		default:
		}
		info := &types.MessageInfo{
			MessageSource: types.MessageSource{Chat: peer, IsFromMe: oldest.FromMe, IsGroup: chat.Server == types.GroupServer},
			ID:            oldest.ID,
			Timestamp:     oldest.Timestamp,
		}
		if _, err := c.wm.SendPeerMessage(ctx, c.wm.BuildHistorySyncRequest(info, count)); err != nil {
			return 0, fmt.Errorf("request history: %w", err)
		}
		select {
		case <-c.onDemandSync:
			answered = true
		case <-time.After(backfillTimeout):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if answered {
			break
		}
	}
	if !answered {
		return 0, errors.New("the phone did not answer the history request (it must be online with WhatsApp reachable)")
	}
	after, err := c.store.CountMessages(ctx, chat.String())
	if err != nil {
		return 0, err
	}
	return after - before, nil
}

// saveHistoryReactions archives the reactions a history sync attaches to a message.
func (c *Client) saveHistoryReactions(ctx context.Context, chat types.JID, msg *waWeb.WebMessageInfo) {
	for _, r := range msg.GetReactions() {
		key := r.GetKey()
		var reactor types.JID
		switch {
		case key.GetFromMe():
			reactor = c.OwnJID()
		case key.GetParticipant() != "":
			j, err := types.ParseJID(key.GetParticipant())
			if err != nil {
				continue
			}
			reactor = c.normalize(ctx, j, types.EmptyJID)
		case chat.Server != types.GroupServer:
			reactor = chat
		default:
			continue
		}
		at := time.UnixMilli(r.GetSenderTimestampMS())
		c.logErr(c.store.SetReaction(ctx, chat.String(), msg.GetKey().GetID(), reactor.String(), r.GetText(), at))
	}
}

// mentionText rewrites "@<phone>" mentions for the given users and returns the
// JIDs to list as mentioned. In groups addressed by LID, WhatsApp expects the
// LID both in the text and in the mention list.
func (c *Client) mentionText(ctx context.Context, chat types.JID, text string, users []types.JID) (string, []string, error) {
	lidGroup := false
	if chat.Server == types.GroupServer && len(users) > 0 {
		if info, err := c.wm.GetGroupInfo(ctx, chat); err == nil && info.AddressingMode == types.AddressingModeLID {
			lidGroup = true
		}
	}
	var jids []string
	for _, u := range users {
		tag := "@" + u.User
		if !strings.Contains(text, tag) {
			return "", nil, fmt.Errorf("the text must contain %s where %s is mentioned", tag, c.DisplayName(ctx, u.String()))
		}
		target := u
		if lidGroup && u.Server == types.DefaultUserServer {
			if lid, err := c.wm.Store.LIDs.GetLIDForPN(ctx, u); err == nil && !lid.IsEmpty() {
				target = lid
				text = strings.ReplaceAll(text, tag, "@"+lid.User)
			}
		}
		jids = append(jids, target.String())
	}
	return text, jids, nil
}
