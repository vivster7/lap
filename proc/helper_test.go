package proc

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The test binary doubles as the workload. With LAP_PROC_HELPER=1 it runs
// the plan given in its arguments instead of the tests. Plan tokens:
//
//	report=NAME   write "NAME <pid>\n" to stdout (one write, atomic on pipes)
//	ids           write "ids <pid> <pgid> <sid>\n"
//	ignore=SIG    ignore TERM or HUP (inherited across exec)
//	setsid        call setsid(2)            (escape group and session)
//	setpgid       call setpgid(0,0)         (escape group, stay in session)
//	closeio       point fds 0,1,2 at /dev/null (daemon-like)
//	stop          SIGSTOP self
//	spawn { ... } start a child running the nested plan; do not wait
//	wait          wait for children spawned so far
//	sleepms=N     sleep N ms
//	sleep         sleep 30s then exit 0 (tests always kill sooner)
//	exit[=N]      exit now
//	runner { ... } (Linux) proc.Start the nested plan with Pdeathsig=SIGKILL,
//	              report "runner <pid>", then sleep
//	subreaper { ... } (Linux) see TestProbeSubreaperKeepsLineage
const helperEnv = "LAP_PROC_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runPlan(os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helperArgv(plan ...string) []string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return append([]string{exe}, plan...)
}

// GORACE=atexit_sleep_ms=0: race-instrumented helpers otherwise sleep 1s in
// exit, which distorts every "child exits" timing under -race.
func helperEnvList() []string {
	return append(os.Environ(), helperEnv+"=1", "GORACE=atexit_sleep_ms=0")
}

func helperSpec(plan ...string) Spec {
	return Spec{Argv: helperArgv(plan...), Env: helperEnvList()}
}

// block splits "{ ... } rest" into the nested plan and the remainder.
func block(plan []string) (inner, rest []string) {
	if len(plan) == 0 || plan[0] != "{" {
		panic(fmt.Sprintf("helper: expected { in %q", plan))
	}
	depth := 0
	for i, tok := range plan {
		switch tok {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return plan[1:i], plan[i+1:]
			}
		}
	}
	panic("helper: unbalanced braces")
}

func say(format string, a ...any) {
	os.Stdout.Write([]byte(fmt.Sprintf(format, a...) + "\n"))
}

func runPlan(plan []string) {
	var kids []*os.Process
	for len(plan) > 0 {
		tok := plan[0]
		plan = plan[1:]
		name, arg, _ := strings.Cut(tok, "=")
		switch name {
		case "report":
			say("%s %d", arg, os.Getpid())
		case "ids":
			pgid, _ := syscall.Getpgid(0)
			sid, _ := unix.Getsid(0)
			say("ids %d %d %d", os.Getpid(), pgid, sid)
		case "ignore":
			switch arg {
			case "TERM":
				signal.Ignore(syscall.SIGTERM)
			case "HUP":
				signal.Ignore(syscall.SIGHUP)
			}
		case "setsid":
			if _, err := syscall.Setsid(); err != nil {
				say("setsid-failed %v", err)
			}
		case "setpgid":
			if err := syscall.Setpgid(0, 0); err != nil {
				say("setpgid-failed %v", err)
			}
		case "closeio":
			null, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			for fd := 0; fd <= 2; fd++ {
				unix.Dup2(int(null.Fd()), fd)
			}
		case "stop":
			syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		case "spawn":
			var inner []string
			inner, plan = block(plan)
			p, err := os.StartProcess(helperArgv()[0], helperArgv(inner...), &os.ProcAttr{
				Env:   os.Environ(),
				Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
			})
			if err != nil {
				say("spawn-failed %v", err)
				os.Exit(3)
			}
			kids = append(kids, p)
		case "wait":
			for _, k := range kids {
				k.Wait()
			}
			kids = nil
		case "sleepms":
			n, _ := strconv.Atoi(arg)
			time.Sleep(time.Duration(n) * time.Millisecond)
		case "sleep":
			time.Sleep(30 * time.Second)
			os.Exit(0)
		case "exit":
			n, _ := strconv.Atoi(arg)
			os.Exit(n)
		case "runner", "runnerpty", "subreaper":
			var inner []string
			inner, plan = block(plan)
			platformHelper(name, inner)
		default:
			say("unknown-token %s", tok)
			os.Exit(2)
		}
	}
}
