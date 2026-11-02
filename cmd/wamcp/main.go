// Command wamcp is a Model Context Protocol server for a personal WhatsApp
// account, connected as a linked device.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/Noneo-Code/wamcp/internal/config"
	"github.com/Noneo-Code/wamcp/internal/mcpserver"
	"github.com/Noneo-Code/wamcp/internal/store"
	"github.com/Noneo-Code/wamcp/internal/wa"
)

// version is set at build time with -ldflags "-X main.version=…" (release
// binaries); for `go install …@vX.Y.Z` it falls back to the module version.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

const usage = `wamcp - WhatsApp MCP server (unofficial, linked-device client)

Usage:
  wamcp login  [--phone +33612345678]   Link your WhatsApp account (QR code or pairing code)
  wamcp serve  [--http 127.0.0.1:8765]  Run the MCP server (stdio by default)
               [--send-root DIR]        Only let send_file read files under DIR
               [--read-only]            Hide the tools that send, react or delete
               [--allow-chats A,B]      Only give access to these chats (JIDs, numbers or names)
  wamcp status                          Show the linked account and archive size
  wamcp token                           Print the bearer token used by --http
  wamcp logout [--purge]                Unlink the device (and delete the archive with --purge)
  wamcp version

Global flags (put them after the command):
  --data-dir DIR    State directory (default: $WAMCP_DATA_DIR or <user config dir>/wamcp)
  --log-level LVL   debug, info, warn or error

Run "wamcp <command> --help" for details.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wamcp:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "login":
		return cmdLogin(ctx, rest)
	case "serve":
		return cmdServe(ctx, rest)
	case "status":
		return cmdStatus(ctx, rest)
	case "token":
		return cmdToken(rest)
	case "logout":
		return cmdLogout(ctx, rest)
	case "version", "--version", "-v":
		fmt.Println("wamcp", version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// common holds the flags every command accepts.
type common struct {
	dataDir  string
	logLevel string
}

func newFlags(name, defaultLevel string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &common{}
	fs.StringVar(&c.dataDir, "data-dir", "", "state directory")
	fs.StringVar(&c.logLevel, "log-level", defaultLevel, "debug, info, warn or error")
	return fs, c
}

// env is everything a command needs once flags are parsed.
type env struct {
	paths  config.Paths
	log    waLog.Logger
	store  *store.Store
	client *wa.Client
	lock   *flock.Flock
}

// open prepares the data directory, takes the single-instance lock when
// exclusive is set, and loads the archive and session.
func open(ctx context.Context, c *common, exclusive bool) (*env, error) {
	paths, err := config.Resolve(c.dataDir)
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	log, err := newLogger(os.Stderr, c.logLevel)
	if err != nil {
		return nil, err
	}
	e := &env{paths: paths, log: log}
	if exclusive {
		e.lock = flock.New(paths.LockFile)
		ok, err := e.lock.TryLock()
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", paths.LockFile, err)
		}
		if !ok {
			return nil, errors.New("another wamcp process is already using this account " +
				"(WhatsApp allows one connection per linked device). Stop it, or run a single " +
				"`wamcp serve --http` and point every MCP client at it")
		}
	}
	if e.store, err = store.Open(paths.MessagesDB); err != nil {
		e.close()
		return nil, err
	}
	if e.client, err = wa.Open(ctx, paths.SessionDB, e.store, log); err != nil {
		e.close()
		return nil, err
	}
	return e, nil
}

func (e *env) close() {
	if e.client != nil {
		_ = e.client.Close()
	}
	if e.store != nil {
		_ = e.store.Close()
	}
	if e.lock != nil {
		_ = e.lock.Unlock()
	}
}

func cmdLogin(ctx context.Context, args []string) error {
	fs, c := newFlags("login", "warn")
	phone := fs.String("phone", "", "link with a pairing code sent for this number instead of a QR code")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(ctx, c, true)
	if err != nil {
		return err
	}
	defer e.close()

	if err := e.client.Login(ctx, *phone, os.Stdout); err != nil {
		return err
	}
	fmt.Printf("\nLinked to +%s. Receiving your recent history from the phone, keep WhatsApp open on it…\n", e.client.OwnJID().User)
	batches := e.client.WaitForHistorySync(ctx, 30*time.Second, 5*time.Minute)
	st, err := e.client.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Done: %d history batches, %d chats and %d messages archived in %s\n", batches, st.Chats, st.Messages, e.paths.Dir)
	fmt.Println("Next: add the server to your MCP client, e.g.  claude mcp add whatsapp -s user -- wamcp serve")
	return nil
}

func cmdServe(ctx context.Context, args []string) error {
	fs, c := newFlags("serve", "warn")
	addr := fs.String("http", "", "serve MCP over Streamable HTTP on this address (e.g. 127.0.0.1:8765) instead of stdio")
	allowRemote := fs.Bool("allow-remote", false, "allow --http on a non-loopback address (put it behind a TLS reverse proxy)")
	sendRoot := fs.String("send-root", "", "only allow send_file to read files under this directory")
	readOnly := fs.Bool("read-only", false, "hide every tool that sends, reacts or deletes")
	allowChats := fs.String("allow-chats", "", "comma-separated chats (JIDs, phone numbers or names) the tools may access; default all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr != "" && !mcpserver.IsLoopback(*addr) && !*allowRemote {
		return fmt.Errorf("refusing to listen on %s: it is reachable from other machines. Use 127.0.0.1, or pass --allow-remote behind TLS", *addr)
	}
	e, err := open(ctx, c, true)
	if err != nil {
		return err
	}
	defer e.close()
	policy := wa.FilePolicy{Protected: e.paths.Dir}
	if *sendRoot != "" {
		if policy.Root, err = filepath.Abs(*sendRoot); err != nil {
			return err
		}
	}
	e.client.SetFilePolicy(policy)
	opts := mcpserver.Options{ReadOnly: *readOnly}
	if *allowChats != "" {
		if opts.AllowedChats, err = resolveChats(ctx, e.client, *allowChats); err != nil {
			return err
		}
	}
	if err := e.client.Connect(); err != nil {
		return err
	}
	srv := mcpserver.New(e.client, e.paths.MediaDir, version, opts)

	if *addr == "" {
		return ignoreCanceled(srv.Run(ctx, &mcp.StdioTransport{}))
	}
	return serveHTTP(ctx, srv, *addr, *allowRemote, e)
}

// resolveChats turns the --allow-chats list into JIDs.
func resolveChats(ctx context.Context, c *wa.Client, list string) ([]string, error) {
	jids := []string{}
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item == "" {
			continue
		}
		jid, err := c.Resolve(ctx, item)
		if err != nil {
			return nil, fmt.Errorf("--allow-chats: %w", err)
		}
		jids = append(jids, jid.String())
	}
	return jids, nil
}

func serveHTTP(ctx context.Context, srv *mcp.Server, addr string, remote bool, e *env) error {
	token, err := resolveToken(e.paths)
	if err != nil {
		return err
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		// Clients that disappear without closing their session must not pile up.
		SessionTimeout: 30 * time.Minute,
		// The SDK rejects non-localhost Host headers on loopback listeners (DNS
		// rebinding protection); a reverse proxy legitimately sends its own host.
		DisableLocalhostProtection: remote,
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.RequireBearer(token, handler))
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	e.log.Infof("listening on http://%s/mcp", addr)
	fmt.Fprintf(os.Stderr, "wamcp: MCP endpoint http://%s/mcp (token: `wamcp token`)\n", addr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func cmdStatus(ctx context.Context, args []string) error {
	fs, c := newFlags("status", "error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(ctx, c, false)
	if err != nil {
		return err
	}
	defer e.close()
	st, err := e.client.Status(ctx)
	if err != nil {
		return err
	}
	account := "not linked (run `wamcp login`)"
	if st.LoggedIn {
		account = st.Account
	}
	running := "yes"
	probe := flock.New(e.paths.LockFile)
	if locked, _ := probe.TryLock(); locked {
		running = "no"
		_ = probe.Unlock()
	}
	w := os.Stdout
	fmt.Fprintf(w, "Account:        %s\nServer running: %s\nArchive:        %d chats, %d messages\nData directory: %s\n",
		account, running, st.Chats, st.Messages, e.paths.Dir)
	return nil
}

func cmdToken(args []string) error {
	fs, c := newFlags("token", "error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := config.Resolve(c.dataDir)
	if err != nil {
		return err
	}
	if err := paths.Ensure(); err != nil {
		return err
	}
	token, err := resolveToken(paths)
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

// resolveToken returns $WAMCP_TOKEN, or the token stored in the data directory.
func resolveToken(paths config.Paths) (string, error) {
	if t := os.Getenv("WAMCP_TOKEN"); t != "" {
		return t, nil
	}
	return mcpserver.LoadOrCreateToken(paths.TokenFile)
}

func cmdLogout(ctx context.Context, args []string) error {
	fs, c := newFlags("logout", "warn")
	purge := fs.Bool("purge", false, "also delete the message archive, downloaded media and token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	e, err := open(ctx, c, true)
	if err != nil {
		return err
	}
	if err := e.client.Logout(ctx); err != nil && !errors.Is(err, wa.ErrNotLoggedIn) {
		e.close()
		return err
	}
	e.close()
	fmt.Println("Device unlinked.")
	if *purge {
		for _, p := range []string{e.paths.MessagesDB, e.paths.MessagesDB + "-wal", e.paths.MessagesDB + "-shm", e.paths.TokenFile} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.RemoveAll(e.paths.MediaDir); err != nil {
			return err
		}
		fmt.Println("Archive, media and token deleted.")
	}
	return nil
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
