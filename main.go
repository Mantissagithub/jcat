package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/term"
)

type options struct {
	input       string
	paging      string
	color       string
	plain       bool
	interactive bool
}

func parseArgs(args []string) (options, bool, error) {
	fs := flag.NewFlagSet("jcat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o options
	var help bool
	fs.StringVar(&o.paging, "paging", "auto", "auto, always, or never")
	fs.StringVar(&o.color, "color", "auto", "auto, always, or never")
	fs.BoolVar(&o.plain, "plain", false, "use the formatter")
	fs.BoolVar(&o.interactive, "interactive", false, "open the terminal inspector")
	fs.BoolVar(&help, "help", false, "show help")
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" {
			help = true
			continue
		}
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "--") {
			flagArgs = append(flagArgs, arg)
			if arg == "--paging" || arg == "--color" {
				if i+1 >= len(args) {
					return o, false, fmt.Errorf("%s requires a value", arg)
				}
				i++
				flagArgs = append(flagArgs, args[i])
			}
			continue
		}
		if arg == "-" {
			positional = append(positional, arg)
		} else if strings.HasPrefix(arg, "-") {
			flagArgs = append(flagArgs, arg)
		} else {
			positional = append(positional, arg)
		}
	}
	if help {
		return o, true, nil
	}
	if err := fs.Parse(flagArgs); err != nil {
		return o, false, err
	}
	positional = append(positional, fs.Args()...)
	if len(positional) > 1 {
		return o, false, errors.New("only one input file may be specified")
	}
	o.input = "-"
	if len(positional) == 1 {
		o.input = positional[0]
	}
	if o.paging != "auto" && o.paging != "always" && o.paging != "never" {
		return o, false, fmt.Errorf("invalid --paging value: %s", o.paging)
	}
	if o.color != "auto" && o.color != "always" && o.color != "never" {
		return o, false, fmt.Errorf("invalid --color value: %s", o.color)
	}
	if o.plain && o.interactive {
		return o, false, errors.New("--plain and --interactive cannot be used together")
	}
	return o, false, nil
}
func usage() string {
	return "Usage: jcat [--plain|--interactive] [--paging auto|always|never] [--color auto|always|never] [FILE|-]\n\nPretty-print JSONL records. Named files open the interactive inspector when stdin and stdout are terminals. Reads stdin when FILE is omitted or '-'."
}
func tty() bool { return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) }
func colorEnabled(mode string, terminal bool) bool {
	if mode == "always" {
		return true
	}
	if mode == "never" {
		return false
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	return terminal && !noColor && os.Getenv("TERM") != "dumb"
}

func main() {
	o, help, err := parseArgs(os.Args[1:])
	if help {
		fmt.Println(usage())
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jcat:", err)
		os.Exit(2)
	}
	interactive := o.interactive || (!o.plain && o.paging == "auto" && o.input != "-" && tty())
	if o.interactive {
		if o.input == "-" {
			fatal("--interactive requires a named input file")
		}
		if !tty() {
			fatal("--interactive requires stdin and stdout to be terminals")
		}
	}
	if interactive {
		if err := runTUI(o.input, colorEnabled(o.color, tty())); err != nil {
			fatal(err.Error())
		}
		return
	}
	var in io.Reader = os.Stdin
	if o.input != "-" {
		f, e := os.Open(o.input)
		if e != nil {
			fatal(fmt.Sprintf("%s: %v", o.input, e))
		}
		defer f.Close()
		in = f
	}
	out := io.Writer(os.Stdout)
	var child *exec.Cmd
	var pagerPipe io.WriteCloser
	if o.paging == "always" || (o.paging == "auto" && term.IsTerminal(int(os.Stdout.Fd())) && hasLess()) {
		child = exec.Command("less", "-R", "-F", "-X", "-L", "--")
		pipe, e := child.StdinPipe()
		if e != nil {
			fatal(e.Error())
		}
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if e = child.Start(); e != nil {
			fatal("could not start less: " + e.Error())
		}
		pagerPipe = pipe
		out = pipe
	}
	err = render(in, out, o.input, colorEnabled(o.color, term.IsTerminal(int(os.Stdout.Fd()))))
	if pagerPipe != nil {
		_ = pagerPipe.Close()
	}
	if child != nil {
		waitErr := child.Wait()
		if err == nil && waitErr != nil && child.ProcessState.ExitCode() != 1 {
			err = waitErr
		}
	}
	if err != nil && !errors.Is(err, syscall.EPIPE) {
		fatal(err.Error())
	}
}
func jsonIndent(out *bytes.Buffer, raw []byte) error { return json.Indent(out, raw, "", "  ") }
func colorJSON(raw []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(raw); {
		switch raw[i] {
		case '"':
			start := i
			i++
			for i < len(raw) {
				if raw[i] == '\\' {
					i += 2
					continue
				}
				i++
				if raw[i-1] == '"' {
					break
				}
			}
			j := i
			for j < len(raw) && (raw[j] == ' ' || raw[j] == '\n' || raw[j] == '\t') {
				j++
			}
			if j < len(raw) && raw[j] == ':' {
				out.WriteString("\x1b[36m")
			} else {
				out.WriteString("\x1b[32m")
			}
			out.Write(raw[start:i])
			out.WriteString("\x1b[0m")
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			start := i
			i++
			for i < len(raw) && strings.ContainsRune("0123456789.eE+-", rune(raw[i])) {
				i++
			}
			out.WriteString("\x1b[33m")
			out.Write(raw[start:i])
			out.WriteString("\x1b[0m")
		case 't', 'f', 'n':
			n := 4
			if bytes.HasPrefix(raw[i:], []byte("false")) {
				n = 5
			}
			out.WriteString("\x1b[35m")
			out.Write(raw[i : i+n])
			out.WriteString("\x1b[0m")
			i += n
		default:
			out.WriteByte(raw[i])
			i++
		}
	}
	return out.Bytes()
}
func hasLess() bool  { _, err := exec.LookPath("less"); return err == nil }
func fatal(s string) { fmt.Fprintln(os.Stderr, "jcat:", s); os.Exit(1) }

// readLine reuses dst for arbitrarily long input lines. ReadSlice avoids the
// per-record allocation made by ReadBytes while retaining no Scanner limit.
func readLine(r *bufio.Reader, dst []byte) ([]byte, bool, error) {
	dst = dst[:0]
	for {
		part, err := r.ReadSlice('\n')
		dst = append(dst, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return dst, true, nil
		}
		if err != nil {
			return dst, false, err
		}
		return dst, false, nil
	}
}

func render(input io.Reader, output io.Writer, source string, color bool) error {
	r := bufio.NewReader(input)
	w := bufio.NewWriter(output)
	physical, record := 0, 0
	var line []byte
	for {
		var eof bool
		var err error
		line, eof, err = readLine(r, line)
		if len(line) == 0 && eof {
			break
		}
		if err != nil {
			return err
		}
		physical++
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			if eof {
				break
			}
			continue
		}
		var pretty bytes.Buffer
		if e := jsonIndent(&pretty, line); e != nil {
			return fmt.Errorf("%s: invalid JSON at physical line %d: %w", source, physical, e)
		}
		record++
		if _, e := fmt.Fprintf(w, "record %d | source line %d\n", record, physical); e != nil {
			return e
		}
		if color {
			if _, e := w.Write(colorJSON(pretty.Bytes())); e != nil {
				return e
			}
		} else if _, e := w.Write(pretty.Bytes()); e != nil {
			return e
		}
		if _, e := w.WriteString("\n\n"); e != nil {
			return e
		}
		if e := w.Flush(); e != nil {
			return e
		}
		if eof {
			break
		}
	}
	return nil
}
