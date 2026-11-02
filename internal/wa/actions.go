package wa

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/Noneo-Code/wamcp/internal/store"
)

// MaxUploadSize bounds files sent with SendFile (WhatsApp's own document limit is 2 GB,
// but anything this large is almost certainly a mistake when driven by an agent).
const MaxUploadSize = 100 << 20

var phoneChars = regexp.MustCompile(`[\s\-.()/]`)

// ParsePhone turns an international phone number ("+33 6 12 34 56 78",
// "0033612345678", "33612345678") into a user JID. Local numbers without a
// country code are rejected because they are ambiguous.
func ParsePhone(s string) (types.JID, error) {
	n := phoneChars.ReplaceAllString(strings.TrimSpace(s), "")
	switch {
	case strings.HasPrefix(n, "+"):
		n = n[1:]
	case strings.HasPrefix(n, "00"):
		n = n[2:]
	case strings.HasPrefix(n, "0"):
		return types.JID{}, fmt.Errorf("%q has no country code: use the international format, e.g. +33612345678", s)
	}
	if len(n) < 8 || len(n) > 15 || strings.Trim(n, "0123456789") != "" {
		return types.JID{}, fmt.Errorf("%q is not a valid phone number", s)
	}
	return types.NewJID(n, types.DefaultUserServer), nil
}

// looksLikePhone reports whether s is meant as a phone number rather than a name.
func looksLikePhone(s string) bool {
	n := phoneChars.ReplaceAllString(strings.TrimSpace(s), "")
	n = strings.TrimPrefix(n, "+")
	return n != "" && strings.Trim(n, "0123456789") == ""
}

// Candidate is a chat or contact matching a name lookup.
type Candidate struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
}

// AmbiguousError lists the matches when a name designates several chats.
type AmbiguousError struct {
	Query      string
	Candidates []Candidate
}

func (e *AmbiguousError) Error() string {
	parts := make([]string, len(e.Candidates))
	for i, c := range e.Candidates {
		parts[i] = fmt.Sprintf("%s (%s)", c.Name, c.JID)
	}
	return fmt.Sprintf("%q matches several chats, use one of these JIDs: %s", e.Query, strings.Join(parts, ", "))
}

// Resolve turns user input (JID, phone number or chat/contact name) into a JID.
func (c *Client) Resolve(ctx context.Context, input string) (types.JID, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return types.JID{}, errors.New("no chat or recipient given")
	}
	if strings.Contains(input, "@") {
		jid, err := types.ParseJID(input)
		if err != nil {
			return types.JID{}, fmt.Errorf("invalid JID %q: %w", input, err)
		}
		if jid.Server == types.HiddenUserServer || jid.Server == types.DefaultUserServer {
			return c.normalize(ctx, jid, types.EmptyJID), nil
		}
		return jid.ToNonAD(), nil
	}
	if looksLikePhone(input) {
		return ParsePhone(input)
	}

	matches, err := c.SearchContacts(ctx, input, 500)
	if err != nil {
		return types.JID{}, err
	}
	// An exact (case-insensitive) name match wins over partial ones.
	var exact []Candidate
	for _, m := range matches {
		if strings.EqualFold(m.Name, input) {
			exact = append(exact, m)
		}
	}
	if len(exact) > 0 {
		matches = exact
	}
	if len(matches) > 10 {
		matches = matches[:10] // enough for the model to ask the user which one
	}
	switch len(matches) {
	case 0:
		return types.JID{}, fmt.Errorf("no chat or contact named %q (try a phone number in international format)", input)
	case 1:
		return types.ParseJID(matches[0].JID)
	default:
		return types.JID{}, &AmbiguousError{Query: input, Candidates: matches}
	}
}

// SearchContacts finds chats and address-book contacts whose name, phone or
// JID contains query.
func (c *Client) SearchContacts(ctx context.Context, query string, limit int) ([]Candidate, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil, errors.New("search query is empty")
	}
	if limit <= 0 {
		limit = 20
	}
	seen := map[string]Candidate{}

	chats, err := c.store.ListChats(ctx, store.ListChatsParams{Query: query, Limit: 200})
	if err != nil {
		return nil, err
	}
	for _, ch := range chats {
		seen[ch.JID] = Candidate{JID: ch.JID, Name: c.chatTitle(ctx, ch)}
	}

	contacts, err := c.wm.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read contacts: %w", err)
	}
	for jid, info := range contacts {
		names := []string{info.FullName, info.FirstName, info.BusinessName, info.PushName}
		hit := strings.Contains(jid.User, q)
		for _, n := range names {
			if n != "" && strings.Contains(strings.ToLower(n), q) {
				hit = true
			}
		}
		if !hit {
			continue
		}
		pn := c.normalize(ctx, jid, types.EmptyJID)
		if _, dup := seen[pn.String()]; dup {
			continue
		}
		name := ""
		for _, n := range names {
			if n != "" {
				name = n
				break
			}
		}
		seen[pn.String()] = Candidate{JID: pn.String(), Name: name}
	}

	out := make([]Candidate, 0, len(seen))
	for _, cand := range seen {
		out = append(out, cand)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ChatTitle returns a human name for a stored chat: its stored name, the
// contact name, or the phone number.
func (c *Client) chatTitle(ctx context.Context, ch store.Chat) string {
	if ch.Name != "" {
		return ch.Name
	}
	return c.DisplayName(ctx, ch.JID)
}

// DisplayName returns a human name for any JID.
func (c *Client) DisplayName(ctx context.Context, jidStr string) string {
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return jidStr
	}
	if jid.ToNonAD() == c.OwnJID() {
		return "me"
	}
	if jid.Server == types.GroupServer {
		if ch, err := c.store.GetChat(ctx, jidStr); err == nil && ch.Name != "" {
			return ch.Name
		}
		return "group " + jid.User
	}
	if name := c.contactName(ctx, jid); name != "" {
		return name
	}
	if jid.Server == types.DefaultUserServer {
		return "+" + jid.User
	}
	return jidStr
}

// ChatTitle is the exported form of chatTitle.
func (c *Client) ChatTitle(ctx context.Context, ch store.Chat) string { return c.chatTitle(ctx, ch) }

// Store gives read access to the local archive.
func (c *Client) Store() *store.Store { return c.store }

// Sent describes a message that was just sent.
type Sent struct {
	ID        string
	ChatJID   string
	Timestamp time.Time
}

// SendText sends a text message, optionally as a reply to a stored message.
func (c *Client) SendText(ctx context.Context, to types.JID, text, replyToID string) (Sent, error) {
	if strings.TrimSpace(text) == "" {
		return Sent{}, errors.New("message text is empty")
	}
	if err := c.checkRecipient(ctx, to); err != nil {
		return Sent{}, err
	}
	msg := &waE2E.Message{Conversation: proto.String(text)}
	if replyToID != "" {
		quoted, err := c.store.GetMessage(ctx, to.String(), replyToID)
		if err != nil {
			return Sent{}, fmt.Errorf("message %s to reply to: %w", replyToID, err)
		}
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:      proto.String(quoted.ID),
				Participant:   proto.String(quoted.SenderJID),
				QuotedMessage: &waE2E.Message{Conversation: proto.String(quoted.Text)},
			},
		}}
	}
	return c.send(ctx, to, msg, content{Text: text})
}

// SendFile uploads a local file and sends it as an image, video, audio or
// document depending on its type.
func (c *Client) SendFile(ctx context.Context, to types.JID, path, caption string) (Sent, error) {
	path, err := c.files.check(path)
	if err != nil {
		return Sent{}, err
	}
	if err := c.checkRecipient(ctx, to); err != nil {
		return Sent{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Sent{}, fmt.Errorf("file: %w", err)
	}
	if info.IsDir() {
		return Sent{}, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > MaxUploadSize {
		return Sent{}, fmt.Errorf("%s is %d MB, above the %d MB limit", path, info.Size()>>20, MaxUploadSize>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Sent{}, fmt.Errorf("read file: %w", err)
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, _ = strings.Cut(mimeType, ";")
	name := filepath.Base(path)

	kind, mediaType := "document", whatsmeow.MediaDocument
	switch {
	case mimeType == "image/jpeg" || mimeType == "image/png":
		kind, mediaType = "image", whatsmeow.MediaImage
	case mimeType == "video/mp4" || mimeType == "video/3gpp":
		kind, mediaType = "video", whatsmeow.MediaVideo
	case strings.HasPrefix(mimeType, "audio/"):
		kind, mediaType = "audio", whatsmeow.MediaAudio
	}

	up, err := c.wm.Upload(ctx, data, mediaType)
	if err != nil {
		return Sent{}, fmt.Errorf("upload: %w", err)
	}
	size := proto.Uint64(uint64(len(data)))
	msg := &waE2E.Message{}
	switch kind {
	case "image":
		msg.ImageMessage = &waE2E.ImageMessage{Caption: optional(caption), Mimetype: &mimeType,
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size}
	case "video":
		msg.VideoMessage = &waE2E.VideoMessage{Caption: optional(caption), Mimetype: &mimeType,
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size}
	case "audio":
		msg.AudioMessage = &waE2E.AudioMessage{Mimetype: &mimeType,
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size}
	default:
		msg.DocumentMessage = &waE2E.DocumentMessage{Caption: optional(caption), Mimetype: &mimeType,
			FileName: &name, Title: &name,
			URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey,
			FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: size}
	}
	ct, _ := extractContent(msg)
	sent, err := c.send(ctx, to, msg, ct)
	if err == nil && kind == "audio" && caption != "" {
		// WhatsApp audio has no caption: send it as a follow-up text.
		if _, err := c.SendText(ctx, to, caption, ""); err != nil {
			return sent, fmt.Errorf("audio sent, but its caption failed: %w", err)
		}
	}
	return sent, err
}

func (c *Client) send(ctx context.Context, to types.JID, msg *waE2E.Message, ct content) (Sent, error) {
	if !c.Connected() {
		return Sent{}, errors.New("not connected to WhatsApp")
	}
	resp, err := c.wm.SendMessage(ctx, to, msg)
	if err != nil {
		return Sent{}, fmt.Errorf("send: %w", err)
	}
	// whatsmeow does not echo our own messages back: archive them now.
	err = c.store.SaveMessage(ctx, store.Message{
		ChatJID: to.String(), ID: resp.ID, SenderJID: c.OwnJID().String(), FromMe: true,
		Timestamp: resp.Timestamp, Text: ct.Text, MediaType: ct.MediaType,
		Filename: ct.Filename, MimeType: ct.MimeType, Raw: ct.Raw,
	})
	if err != nil {
		c.log.Warnf("message sent but not archived: %v", err)
	}
	return Sent{ID: resp.ID, ChatJID: to.String(), Timestamp: resp.Timestamp}, nil
}

// checkRecipient refuses phone numbers that are not on WhatsApp, which would
// otherwise fail silently.
func (c *Client) checkRecipient(ctx context.Context, to types.JID) error {
	if to.Server != types.DefaultUserServer {
		return nil
	}
	if _, err := c.store.GetChat(ctx, to.String()); err == nil {
		return nil // already talked to them
	}
	res, err := c.wm.IsOnWhatsApp(ctx, []string{"+" + to.User})
	if err != nil {
		return fmt.Errorf("check that +%s is on WhatsApp: %w", to.User, err)
	}
	if len(res) == 0 || !res[0].IsIn {
		return fmt.Errorf("+%s is not on WhatsApp", to.User)
	}
	return nil
}

// Download fetches the media of a stored message into dir and returns the file path.
func (c *Client) Download(ctx context.Context, chat types.JID, messageID, dir string) (string, store.Message, error) {
	m, err := c.store.GetMessage(ctx, chat.String(), messageID)
	if err != nil {
		return "", store.Message{}, fmt.Errorf("message %s: %w", messageID, err)
	}
	if m.Deleted {
		return "", m, errors.New("message was deleted")
	}
	if len(m.Raw) == 0 {
		return "", m, errors.New("message has no downloadable media")
	}
	media, err := downloadable(m.Raw)
	if err != nil {
		return "", m, err
	}
	data, err := c.wm.Download(ctx, media)
	if err != nil {
		return "", m, fmt.Errorf("download (the media may have expired from WhatsApp's servers): %w", err)
	}

	name := sanitizeFilename(m.Filename)
	if name == "" {
		name = sanitizeFilename(m.ID) + extensionFor(media.GetMimetype())
	} else {
		name = sanitizeFilename(m.ID) + "-" + name
	}
	target := filepath.Join(dir, sanitizeFilename(chat.User), name)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", m, fmt.Errorf("create media directory: %w", err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return "", m, fmt.Errorf("write media: %w", err)
	}
	return target, m, nil
}

// FilePolicy restricts which local files send_file may read. Messages the
// model reads are untrusted input: a prompt injection could ask it to send a
// sensitive file, so the session keys are always off limits.
type FilePolicy struct {
	Protected string // never readable (the data directory)
	Root      string // if set, only files under this directory are readable
}

// SetFilePolicy configures send_file restrictions.
func (c *Client) SetFilePolicy(p FilePolicy) { c.files = p }

func (p FilePolicy) check(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path must be absolute: %s", path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("file: %w", err)
	}
	if p.Protected != "" && within(resolved, p.Protected) {
		return "", errors.New("refusing to send files from the wamcp data directory (it contains your WhatsApp session keys)")
	}
	if p.Root != "" && !within(resolved, p.Root) {
		return "", fmt.Errorf("refusing to send %s: only files under %s may be sent (--send-root)", path, p.Root)
	}
	return resolved, nil
}

// within reports whether path is dir or inside it. It compares file identity
// (device and inode) rather than strings, so case-insensitive file systems
// (macOS, Windows), symlinks and alternate spellings cannot bypass it.
func within(path, dir string) bool {
	target, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for p := filepath.Clean(path); ; {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, target) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// Status summarises the connection and archive.
type Status struct {
	LoggedIn  bool
	Connected bool
	Account   string
	Chats     int
	Messages  int
}

// Status reports the current state.
func (c *Client) Status(ctx context.Context) (Status, error) {
	chats, msgs, err := c.store.Stats(ctx)
	if err != nil {
		return Status{}, err
	}
	s := Status{LoggedIn: c.LoggedIn(), Connected: c.Connected(), Chats: chats, Messages: msgs}
	if s.LoggedIn {
		s.Account = "+" + c.OwnJID().User
	}
	return s, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var unsafeFilename = regexp.MustCompile(`[^\w.\-]+`)

func sanitizeFilename(s string) string {
	s = filepath.Base(strings.TrimSpace(s))
	if s == "." || s == "/" || s == ".." {
		return ""
	}
	return strings.Trim(unsafeFilename.ReplaceAllString(s, "_"), "._")
}

func extensionFor(mimeType string) string {
	mimeType, _, _ = strings.Cut(mimeType, ";")
	switch mimeType {
	case "image/jpeg":
		return ".jpg"
	case "audio/ogg":
		return ".ogg"
	case "video/mp4":
		return ".mp4"
	case "image/webp":
		return ".webp"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
