package cli

import "fmt"

// Color belongs to the output terminal, not the input used for confirmations.
// Labels remain meaningful in plain text and when output is redirected.
func (e *Env) label(level, text string) string {
	if !e.Color || (e.Getenv != nil && (e.Getenv("NO_COLOR") != "" || e.Getenv("TERM") == "dumb")) {
		return text
	}
	color := "36"
	switch level {
	case "ok":
		color = "32"
	case "warn", "action", "unknown":
		color = "33"
	case "fail":
		color = "31"
	}
	return "\x1b[" + color + "m" + text + "\x1b[0m"
}

func (e *Env) heading(title string) { fmt.Fprintf(e.Out, "\n%s\n\n", e.label("info", title)) }

func (e *Env) checks(checks []check) bool {
	failed := false
	for _, c := range checks {
		fmt.Fprintf(e.Out, "%s %s\n", e.label(c.level, fmt.Sprintf("%-7s", c.level)), c.text)
		failed = failed || c.level == "fail"
	}
	return failed
}

func (e *Env) next(title, detail string) {
	fmt.Fprintf(e.Out, "\n%s\n\n", e.label("action", "NEXT: "+title))
	fmt.Fprintln(e.Out, detail)
}
