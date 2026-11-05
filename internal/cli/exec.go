package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner runs local programs (gh, git, ssh-keygen, ssh-keyscan, upstate). Secret values
// only ever travel on stdin; tests substitute a fake that asserts exactly that.
type Runner interface {
	Run(stdin io.Reader, name string, args ...string) ([]byte, error)
	LookPath(name string) error
}

// Pipe runs `from` and feeds its stdout straight into `to`'s stdin, so a value (an Upstate
// hook token) moves between two programs without passing through this process's output.
type Piper interface {
	Pipe(from []string, fromEnv []string, to []string) error
}

type osRunner struct{ dir string }

func (r osRunner) Run(stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = r.dir
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(name + " " + firstArg(args) + ": " + msg)
	}
	return out.Bytes(), nil
}

func (r osRunner) LookPath(name string) error {
	_, err := exec.LookPath(name)
	return err
}

func (r osRunner) Pipe(from []string, fromEnv []string, to []string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	src := exec.Command(from[0], from[1:]...)
	src.Dir = r.dir
	src.Env = append(os.Environ(), fromEnv...)
	src.Stdout = pw
	src.Stderr = io.Discard
	dst := exec.Command(to[0], to[1:]...)
	dst.Dir = r.dir
	dst.Stdin = pr
	var errb bytes.Buffer
	dst.Stderr = &errb
	if err := src.Start(); err != nil {
		pr.Close()
		pw.Close()
		return errors.New(from[0] + " could not start; nothing was stored")
	}
	pw.Close()
	srcErr := src.Wait()
	if srcErr != nil {
		pr.Close()
		return errors.New(from[0] + " failed; nothing was stored")
	}
	if err := dst.Start(); err != nil {
		pr.Close()
		return err
	}
	pr.Close()
	if err := dst.Wait(); err != nil {
		return errors.New(to[0] + " failed: " + strings.TrimSpace(errb.String()))
	}
	return nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		return args[0] + " " + args[1]
	}
	return args[0]
}
