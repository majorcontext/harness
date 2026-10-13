package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

var (
	// errHelp reports that a command printed its usage on request; main exits 0.
	errHelp = errors.New("help requested")
	// errUsage reports that a command printed a usage error on stderr; main exits 2.
	errUsage = errors.New("usage error")
)

// parseFlags is the only caller of FlagSet.Parse. A help request (-h or
// --help) prints the usage on stdout and returns errHelp. A flag error or a
// stray argument prints the error and the usage on stderr and returns
// errUsage. The command does not run in either case.
func parseFlags(fs *flag.FlagSet, args []string) error {
	fs.SetOutput(io.Discard)
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		printFlagUsage(fs, os.Stdout)
		return errHelp
	case err != nil:
		return usageError(fs, err)
	case fs.NArg() > 0:
		return usageError(fs, fmt.Errorf("unexpected argument %q", fs.Arg(0)))
	}
	return nil
}

func usageError(fs *flag.FlagSet, err error) error {
	fmt.Fprintln(os.Stderr, "harness:", err)
	printFlagUsage(fs, os.Stderr)
	return errUsage
}

func printFlagUsage(fs *flag.FlagSet, w io.Writer) {
	_, _ = fmt.Fprintf(w, "usage: harness %s [flags]\n", fs.Name())
	fs.SetOutput(w)
	fs.PrintDefaults()
}
