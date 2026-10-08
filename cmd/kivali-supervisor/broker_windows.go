//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/eventlog"

	"github.com/kivali-ai/kivali/internal/supervisor/broker"
)

const brokerUsage = `usage: kivali-supervisor broker [--console | install | uninstall | status | setup]

  (no subcommand)   run as a Windows service (started by the service manager)
  --console         run in the foreground, in an elevated terminal
  install           register the service under its own account and start it; also the
                    Hyper-V Administrators membership, the log directory under
                    ProgramData\Kivali, the event source and the guest agent's
                    Hyper-V socket registration (run as administrator)
  uninstall         remove all of that (run as administrator)
  status            ask the running broker whether Hyper-V, its Default Switch and
                    the guest agent's vsock service are in place (exit 1 if not)
  setup             have the running broker register what it can (the vsock
                    service), then report as status does

status and setup run as you: the broker does the privileged part. Hyper-V
itself is enabled by an administrator, and needs a reboot.
`

// broker runs or manages the Hyper-V broker (docs/developers/supervisor.md,
// "The broker"). It serves HTTP/1.1 on the broker's named pipe as a
// Windows service, or in the foreground with --console; install and
// uninstall register and remove the service; status and setup ask the
// running broker about, and for, what it needs (brokerSetup).
func (c *cli) broker(args []string) int {
	fs := flag.NewFlagSet("broker", flag.ExitOnError)
	console := fs.Bool("console", false, "run in the foreground instead of as a service")
	fs.Usage = func() { fmt.Fprint(os.Stderr, brokerUsage) }
	_ = fs.Parse(args)
	switch rest := fs.Args(); {
	case len(rest) == 1 && rest[0] == "install":
		exe, err := os.Executable()
		if err != nil {
			return fail(err)
		}
		if err := broker.Install(exe); err != nil {
			return fail(err)
		}
		fmt.Printf("installed the %s service\n", broker.ServiceName)
		return 0
	case len(rest) == 1 && rest[0] == "uninstall":
		left, err := broker.Uninstall()
		if err != nil {
			return fail(err)
		}
		fmt.Printf("removed the %s service\n", broker.ServiceName)
		if len(left) > 0 {
			dir, _ := broker.LogDir()
			fmt.Printf("left %s: the VMs of %d team(s) are still registered in Hyper-V (%s); destroy those teams first, or remove each VM in an administrator's PowerShell (Remove-VM kivali-<key> -Force) and delete the directory\n",
				dir, len(left), strings.Join(left, ", "))
		}
		return 0
	case len(rest) == 1 && (rest[0] == "status" || rest[0] == "setup"):
		return brokerSetup(rest[0] == "setup")
	case len(rest) > 0:
		fmt.Fprint(os.Stderr, brokerUsage)
		return 2
	}

	// The console broker logs to its terminal; the service, which has
	// none, to its own file under ProgramData (made at install), and to
	// the event log only when that file cannot be opened.
	logger := log.New(os.Stderr, "", log.LstdFlags)
	service := !*console && broker.IsService()
	if service {
		if f, err := broker.OpenLog(); err == nil {
			defer func() { _ = f.Close() }()
			logger = log.New(f, "", log.LstdFlags|log.LUTC)
		} else if el, eerr := eventlog.Open(broker.ServiceName); eerr == nil {
			defer func() { _ = el.Close() }()
			_ = el.Warning(1, fmt.Sprintf("the broker cannot open its log (%v); logging here", err))
			logger = log.New(eventLogWriter{el}, "", 0)
		}
	}
	srv, err := broker.NewServer(logger.Printf)
	if err != nil {
		return fail(err)
	}
	ln, err := broker.Listen(os.Getenv("KIVALI_BROKER_PIPE"))
	if err != nil {
		logger.Printf("kivali-broker: %v", err)
		return fail(err)
	}
	if !service {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		logger.Printf("kivali-broker serving on the broker pipe (console); Ctrl+C to stop")
		if err := srv.RunConsole(ctx, ln); err != nil {
			return fail(err)
		}
		return 0
	}
	logger.Printf("kivali-broker %s starting as the service (%s)", version, processAccount())
	if err := srv.RunService(ln); err != nil {
		logger.Printf("kivali-broker: %v", err)
		return fail(err)
	}
	logger.Printf("kivali-broker stopped")
	return 0
}

// processAccount names this process's account, for the service's first
// log line: it should be the broker's own virtual account.
func processAccount() string {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "unknown account"
	}
	name, domain, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return u.User.Sid.String()
	}
	return domain + `\` + name + " (" + u.User.Sid.String() + ")"
}

// eventLogWriter writes each log line as an informational event.
type eventLogWriter struct{ el *eventlog.Log }

func (w eventLogWriter) Write(p []byte) (int, error) {
	return len(p), w.el.Info(1, strings.TrimRight(string(p), "\r\n"))
}

// brokerSetup is broker status (GET /v1/setup) and, with apply, broker
// setup (POST /v1/setup, which registers what the broker can and then
// answers the same). It runs as the person, through broker.Dial like the
// backend, prints the three facts and exits 0 when everything is in
// place (Setup.Ready), 1 otherwise or when the broker cannot be asked.
// The bound covers setup's two scripts, a minute each at most.
func brokerSetup(apply bool) int {
	client := &broker.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		return broker.Dial(ctx, brokerPipe())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ask := client.Setup
	if apply {
		ask = client.ApplySetup
	}
	st, err := ask(ctx)
	if err != nil {
		return fail(err)
	}
	fmt.Print(setupReport(st))
	if !st.Ready() {
		return 1
	}
	return 0
}

// setupReport is the three facts of a broker's setup, one per line.
func setupReport(st broker.Setup) string {
	yes := func(b bool, y, n string) string {
		if b {
			return y
		}
		return n
	}
	return "hyperv: " + yes(st.HyperV, "yes", "no") + "\n" +
		"default switch: " + yes(st.DefaultSwitch, "yes", "no") + "\n" +
		"vsock service: " + yes(st.VsockService, "registered", "not registered") + "\n"
}
