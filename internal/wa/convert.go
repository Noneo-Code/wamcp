package wa

import (
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
}

// extractContent turns a (already unwrapped) WhatsApp message into text.
// ok is false for messages that carry nothing worth storing (receipts,
// reactions, protocol messages, key distribution…).
func extractContent(m *waE2E.Message) (c content, ok bool) {
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
		return extractContent(m.GetDocumentWithCaptionMessage().GetMessage())
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

// downloadable returns the media sub-message of a stored raw message.
func downloadable(raw []byte) (mediaMessage, error) {
	m := &waE2E.Message{}
	if err := proto.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("decode stored message: %w", err)
	}
	if dwc := m.GetDocumentWithCaptionMessage().GetMessage(); dwc != nil {
		m = dwc
	}
	switch {
	case m.GetImageMessage() != nil:
		return m.GetImageMessage(), nil
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage(), nil
	case m.GetPtvMessage() != nil:
		return m.GetPtvMessage(), nil
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage(), nil
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage(), nil
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage(), nil
	}
	return nil, fmt.Errorf("message has no downloadable media")
}

// mediaMessage is satisfied by every waE2E media message type.
type mediaMessage interface {
	GetDirectPath() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
	GetMimetype() string
}
