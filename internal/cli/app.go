package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

type App struct {
	Config  ConfigHandler
	Project ProjectHandler
	Runtime RuntimeHandler
	Out     io.Writer
	Err     io.Writer
}

var signalNotifyContext = signal.NotifyContext

type ConfigHandler interface {
	Init(context.Context, []string) error
	Show(context.Context, []string) error
	Validate(context.Context, []string) error
	SetBaseWorkspace(context.Context, []string) error
	Login(context.Context, []string) error
	Status(context.Context, []string) error
	Logout(context.Context, []string) error
}

type ProjectHandler interface {
	ImportRemote(context.Context, []string) error
	ImportLocal(context.Context, []string) error
	List(context.Context, []string) error
	Show(context.Context, []string) error
	Delete(context.Context, []string) error
}

type RuntimeHandler interface {
	Start(context.Context, []string) error
	Status(context.Context, []string) error
	Reload(context.Context, []string) error
	Doctor(context.Context, []string) error
	Cancel(context.Context, []string) error
}

func New() *App {
	app := &App{
		Out: os.Stdout,
		Err: os.Stderr,
	}
	app.Config = newConfigCommandHandler(app.out(), os.Stdin)
	app.Project = newProjectCommandHandler(app.out())
	app.Runtime = newRuntimeCommandHandler(app.out())
	return app
}

func (a *App) Run(args []string) int {
	if a == nil {
		return 1
	}

	out := a.out()
	errOut := a.err()

	if len(args) == 0 {
		printRootHelp(out)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		printRootHelp(out)
		return 0
	case "config":
		return a.runConfig(args[1:])
	case "project":
		return a.runProject(args[1:])
	case "runtime":
		return a.runRuntime(args[1:])
	default:
		fmt.Fprintf(errOut, "unknown command %q\n\n", args[0])
		printRootHelp(out)
		return 1
	}
}

func (a *App) runConfig(args []string) int {
	out := a.out()
	handler := a.configHandler()

	if len(args) == 0 {
		printConfigHelp(out)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		printConfigHelp(out)
		return 0
	case "init":
		return runCommand(handler.Init, out, "config init", args[1:])
	case "show":
		return runCommand(handler.Show, out, "config show", args[1:])
	case "validate":
		return runCommand(handler.Validate, out, "config validate", args[1:])
	case "set-base-workspace":
		return runCommand(handler.SetBaseWorkspace, out, "config set-base-workspace", args[1:])
	case "slack-auth":
		return a.runConfigSlackAuth(args[1:])
	default:
		return unknownSubcommand(out, "config", args[0], printConfigHelp)
	}
}

func (a *App) runConfigSlackAuth(args []string) int {
	out := a.out()
	handler := a.configHandler()

	if len(args) == 0 {
		printSlackAuthHelp(out)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		printSlackAuthHelp(out)
		return 0
	case "login":
		return runCommand(handler.Login, out, "config slack-auth login", args[1:])
	case "status":
		return runCommand(handler.Status, out, "config slack-auth status", args[1:])
	case "logout":
		return runCommand(handler.Logout, out, "config slack-auth logout", args[1:])
	default:
		return unknownSubcommand(out, "config slack-auth", args[0], printSlackAuthHelp)
	}
}

func (a *App) runProject(args []string) int {
	out := a.out()
	handler := a.projectHandler()

	if len(args) == 0 {
		printProjectHelp(out)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		printProjectHelp(out)
		return 0
	case "import-remote":
		return runCommand(handler.ImportRemote, out, "project import-remote", args[1:])
	case "import-local":
		return runCommand(handler.ImportLocal, out, "project import-local", args[1:])
	case "list":
		return runCommand(handler.List, out, "project list", args[1:])
	case "show":
		return runCommand(handler.Show, out, "project show", args[1:])
	case "delete":
		return runCommand(handler.Delete, out, "project delete", args[1:])
	default:
		return unknownSubcommand(out, "project", args[0], printProjectHelp)
	}
}

func (a *App) runRuntime(args []string) int {
	out := a.out()
	handler := a.runtimeHandler()

	if len(args) == 0 {
		return runCommand(handler.Status, out, "runtime", args)
	}

	switch args[0] {
	case "-h", "--help", "help":
		printRuntimeHelp(out)
		return 0
	case "start":
		ctx, stop := signalNotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := handler.Start(ctx, args[1:]); err != nil {
			if errors.Is(err, context.Canceled) {
				return 0
			}
			fmt.Fprintln(out, err)
			return 1
		}
		return 0
	case "status":
		return runCommand(handler.Status, out, "runtime status", args[1:])
	case "reload":
		return runCommand(handler.Reload, out, "runtime reload", args[1:])
	case "doctor":
		return runCommand(handler.Doctor, out, "runtime doctor", args[1:])
	case "cancel":
		return runCommand(handler.Cancel, out, "runtime cancel", args[1:])
	default:
		return unknownSubcommand(out, "runtime", args[0], printRuntimeHelp)
	}
}

func (a *App) configHandler() ConfigHandler {
	if a.Config != nil {
		return a.Config
	}
	return noopConfigHandler{out: a.out()}
}

func (a *App) projectHandler() ProjectHandler {
	if a.Project != nil {
		return a.Project
	}
	return noopProjectHandler{out: a.out()}
}

func (a *App) runtimeHandler() RuntimeHandler {
	if a.Runtime != nil {
		return a.Runtime
	}
	return noopRuntimeHandler{out: a.out()}
}

func (a *App) out() io.Writer {
	if a != nil && a.Out != nil {
		return a.Out
	}
	return os.Stdout
}

func (a *App) err() io.Writer {
	if a != nil && a.Err != nil {
		return a.Err
	}
	return os.Stderr
}

type commandRunner func(context.Context, []string) error

func runCommand(fn commandRunner, out io.Writer, label string, args []string) int {
	if fn == nil {
		if _, err := fmt.Fprintf(out, "%s is scaffolded and ready for follow-up implementation.\n", label); err != nil {
			return 1
		}
		return 0
	}

	if err := fn(context.Background(), args); err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	return 0
}

func unknownSubcommand(out io.Writer, group, subcommand string, help func(io.Writer)) int {
	fmt.Fprintf(out, "unknown %s command %q\n\n", group, subcommand)
	help(out)
	return 1
}

func printRootHelp(out io.Writer) {
	fmt.Fprintln(out, "spexus-agent")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  spexus-agent <group> <command> [args]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Groups:")
	fmt.Fprintln(out, "  config   global config and Slack auth")
	fmt.Fprintln(out, "  project  project registry operations")
	fmt.Fprintln(out, "  runtime  runtime orchestration commands")
}

func printConfigHelp(out io.Writer) {
	fmt.Fprintln(out, "spexus-agent config")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  init")
	fmt.Fprintln(out, "  show")
	fmt.Fprintln(out, "  validate")
	fmt.Fprintln(out, "  set-base-workspace")
	fmt.Fprintln(out, "  slack-auth")
}

func printSlackAuthHelp(out io.Writer) {
	fmt.Fprintln(out, "spexus-agent config slack-auth")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  login")
	fmt.Fprintln(out, "  status")
	fmt.Fprintln(out, "  logout")
}

func printProjectHelp(out io.Writer) {
	fmt.Fprintln(out, "spexus-agent project")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  import-remote")
	fmt.Fprintln(out, "  import-local")
	fmt.Fprintln(out, "  list")
	fmt.Fprintln(out, "  show")
	fmt.Fprintln(out, "  delete")
}

func printRuntimeHelp(out io.Writer) {
	fmt.Fprintln(out, "spexus-agent runtime")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "No subcommand runs the runtime foundation status check.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Commands:")
	fmt.Fprintln(out, "  start [--debug]")
	fmt.Fprintln(out, "  status [execution-id]")
	fmt.Fprintln(out, "  reload")
	fmt.Fprintln(out, "  doctor")
	fmt.Fprintln(out, "  cancel <execution-id|thread-ts>")
}
