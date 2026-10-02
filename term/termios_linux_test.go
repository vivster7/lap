package term

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Q2: what line discipline does a fresh Linux PTY have? These are the kernel
// defaults (tty_std_termios) and what an interactive terminal also starts
// with; term deliberately leaves them alone.
func TestPTYTermiosDefaults(t *testing.T) {
	s := openTest(t, Spec{Mode: PTY})
	_, out, _ := s.ChildStdio()
	rc, _ := out.SyscallConn()
	var tio *unix.Termios
	var err error
	rc.Control(func(fd uintptr) { tio, err = unix.IoctlGetTermios(int(fd), unix.TCGETS) })
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{
		"OPOST":   tio.Oflag&unix.OPOST != 0,
		"ONLCR":   tio.Oflag&unix.ONLCR != 0,
		"ECHO":    tio.Lflag&unix.ECHO != 0,
		"ECHOCTL": tio.Lflag&unix.ECHOCTL != 0,
		"ICANON":  tio.Lflag&unix.ICANON != 0,
		"ISIG":    tio.Lflag&unix.ISIG != 0,
		"ICRNL":   tio.Iflag&unix.ICRNL != 0,
		"IXON":    tio.Iflag&unix.IXON != 0,
		"IUTF8":   tio.Iflag&unix.IUTF8 != 0,
	}
	t.Logf("fresh pty termios: %v", flags)
	for _, f := range []string{"OPOST", "ONLCR", "ECHO", "ICANON", "ISIG"} {
		if !flags[f] {
			t.Errorf("%s unexpectedly off", f)
		}
	}
	ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col != DefaultCols || ws.Row != DefaultRows {
		t.Fatalf("winsize %+v %v", ws, err)
	}
	s.CloseChildEnds()
}

// Q2: the child sees the same settings via its controlling terminal even
// with stdin = /dev/null.
func TestPTYChildSeesCookedTerminal(t *testing.T) {
	sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "sh", "-c", "stty -a < /dev/tty")
	if err != nil {
		t.Fatal(err, sum)
	}
	out := string(rawOf(t, c))
	for _, want := range []string{"rows 24; columns 80", " onlcr", " echo ", " icanon"} {
		if !strings.Contains(out, want) {
			t.Errorf("stty output lacks %q:\n%s", want, out)
		}
	}
}

// Q2/Q6: ECHO consequence. A program that sends a query without switching
// the terminal to raw mode gets the reply echoed into its output, exactly
// as in a real terminal; the record shows what happened.
func TestReplyEchoedInCookedMode(t *testing.T) {
	sum, err, c := run(t, Spec{Mode: PTY}, 5*time.Second, "sh", "-c", `printf 'q\033[6n'; sleep 0.3; printf done`)
	if err != nil {
		t.Fatal(err, sum)
	}
	out := string(rawOf(t, c))
	if !strings.Contains(out, "^[[1;2R") || sum.Replies != 1 {
		t.Fatalf("raw=%q replies=%d", out, sum.Replies)
	}
	var input []string
	for _, ev := range events(t, c) {
		if ev.T == "input" {
			input = append(input, ev.Data)
		}
	}
	if len(input) != 1 || input[0] != "\x1b[1;2R" {
		t.Fatalf("journaled input %q", input)
	}
	t.Logf("raw record: %q (reply echoed by the line discipline as ^[)", out)
}
