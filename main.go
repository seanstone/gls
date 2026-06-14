// gls — an `ls` that colors entries by their git status.
//
// The expensive part of a git-aware listing is finding each file's status.
// Instead of asking git per file, we run a single
//
//	git status --porcelain=v2 --ignored -z
//
// which reports every non-clean path (untracked, ignored, staged, modified,
// conflicted) in one machine-readable pass. Anything not reported is clean.
// We map those paths onto the directory entries and pick an ANSI color.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ANSI SGR color codes.
const (
	colReset    = "\x1b[0m"
	colRed      = "31"   // untracked
	colGreen    = "32"   // staged
	colYellow   = "33"   // modified in worktree
	colBlue     = "34"   // clean directory (ls-style)
	colGray     = "90"   // ignored
	colBoldRed  = "1;31" // conflicted / unmerged
)

func main() {
	var (
		long    = flag.Bool("l", false, "long format: status, mode, size, mtime, name")
		all     = flag.Bool("a", false, "include entries starting with '.'")
		colorWhen = flag.String("color", "auto", "colorize output: auto, always, or never")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gls [-l] [-a] [--color=auto|always|never] [path]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}

	useColor := decideColor(*colorWhen)

	if err := run(dir, *long, *all, useColor); err != nil {
		fmt.Fprintln(os.Stderr, "gls:", err)
		os.Exit(1)
	}
}

func run(dir string, long, all, useColor bool) error {
	// Canonicalize so paths line up with git's (symlink-resolved) output.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	info, err := os.Stat(abs)
	if err != nil {
		return err
	}

	status := gitStatusMap(abs) // absPath -> 2-char code; nil if not a repo

	// A single non-directory argument: just print that one entry.
	if !info.IsDir() {
		printEntry(filepath.Dir(abs), fs.FileInfoToDirEntry(info), status, long, useColor)
		return nil
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		if !all && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		printEntry(abs, e, status, long, useColor)
	}
	return nil
}

func printEntry(dir string, e fs.DirEntry, status map[string]string, long, useColor bool) {
	full := filepath.Join(dir, e.Name())
	code := status[full] // "" when clean or not in a repo

	name := e.Name()
	if e.IsDir() {
		name += "/"
	}
	name = colorize(name, colorFor(code, e.IsDir()), useColor)

	if !long {
		fmt.Printf("%s %s\n", displayCode(code), name)
		return
	}

	var (
		mode  = "?---------"
		size  int64
		mtime = ""
	)
	if fi, err := e.Info(); err == nil {
		mode = fi.Mode().String()
		size = fi.Size()
		mtime = fi.ModTime().Format("Jan _2 15:04")
	}
	fmt.Printf("%s %s %9d %s %s\n", displayCode(code), mode, size, mtime, name)
}

// displayCode renders the 2-char status column the way `git status -s` does,
// turning porcelain's '.' placeholders into spaces. Clean entries get blanks.
func displayCode(code string) string {
	if code == "" {
		return "  "
	}
	return strings.ReplaceAll(code, ".", " ")
}

// colorFor maps a porcelain status code to an ANSI color. Status wins over the
// ls-style blue used for plain directories.
func colorFor(code string, isDir bool) string {
	switch code {
	case "":
		if isDir {
			return colBlue
		}
		return ""
	case "??":
		return colRed
	case "!!":
		return colGray
	case "UU":
		return colBoldRed
	}
	// Ordinary/renamed entry: XY where X=index (staged), Y=worktree.
	x, y := code[0], code[1]
	if y != '.' && y != ' ' {
		return colYellow // unstaged worktree change outstanding
	}
	if x != '.' && x != ' ' {
		return colGreen // staged, nothing unstaged
	}
	if isDir {
		return colBlue
	}
	return ""
}

func colorize(s, col string, useColor bool) string {
	if !useColor || col == "" {
		return s
	}
	return "\x1b[" + col + "m" + s + colReset
}

// gitStatusMap returns a map of absolute path -> 2-char status code for every
// non-clean entry under dir's repo. Returns nil if dir is not in a git repo.
func gitStatusMap(dir string) map[string]string {
	root, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil
	}
	root = strings.TrimRight(root, "\n")
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	out, err := gitOutput(dir, "status", "--porcelain=v2", "--ignored", "-z")
	if err != nil {
		return nil
	}

	m := make(map[string]string)
	set := func(code, rel string) {
		m[filepath.Join(root, rel)] = code
	}

	recs := strings.Split(out, "\x00")
	for i := 0; i < len(recs); i++ {
		rec := recs[i]
		if rec == "" {
			continue
		}
		switch rec[0] {
		case '1': // ordinary changed entry
			p := strings.SplitN(rec, " ", 9)
			if len(p) == 9 {
				set(p[1], p[8])
			}
		case '2': // renamed/copied — origPath is the next NUL field
			p := strings.SplitN(rec, " ", 10)
			if len(p) == 10 {
				set(p[1], p[9])
			}
			i++ // skip origPath record
		case 'u': // unmerged / conflicted
			p := strings.SplitN(rec, " ", 11)
			if len(p) == 11 {
				set("UU", p[10])
			}
		case '?': // untracked
			set("??", rec[2:])
		case '!': // ignored
			set("!!", rec[2:])
		case '#': // header line — ignore
		}
	}
	return m
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

func decideColor(when string) bool {
	switch when {
	case "always":
		return true
	case "never":
		return false
	default: // auto
		if os.Getenv("NO_COLOR") != "" {
			return false
		}
		fi, err := os.Stdout.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
}
