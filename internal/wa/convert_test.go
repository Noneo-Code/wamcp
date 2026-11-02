package wa

import (
	"encoding/binary"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestExtractContent(t *testing.T) {
	tests := []struct {
		name      string
		msg       *waE2E.Message
		wantOK    bool
		wantText  string
		wantMedia string
		wantRaw   bool
	}{
		{"nil", nil, false, "", "", false},
		{"plain text", &waE2E.Message{Conversation: proto.String("salut")}, true, "salut", "", false},
		{"extended text", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("lien https://x")}}, true, "lien https://x", "", false},
		{"image with caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("vacances")}}, true, "vacances", "image", true},
		{"image without caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, true, "[image]", "image", true},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("devis.pdf")}}, true, "[document] devis.pdf", "document", true},
		{"voice note", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)}}, true, "[voice message]", "audio", true},
		{"document with caption wrapper", &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String("a.pdf"), Caption: proto.String("voici")}},
		}}, true, "voici", "document", true},
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{
			DegreesLatitude: proto.Float64(43.6), DegreesLongitude: proto.Float64(3.88), Name: proto.String("Montpellier"),
		}}, true, "[location] 43.600000,3.880000 Montpellier", "", false},
		{"reaction is ignored", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: proto.String("👍")}}, false, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractContent(tt.msg)
			if ok != tt.wantOK || got.Text != tt.wantText || got.MediaType != tt.wantMedia || (len(got.Raw) > 0) != tt.wantRaw {
				t.Errorf("got ok=%v %+v", ok, got)
			}
		})
	}
}

func TestDownloadableRoundTrip(t *testing.T) {
	msg := &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{DirectPath: proto.String("/v/x"), Mimetype: proto.String("application/pdf")}},
	}}
	ct, _ := extractContent(msg)
	root, m, err := downloadable(ct.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.GetDirectPath() != "/v/x" || m.GetMimetype() != "application/pdf" {
		t.Errorf("got %v %v", m.GetDirectPath(), m.GetMimetype())
	}
	// A refreshed path must survive re-encoding the whole message.
	setDirectPath(m, "/v/new")
	raw, _ := proto.Marshal(root)
	if _, m2, _ := downloadable(raw); m2.GetDirectPath() != "/v/new" {
		t.Errorf("path after re-encode = %q", m2.GetDirectPath())
	}
	raw, _ = proto.Marshal(&waE2E.Message{Conversation: proto.String("x")})
	if _, _, err := downloadable(raw); err == nil {
		t.Error("expected error for text message")
	}
}

func TestParsePhone(t *testing.T) {
	ok := map[string]string{
		"+33 6 12 34 56 78": "33612345678",
		"0033612345678":     "33612345678",
		"33612345678":       "33612345678",
		"+1 (415) 555-0100": "14155550100",
	}
	for in, want := range ok {
		jid, err := ParsePhone(in)
		if err != nil || jid.User != want || jid.Server != "s.whatsapp.net" {
			t.Errorf("ParsePhone(%q) = %v, %v; want %s", in, jid, err, want)
		}
	}
	for _, in := range []string{"0612345678", "+33abc", "123", ""} {
		if _, err := ParsePhone(in); err == nil {
			t.Errorf("ParsePhone(%q) should fail", in)
		}
	}
}

func TestLooksLikePhone(t *testing.T) {
	for in, want := range map[string]bool{"+33 6 12": true, "0612345678": true, "Sylvain": false, "Team 2026": false, "": false} {
		if got := looksLikePhone(in); got != want {
			t.Errorf("looksLikePhone(%q) = %v", in, got)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"devis.pdf":         "devis.pdf",
		"../../etc/passwd":  "passwd",
		"facture mars?.pdf": "facture_mars_.pdf",
		"..":                "",
		".hidden":           "hidden",
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractContentExtraTypes(t *testing.T) {
	ptv := &waE2E.Message{PtvMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4")}}
	if c, ok := extractContent(ptv); !ok || c.MediaType != "video" || len(c.Raw) == 0 {
		t.Errorf("video note: %+v", c)
	} else if _, _, err := downloadable(c.Raw); err != nil {
		t.Errorf("video note should be downloadable: %v", err)
	}
	inv := &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: proto.String("Famille")}}
	if c, ok := extractContent(inv); !ok || c.Text != "[group invite] Famille" {
		t.Errorf("group invite: %+v", c)
	}
}

func TestQuotedID(t *testing.T) {
	reply := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("yes"), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ORIG")},
	}}
	if c, _ := extractContent(reply); c.QuotedID != "ORIG" {
		t.Errorf("text reply quoted id = %q", c.QuotedID)
	}
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("IMG")}}}
	if c, _ := extractContent(img); c.QuotedID != "IMG" {
		t.Errorf("image reply quoted id = %q", c.QuotedID)
	}
	if c, _ := extractContent(&waE2E.Message{Conversation: proto.String("hi")}); c.QuotedID != "" {
		t.Errorf("plain text has quoted id %q", c.QuotedID)
	}
}

func TestOpusDuration(t *testing.T) {
	page := func(granule uint64, payload string) []byte {
		b := make([]byte, 27)
		copy(b, "OggS")
		binary.LittleEndian.PutUint64(b[6:14], granule)
		return append(b, payload...)
	}
	ogg := append(page(0, "OpusHead"), page(48000*7+100, "audio")...)
	if s, ok := opusDuration(ogg); !ok || s != 8 {
		t.Errorf("duration = %d, %v; want 8 (rounded up)", s, ok)
	}
	if _, ok := opusDuration(append(page(0, "vorbis"), page(1, "x")...)); ok {
		t.Error("Ogg/Vorbis must not be taken for Opus")
	}
	if _, ok := opusDuration([]byte("ID3 mp3 data")); ok {
		t.Error("mp3 must not be taken for Opus")
	}
}
