// Package wa connects to WhatsApp (as a linked device, through whatsmeow) and
// mirrors every message it sees into the local store.
package wa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/Noneo-Code/wamcp/internal/store"
)

// ErrNotLoggedIn means no WhatsApp account is linked yet (run `wamcp login`).
var ErrNotLoggedIn = errors.New("no WhatsApp account linked: run `wamcp login` first")

// Client is a WhatsApp linked device backed by a local message archive.
type Client struct {
	wm        *whatsmeow.Client
	container *sqlstore.Container
	store     *store.Store
	log       waLog.Logger

	groupLookups sync.Map // group JID -> struct{}: name fetch already attempted
	files        FilePolicy

	syncMu      sync.Mutex
	lastSyncAt  time.Time // last history sync batch, used by `login` to wait for the initial sync
	syncBatches int
}

// Open loads the session database at sessionPath and wires events into st.
func Open(ctx context.Context, sessionPath string, st *store.Store, log waLog.Logger) (*Client, error) {
	dsn := "file:" + sessionPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, log.Sub("Database"))
	if err != nil {
		return nil, fmt.Errorf("open session database: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("load device: %w", err)
	}
	c := &Client{
		wm:        whatsmeow.NewClient(device, log.Sub("Client")),
		container: container,
		store:     st,
		log:       log,
	}
	c.wm.AddEventHandler(c.handleEvent)
	return c, nil
}

// LoggedIn reports whether an account is linked to this device.
func (c *Client) LoggedIn() bool { return c.wm.Store.ID != nil }

// Connected reports whether the WebSocket to WhatsApp is up.
func (c *Client) Connected() bool { return c.wm.IsConnected() }

// OwnJID is the linked account (zero value when not logged in).
func (c *Client) OwnJID() types.JID {
	if c.wm.Store.ID == nil {
		return types.JID{}
	}
	return c.wm.Store.ID.ToNonAD()
}

// Connect opens the connection for an already linked account. Messages start
// flowing into the store immediately.
func (c *Client) Connect() error {
	if !c.LoggedIn() {
		return ErrNotLoggedIn
	}
	if err := c.wm.Connect(); err != nil {
		return fmt.Errorf("connect to WhatsApp: %w", err)
	}
	return nil
}

// Close disconnects and releases the session database.
func (c *Client) Close() error {
	c.wm.Disconnect()
	return c.container.Close()
}

// Login links this device to a WhatsApp account. Without phone, a QR code is
// printed to out; with phone (international format), a pairing code is
// printed instead, to be typed in WhatsApp > Linked devices > Link with phone
// number. It returns once the device is paired and connected.
func (c *Client) Login(ctx context.Context, phone string, out io.Writer) error {
	if c.LoggedIn() {
		return errors.New("already linked: run `wamcp logout` first to link another account")
	}
	qrChan, err := c.wm.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	if err := c.wm.Connect(); err != nil {
		return fmt.Errorf("connect to WhatsApp: %w", err)
	}

	if phone != "" {
		jid, err := ParsePhone(phone)
		if err != nil {
			return err
		}
		// The pairing code can only be requested once the first QR event arrived.
		select {
		case <-qrChan:
		case <-ctx.Done():
			return ctx.Err()
		}
		code, err := c.wm.PairPhone(ctx, jid.User, true, whatsmeow.PairClientChrome, pairDisplayName())
		if err != nil {
			return fmt.Errorf("request pairing code: %w", err)
		}
		fmt.Fprintf(out, "\nOn your phone: WhatsApp > Linked devices > Link a device > Link with phone number instead\nEnter this code: %s\n\n", code)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item, ok := <-qrChan:
			if !ok {
				if c.LoggedIn() {
					return nil
				}
				return errors.New("pairing ended without success")
			}
			switch item.Event {
			case "code":
				if phone == "" {
					fmt.Fprintln(out, "\nScan this QR code with WhatsApp > Linked devices > Link a device:")
					qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, out)
				}
			case whatsmeow.QRChannelSuccess.Event:
				return nil
			case whatsmeow.QRChannelTimeout.Event:
				return errors.New("pairing timed out, run `wamcp login` again")
			case "error":
				return fmt.Errorf("pairing failed: %w", item.Error)
			default:
				if item.Error != nil {
					return fmt.Errorf("pairing failed (%s): %w", item.Event, item.Error)
				}
			}
		}
	}
}

// pairDisplayName must be "Browser (OS)" with a browser and OS WhatsApp knows,
// otherwise the pairing request is rejected.
func pairDisplayName() string {
	switch runtime.GOOS {
	case "darwin":
		return "Chrome (macOS)"
	case "windows":
		return "Chrome (Windows)"
	default:
		return "Chrome (Linux)"
	}
}

// WaitForHistorySync blocks until no history batch arrived for idle, or until
// maxWait elapses. It returns the number of batches received.
func (c *Client) WaitForHistorySync(ctx context.Context, idle, maxWait time.Duration) int {
	deadline := time.Now().Add(maxWait)
	start := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return c.batches()
		case now := <-ticker.C:
			c.syncMu.Lock()
			last, n := c.lastSyncAt, c.syncBatches
			c.syncMu.Unlock()
			if last.IsZero() {
				last = start
			}
			if now.Sub(last) >= idle || now.After(deadline) {
				return n
			}
		}
	}
}

func (c *Client) batches() int {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	return c.syncBatches
}

// Logout unlinks the device from the account and wipes the session.
func (c *Client) Logout(ctx context.Context) error {
	if !c.LoggedIn() {
		return ErrNotLoggedIn
	}
	if !c.wm.IsConnected() {
		if err := c.wm.Connect(); err != nil {
			return fmt.Errorf("connect to WhatsApp: %w", err)
		}
	}
	// Connect returns after the handshake; the logout request needs the
	// session to be authenticated first.
	if !c.WaitAuthenticated(ctx, 20*time.Second) {
		return errors.New("could not authenticate with WhatsApp (if the device was already removed from the phone, the local session is gone: nothing left to do)")
	}
	if err := c.wm.Logout(ctx); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// WaitAuthenticated waits until the connection is logged in, up to timeout.
func (c *Client) WaitAuthenticated(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !c.wm.IsLoggedIn() {
		if time.Now().After(deadline) || !c.LoggedIn() {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return true
}

func (c *Client) handleEvent(evt any) {
	// Handlers run synchronously in whatsmeow's event loop: keep them bounded.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch e := evt.(type) {
	case *events.Message:
		c.saveMessage(ctx, e)
	case *events.HistorySync:
		// Full syncs can hold tens of thousands of messages: no deadline, so a
		// slow disk cannot make the tail of the sync silently fail.
		c.saveHistory(context.Background(), e)
	case *events.Connected:
		go c.refreshNames(context.Background())
	case *events.PushName:
		c.refreshChatName(ctx, c.normalize(ctx, e.JID, types.EmptyJID))
	case *events.LoggedOut:
		c.log.Errorf("WhatsApp logged this device out (reason: %v): run `wamcp login` again", e.Reason)
	case *events.StreamReplaced:
		c.log.Errorf("another client using the same session took over the connection")
	}
}

// saveMessage archives a message event and reports whether a message was stored.
func (c *Client) saveMessage(ctx context.Context, e *events.Message) bool {
	if skipChat(e.Info.Chat) {
		return false
	}
	alt := e.Info.SenderAlt
	if e.Info.IsFromMe {
		alt = e.Info.RecipientAlt
	}
	chat := e.Info.Chat
	if !e.Info.IsGroup {
		chat = c.normalize(ctx, chat, alt)
	}

	sender := c.normalize(ctx, e.Info.Sender, e.Info.SenderAlt)
	if e.Info.IsFromMe {
		sender = c.OwnJID()
	}

	if pm := e.Message.GetProtocolMessage(); pm != nil {
		target := pm.GetKey().GetID()
		switch pm.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			c.revoke(ctx, chat, target, sender, e.Info.IsGroup)
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if ct, ok := extractContent(pm.GetEditedMessage()); ok {
				c.logErr(c.store.EditMessage(ctx, chat.String(), target, sender.String(), ct.Text))
			}
		}
		return false
	}

	ct, ok := extractContent(e.Message)
	if !ok {
		return false
	}
	if e.IsEdit {
		// History sync delivers edits already merged into the original ID but
		// with the edit's timestamp: only update the text, keep the position.
		c.logErr(c.store.EditMessage(ctx, chat.String(), e.Info.ID, sender.String(), ct.Text))
		return false
	}
	err := c.store.SaveMessage(ctx, store.Message{
		ChatJID:   chat.String(),
		ID:        e.Info.ID,
		SenderJID: sender.String(),
		FromMe:    e.Info.IsFromMe,
		Timestamp: e.Info.Timestamp,
		Text:      ct.Text,
		MediaType: ct.MediaType,
		Filename:  ct.Filename,
		MimeType:  ct.MimeType,
		Raw:       ct.Raw,
		Edited:    e.IsEdit,
	})
	if err != nil {
		c.log.Errorf("store message %s: %v", e.Info.ID, err)
		return false
	}
	if e.Info.IsGroup {
		c.ensureGroupName(chat)
	} else {
		c.refreshChatName(ctx, chat)
	}
	return true
}

func (c *Client) saveHistory(ctx context.Context, e *events.HistorySync) {
	stored := 0
	for _, conv := range e.Data.GetConversations() {
		raw, err := types.ParseJID(conv.GetID())
		if err != nil || skipChat(raw) {
			continue
		}
		isGroup := raw.Server == types.GroupServer
		chat := raw
		if !isGroup {
			// whatsmeow stores the sync's LID<->phone mappings asynchronously,
			// so they may not be queryable yet: use the ones on the conversation.
			c.learnLIDMapping(ctx, raw, conv.GetLidJID(), conv.GetPnJID())
			chat = c.normalize(ctx, raw, types.EmptyJID)
		}
		c.logErr(c.store.UpsertChat(ctx, chat.String(), conv.GetName(), isGroup, time.Time{}))
		for _, hm := range conv.GetMessages() {
			evt, err := c.wm.ParseWebMessage(raw, hm.GetMessage())
			if err != nil {
				continue
			}
			if c.saveMessage(ctx, evt) {
				stored++
			}
		}
	}
	c.syncMu.Lock()
	c.lastSyncAt = time.Now()
	c.syncBatches++
	c.syncMu.Unlock()
	c.log.Infof("history sync (%s): %d messages", e.Data.GetSyncType(), stored)
}

// revoke applies a "delete for everyone": allowed for the message's sender, and
// in groups for admins (checked against WhatsApp, since anyone can send a revoke).
func (c *Client) revoke(ctx context.Context, chat types.JID, target string, revoker types.JID, isGroup bool) {
	deleted, err := c.store.DeleteMessage(ctx, chat.String(), target, revoker.String(), false)
	if err != nil || deleted || !isGroup {
		c.logErr(err)
		return
	}
	if _, err := c.store.MessageSender(ctx, chat.String(), target); err != nil {
		return // unknown message, nothing to delete
	}
	info, err := c.wm.GetGroupInfo(ctx, chat)
	if err != nil {
		c.log.Warnf("ignoring revoke of %s: cannot check admins of %s: %v", target, chat, err)
		return
	}
	for _, p := range info.Participants {
		if !p.IsAdmin && !p.IsSuperAdmin {
			continue
		}
		for _, j := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
			if !j.IsEmpty() && c.normalize(ctx, j, types.EmptyJID) == revoker {
				_, err := c.store.DeleteMessage(ctx, chat.String(), target, revoker.String(), true)
				c.logErr(err)
				return
			}
		}
	}
	c.log.Warnf("ignoring revoke of %s in %s by non-admin %s", target, chat, revoker)
}

// learnLIDMapping records the LID<->phone-number pair a history conversation carries.
func (c *Client) learnLIDMapping(ctx context.Context, id types.JID, lidStr, pnStr string) {
	lid, pn := types.EmptyJID, types.EmptyJID
	switch id.Server {
	case types.HiddenUserServer:
		lid = id
	case types.DefaultUserServer:
		pn = id
	}
	if j, err := types.ParseJID(lidStr); err == nil && j.Server == types.HiddenUserServer {
		lid = j
	}
	if j, err := types.ParseJID(pnStr); err == nil && j.Server == types.DefaultUserServer {
		pn = j
	}
	if !lid.IsEmpty() && !pn.IsEmpty() {
		c.wm.StoreLIDPNMapping(ctx, lid.ToNonAD(), pn.ToNonAD())
	}
}

// normalize maps a LID (WhatsApp's privacy identifier) to the phone-number JID
// when it is known, so one person does not end up as two chats.
func (c *Client) normalize(ctx context.Context, jid, alt types.JID) types.JID {
	jid = jid.ToNonAD()
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	if alt.Server == types.DefaultUserServer {
		return alt.ToNonAD()
	}
	if pn, err := c.wm.Store.LIDs.GetPNForLID(ctx, jid); err == nil && !pn.IsEmpty() {
		return pn.ToNonAD()
	}
	return jid
}

func (c *Client) ensureGroupName(chat types.JID) {
	if _, done := c.groupLookups.LoadOrStore(chat.String(), struct{}{}); done {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		info, err := c.wm.GetGroupInfo(ctx, chat)
		if err != nil {
			c.log.Warnf("fetch group name for %s: %v", chat, err)
			return
		}
		c.logErr(c.store.UpsertChat(ctx, chat.String(), info.Name, true, time.Time{}))
	}()
}

func (c *Client) refreshChatName(ctx context.Context, chat types.JID) {
	if name := c.contactName(ctx, chat); name != "" {
		c.logErr(c.store.RenameChat(ctx, chat.String(), name))
	}
}

// refreshNames fills group subjects and contact names for every known chat.
func (c *Client) refreshNames(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if groups, err := c.wm.GetJoinedGroups(ctx); err == nil {
		for _, g := range groups {
			c.groupLookups.Store(g.JID.String(), struct{}{})
			c.logErr(c.store.UpsertChat(ctx, g.JID.String(), g.Name, true, time.Time{}))
		}
	} else {
		c.log.Warnf("list groups: %v", err)
	}
	chats, err := c.store.AllChats(ctx)
	if err != nil {
		c.log.Warnf("refresh names: %v", err)
		return
	}
	for _, ch := range chats {
		jid, err := types.ParseJID(ch.JID)
		if err != nil || ch.IsGroup {
			continue
		}
		c.refreshChatName(ctx, jid)
	}
}

// contactName is the best human name WhatsApp gave us for a user, or "".
func (c *Client) contactName(ctx context.Context, jid types.JID) string {
	candidates := []types.JID{jid}
	if jid.Server == types.DefaultUserServer {
		if lid, err := c.wm.Store.LIDs.GetLIDForPN(ctx, jid); err == nil && !lid.IsEmpty() {
			candidates = append(candidates, lid)
		}
	}
	for _, j := range candidates {
		info, err := c.wm.Store.Contacts.GetContact(ctx, j)
		if err != nil || !info.Found {
			continue
		}
		for _, n := range []string{info.FullName, info.FirstName, info.BusinessName, info.PushName} {
			if n != "" {
				return n
			}
		}
	}
	return ""
}

func (c *Client) logErr(err error) {
	if err != nil {
		c.log.Errorf("%v", err)
	}
}

// skipChat filters out status updates, broadcast lists, newsletters and the
// WhatsApp system account.
func skipChat(j types.JID) bool {
	return j.IsEmpty() ||
		j.Server == types.BroadcastServer ||
		j.Server == types.NewsletterServer ||
		j == types.PSAJID
}
