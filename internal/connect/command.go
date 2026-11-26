package connect

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/srinivasgumdelli/murmur/internal/model"
	"github.com/srinivasgumdelli/murmur/skills/murmur-connect/hooks"
)

const usage = `Usage:
  murmur                                   Serve the Murmur bus
  murmur enroll --agent NAME [--url URL]     Create or reuse a host-side credential
  murmur connect --agent NAME --workspace ID [--claude-hooks] -- COMMAND [ARGS...]
  murmur mcp-headers --agent NAME           Supply the saved header to an MCP client
  murmur watch --agent NAME [--codex-thread ID]
`

var scopeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@:/-]{0,127}$`)

type options struct {
	url       string
	agent     string
	workspace string
	task      string
	dir       string
	thread    string
	hooks     bool
}

func Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		_, err := io.WriteString(os.Stdout, usage)
		return err
	}
	command := args[0]
	switch command {
	case "help", "-h", "--help":
		_, err := io.WriteString(os.Stdout, usage)
		return err
	case "connect", "enroll", "watch", "mcp-headers":
	default:
		return fmt.Errorf("unknown command %q (run murmur --help)", command)
	}

	o := options{}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.StringVar(&o.url, "url", defaultURL(), "Murmur base URL")
	flags.StringVar(&o.agent, "agent", os.Getenv("MURMUR_AGENT"), "registered agent name")
	flags.StringVar(&o.dir, "profile-dir", os.Getenv("MURMUR_PROFILE_DIR"), "private credential directory")
	if command == "connect" {
		flags.StringVar(&o.workspace, "workspace", os.Getenv("MURMUR_WORKSPACE"), "memory workspace ID")
		flags.StringVar(&o.task, "task", os.Getenv("MURMUR_TASK"), "optional memory task ID")
		flags.BoolVar(&o.hooks, "claude-hooks", false, "attach bundled Claude Code hooks for this invocation")
	}
	if command == "watch" || command == "connect" {
		flags.StringVar(&o.thread, "codex-thread", "", "exact Codex thread ID for new message notifications")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if !model.ValidAgentName(o.agent) {
		return errors.New("provide --agent with a name using letters, digits, dots, underscores, or hyphens")
	}
	if command == "connect" {
		if flags.NArg() == 0 {
			return errors.New("provide a command after --")
		}
		if !scopeID.MatchString(o.workspace) || (o.task != "" && !scopeID.MatchString(o.task)) {
			return errors.New("provide a valid --workspace and optional --task")
		}
		if o.hooks && clientName(flags.Arg(0)) != "claude" {
			return errors.New("--claude-hooks requires a Claude Code command")
		}
	} else if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if o.thread != "" {
		if _, err := exec.LookPath("codex"); err != nil {
			return errors.New("--codex-thread requires codex on PATH")
		}
	}

	if o.url == "" {
		return errors.New("provide --url or MURMUR_URL in a sandbox (for example, http://murmur:4444 on the Compose network)")
	}
	c, err := newClient(o.url)
	if err != nil {
		return err
	}
	// The helper may be invoked by a project MCP config. Return a header
	// only for the origin and endpoint the operator configured.
	if endpoint := os.Getenv("CLAUDE_CODE_MCP_SERVER_URL"); command == "mcp-headers" &&
		endpoint != "" && strings.TrimRight(endpoint, "/") != c.base+"/mcp" {
		return errors.New("Murmur helper refused to provide a credential for a different MCP endpoint")
	}
	var key string
	if command == "enroll" && os.Getenv("MURMUR_KEY") != "" {
		err = importCredential(ctx, c, o, os.Getenv("MURMUR_KEY"))
	} else {
		key, err = loadCredential(ctx, c, o, command == "connect" || command == "enroll")
	}
	if err != nil {
		return err
	}

	switch command {
	case "enroll":
		_, err := fmt.Fprintf(os.Stdout, "Agent %s is enrolled at %s.\n", o.agent, c.base)
		return err
	case "mcp-headers":
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"Authorization": "Bearer " + key})
	case "watch":
		return watch(ctx, c, o, key, os.Stdout)
	default:
		return launch(ctx, c, o, key, flags.Args())
	}
}

func defaultURL() string {
	if configured := os.Getenv("MURMUR_URL"); configured != "" {
		return configured
	}
	if os.Getenv("MOAT_SANDBOX") != "" || os.Getenv("DEVCONTAINER") != "" {
		return ""
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return ""
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return ""
	}
	return "http://localhost:4444"
}

func clientName(program string) string {
	return strings.TrimSuffix(filepath.Base(program), ".exe")
}

func childArgs(argv []string, endpoint string, settings string) []string {
	switch clientName(argv[0]) {
	case "claude":
		config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			"murmur": map[string]any{"type": "http", "url": endpoint,
				"headers": map[string]string{"X-Murmur-Key": "${MURMUR_KEY:-}"}},
		}})
		extra := []string{"--mcp-config", string(config)}
		if settings != "" {
			extra = append(extra, "--settings", settings)
		}
		return append(append([]string{}, argv[1:]...), extra...)
	case "codex":
		return append([]string{
			"-c", "mcp_servers.murmur.url=" + strconv.Quote(endpoint),
			"-c", `mcp_servers.murmur.bearer_token_env_var="MURMUR_KEY"`,
		}, argv[1:]...)
	default:
		return argv[1:]
	}
}

func childEnvironment(env []string, merged map[string]string) []string {
	result := make([]string, 0, len(env)+len(merged))
	for _, variable := range env {
		name, _, _ := strings.Cut(variable, "=")
		if _, replaced := merged[name]; !replaced && name != "MURMUR_ADMIN_KEY" {
			result = append(result, variable)
		}
	}
	for name, value := range merged {
		result = append(result, name+"="+value)
	}
	return result
}

func materializeHooks() (string, string, error) {
	for _, program := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(program); err != nil {
			return "", "", fmt.Errorf("--claude-hooks requires %s on PATH", program)
		}
	}
	dir, err := os.MkdirTemp("", "murmur-hooks-")
	if err != nil {
		return "", "", err
	}
	entries, err := hooks.Assets.ReadDir(".")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		data, err := hooks.Assets.ReadFile(name)
		if err == nil {
			if name == "hooks.json" {
				data = []byte(strings.ReplaceAll(string(data), "$CLAUDE_PROJECT_DIR/.claude/skills/murmur-connect/hooks", "$MURMUR_HOOK_DIR"))
			}
			err = os.WriteFile(filepath.Join(dir, name), data, 0600)
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", "", err
		}
	}
	return dir, filepath.Join(dir, "hooks.json"), nil
}

func launch(ctx context.Context, c *client, o options, key string, argv []string) error {
	env := map[string]string{
		"MURMUR_URL": c.base, "MURMUR_AGENT": o.agent, "MURMUR_KEY": key,
		"MURMUR_WORKSPACE": o.workspace, "MURMUR_TASK": o.task,
	}
	settings := ""
	if o.hooks {
		dir, path, err := materializeHooks()
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		env["MURMUR_HOOK_DIR"] = dir
		settings = path
	}
	cmd := exec.CommandContext(ctx, argv[0], childArgs(argv, c.base+"/mcp", settings)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Env = childEnvironment(os.Environ(), env)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// An MCP-only client still appears live in the operator dashboard.
	if err := c.heartbeat(ctx, o.agent, key); err != nil {
		var status *statusError
		if !errors.As(err, &status) || status.code != http.StatusNotFound {
			return fmt.Errorf("start agent presence: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Murmur has no presence record for %s. Register the agent to appear in the dashboard.\n", o.agent)
	}
	var output io.Writer
	if o.thread != "" {
		logfile, err := os.CreateTemp("", "murmur-inbox-*.jsonl")
		if err != nil {
			return err
		}
		defer logfile.Close()
		fmt.Fprintf(os.Stderr, "Murmur notification log: %s\n", logfile.Name())
		output = logfile
	}
	watchCtx, cancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if o.thread == "" {
			keepPresence(watchCtx, c, o.agent, key)
		} else if err := watch(watchCtx, c, o, key, output); err != nil && watchCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "Murmur watcher stopped: %v\n", err)
		}
	}()
	// The terminal delivers Ctrl+C to the foreground group, including the child.
	// Keep the launcher alive while the agent handles the interrupt.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	err := cmd.Run()
	cancel()
	<-finished
	if errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled {
		return nil
	}
	return err
}

func keepPresence(ctx context.Context, c *client, agent, key string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.heartbeat(ctx, agent, key); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "Murmur heartbeat: %v\n", err)
			}
		}
	}
}
