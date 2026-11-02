package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Noneo-Code/wamcp/internal/wa"
)

func listTools(t *testing.T, opts Options) ([]string, map[string]*mcp.Tool) {
	t.Helper()
	ctx := context.Background()
	srv := New(nil, t.TempDir(), "test", opts)
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	sort.Strings(names)
	return names, byName
}

func TestReadOnlyHidesSendingTools(t *testing.T) {
	names, _ := listTools(t, Options{ReadOnly: true})
	want := "download_media,get_group_info,get_messages,list_chats,load_older_messages,search_contacts,search_messages,whatsapp_status"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("tools = %s\nwant    %s", got, want)
	}
}

func TestImageResult(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(img, []byte("\xff\xd8jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := imageResult(DownloadOut{Path: img}, img, "image/jpeg")
	if res == nil || len(res.Content) != 2 {
		t.Fatalf("image result = %+v", res)
	}
	if _, ok := res.Content[1].(*mcp.ImageContent); !ok {
		t.Errorf("second block is %T", res.Content[1])
	}
	if imageResult(DownloadOut{Path: img}, img, "application/pdf") != nil {
		t.Error("a PDF must not be returned as an image")
	}
}

func TestAllowedChatsFilter(t *testing.T) {
	s := &Server{allowed: []string{"1@s.whatsapp.net"}}
	got := s.filter([]wa.Candidate{{JID: "1@s.whatsapp.net"}, {JID: "2@s.whatsapp.net"}})
	if len(got) != 1 || got[0].JID != "1@s.whatsapp.net" {
		t.Errorf("filter = %+v", got)
	}
	if s.isAllowed("2@s.whatsapp.net") || !(&Server{}).isAllowed("2@s.whatsapp.net") {
		t.Error("isAllowed mismatch")
	}
}

func TestToolsAreExposed(t *testing.T) {
	names, byName := listTools(t, Options{})
	want := "delete_message,download_media,get_group_info,get_messages,list_chats,load_older_messages,search_contacts,search_messages,send_file,send_message,send_reaction,whatsapp_status"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("tools = %s\nwant    %s", got, want)
	}
	if a := byName["delete_message"].Annotations; a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Error("delete_message must be marked destructive")
	}

	for _, name := range []string{"send_message", "send_file", "send_reaction"} {
		if a := byName[name].Annotations; a == nil || a.ReadOnlyHint {
			t.Errorf("%s must not be marked read-only", name)
		}
	}
	if a := byName["list_chats"].Annotations; a == nil || !a.ReadOnlyHint {
		t.Error("list_chats should be read-only")
	}
	if byName["send_message"].OutputSchema == nil {
		t.Error("send_message should declare an output schema")
	}
}
