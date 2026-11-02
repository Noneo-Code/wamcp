package wa

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// content is the searchable, displayable part of a message.
type content struct {
	Text      string
	MediaType string // image, video, audio, document, sticker
	Filename  string
	MimeType  string
	Raw       []byte // set for media so it can be downloaded later
	QuotedID  string // id of the message this one replies to
}

// extractContent turns a (already unwrapped) WhatsApp message into text.
// ok is false for messages that carry nothing worth storing (receipts,
// reactions, protocol messages, key distribution…).
func extractContent(m *waE2E.Message) (c content, ok bool) {
	c, ok = extractBody(m)
	if ok {
		c.QuotedID = contextInfo(m).GetStanzaID()
	}
	return c, ok
}

func extractBody(m *waE2E.Message) (c content, ok bool) {
	if m == nil {
		return content{}, false
	}
	switch {
	case m.GetConversation() != "":
		return content{Text: m.GetConversation()}, true
	case m.GetExtendedTextMessage() != nil:
		return content{Text: m.GetExtendedTextMessage().GetText()}, true
	case m.GetImageMessage() != nil:
		im := m.GetImageMessage()
		return media(m, "image", im.GetCaption(), "", im.GetMimetype()), true
	case m.GetVideoMessage() != nil:
		vm := m.GetVideoMessage()
		return media(m, "video", vm.GetCaption(), "", vm.GetMimetype()), true
	case m.GetPtvMessage() != nil: // round "video note"
		return media(m, "video", "[video note]", "", m.GetPtvMessage().GetMimetype()), true
	case m.GetAudioMessage() != nil:
		am := m.GetAudioMessage()
		label := "[audio]"
		if am.GetPTT() {
			label = "[voice message]"
		}
		return media(m, "audio", label, "", am.GetMimetype()), true
	case m.GetDocumentMessage() != nil:
		dm := m.GetDocumentMessage()
		return media(m, "document", dm.GetCaption(), dm.GetFileName(), dm.GetMimetype()), true
	case m.GetDocumentWithCaptionMessage().GetMessage().GetDocumentMessage() != nil:
		return extractBody(m.GetDocumentWithCaptionMessage().GetMessage())
	case m.GetStickerMessage() != nil:
		return media(m, "sticker", "[sticker]", "", m.GetStickerMessage().GetMimetype()), true
	case m.GetLocationMessage() != nil:
		lm := m.GetLocationMessage()
		text := fmt.Sprintf("[location] %.6f,%.6f", lm.GetDegreesLatitude(), lm.GetDegreesLongitude())
		if name := strings.TrimSpace(lm.GetName() + " " + lm.GetAddress()); name != "" {
			text += " " + name
		}
		return content{Text: text}, true
	case m.GetLiveLocationMessage() != nil:
		return content{Text: "[live location]"}, true
	case m.GetContactMessage() != nil:
		return content{Text: "[contact] " + m.GetContactMessage().GetDisplayName()}, true
	case m.GetContactsArrayMessage() != nil:
		return content{Text: "[contacts] " + m.GetContactsArrayMessage().GetDisplayName()}, true
	case m.GetPollCreationMessage() != nil:
		return poll(m.GetPollCreationMessage()), true
	case m.GetPollCreationMessageV3() != nil:
		return poll(m.GetPollCreationMessageV3()), true
	case m.GetGroupInviteMessage() != nil:
		return content{Text: "[group invite] " + m.GetGroupInviteMessage().GetGroupName()}, true
	case m.GetEventMessage() != nil:
		return content{Text: "[event] " + m.GetEventMessage().GetName()}, true
	case m.GetListMessage() != nil:
		return content{Text: strings.TrimSpace("[list] " + m.GetListMessage().GetDescription())}, true
	case m.GetButtonsMessage() != nil:
		return content{Text: strings.TrimSpace("[buttons] " + m.GetButtonsMessage().GetContentText())}, true
	case m.GetInteractiveMessage() != nil:
		return content{Text: strings.TrimSpace("[interactive] " + m.GetInteractiveMessage().GetBody().GetText())}, true
	case m.GetTemplateMessage() != nil:
		return content{Text: "[template message]"}, true
	}
	return content{}, false
}

// contextInfo returns the reply/mention metadata of a message, whatever its type.
func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	if dwc := m.GetDocumentWithCaptionMessage().GetMessage(); dwc != nil {
		m = dwc
	}
	for _, ci := range []*waE2E.ContextInfo{
		m.GetExtendedTextMessage().GetContextInfo(),
		m.GetImageMessage().GetContextInfo(),
		m.GetVideoMessage().GetContextInfo(),
		m.GetPtvMessage().GetContextInfo(),
		m.GetAudioMessage().GetContextInfo(),
		m.GetDocumentMessage().GetContextInfo(),
		m.GetStickerMessage().GetContextInfo(),
		m.GetLocationMessage().GetContextInfo(),
		m.GetContactMessage().GetContextInfo(),
		m.GetPollCreationMessage().GetContextInfo(),
		m.GetPollCreationMessageV3().GetContextInfo(),
	} {
		if ci != nil {
			return ci
		}
	}
	return nil
}

func media(m *waE2E.Message, kind, text, filename, mime string) content {
	raw, err := proto.Marshal(m)
	if err != nil {
		raw = nil // the message stays readable, only the download will be unavailable
	}
	if text == "" && filename != "" {
		text = "[" + kind + "] " + filename
	} else if text == "" {
		text = "[" + kind + "]"
	}
	return content{Text: text, MediaType: kind, Filename: filename, MimeType: mime, Raw: raw}
}

func poll(p *waE2E.PollCreationMessage) content {
	opts := make([]string, 0, len(p.GetOptions()))
	for _, o := range p.GetOptions() {
		opts = append(opts, o.GetOptionName())
	}
	return content{Text: fmt.Sprintf("[poll] %s (%s)", p.GetName(), strings.Join(opts, " / "))}
}

// downloadable decodes a stored raw message and returns it with its media
// sub-message (which points into root, so changes to it are kept on re-marshal).
func downloadable(raw []byte) (root *waE2E.Message, media mediaMessage, err error) {
	root = &waE2E.Message{}
	if err := proto.Unmarshal(raw, root); err != nil {
		return nil, nil, fmt.Errorf("decode stored message: %w", err)
	}
	m := root
	if dwc := m.GetDocumentWithCaptionMessage().GetMessage(); dwc != nil {
		m = dwc
	}
	switch {
	case m.GetImageMessage() != nil:
		return root, m.GetImageMessage(), nil
	case m.GetVideoMessage() != nil:
		return root, m.GetVideoMessage(), nil
	case m.GetPtvMessage() != nil:
		return root, m.GetPtvMessage(), nil
	case m.GetAudioMessage() != nil:
		return root, m.GetAudioMessage(), nil
	case m.GetDocumentMessage() != nil:
		return root, m.GetDocumentMessage(), nil
	case m.GetStickerMessage() != nil:
		return root, m.GetStickerMessage(), nil
	}
	return nil, nil, fmt.Errorf("message has no downloadable media")
}

// setDirectPath points a media message at a new location on WhatsApp's servers.
func setDirectPath(m mediaMessage, path string) {
	switch v := m.(type) {
	case *waE2E.ImageMessage:
		v.DirectPath = &path
	case *waE2E.VideoMessage:
		v.DirectPath = &path
	case *waE2E.AudioMessage:
		v.DirectPath = &path
	case *waE2E.DocumentMessage:
		v.DirectPath = &path
	case *waE2E.StickerMessage:
		v.DirectPath = &path
	}
}

// mediaMessage is satisfied by every waE2E media message type.
type mediaMessage interface {
	GetDirectPath() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
	GetMimetype() string
}

// opusDuration returns the length in seconds of an Ogg/Opus file, read from
// the granule position of its last page (Opus always runs at 48 kHz). ok is
// false for anything that is not Ogg/Opus.
func opusDuration(data []byte) (seconds uint32, ok bool) {
	if !bytes.HasPrefix(data, []byte("OggS")) || !bytes.Contains(data[:min(len(data), 512)], []byte("OpusHead")) {
		return 0, false
	}
	last := bytes.LastIndex(data, []byte("OggS"))
	if last < 0 || len(data) < last+14 {
		return 0, false
	}
	granule := binary.LittleEndian.Uint64(data[last+6 : last+14])
	if granule == 0 || granule == ^uint64(0) {
		return 0, true
	}
	return uint32(min((granule+47999)/48000, 1<<31)), true //nolint:gosec // bounded above
}
