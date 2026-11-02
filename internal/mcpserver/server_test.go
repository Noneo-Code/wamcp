package mcpserver

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolsAreExposed(t *testing.T) {
	ctx := context.Background()
	srv := New(nil, t.TempDir(), "test")
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

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
	want := "download_media,get_messages,list_chats,search_contacts,search_messages,send_file,send_message,whatsapp_status"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("tools = %s\nwant    %s", got, want)
	}

	for _, name := range []string{"send_message", "send_file"} {
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
