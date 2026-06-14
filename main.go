// gls — an `ls` that colors entries by their git status.
//
// The expensive part of a git-aware listing is finding each file's status.
// Instead of asking git per file, we run a single
//
//	git status --porcelain=v2 --ignored -z
//
// which reports every non-clean path (untracked, ignored, staged, modified,
// deleted, conflicted) in one machine-readable pass. Anything not reported is
// clean. We map those paths onto the directory entries and pick an ANSI color.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ANSI SGR color codes.
const (
	colReset   = "\x1b[0m"
	colRed     = "31"   // untracked
	colGreen   = "32"   // staged
	colYellow  = "33"   // modified / deleted in worktree
	colBlue    = "34"   // clean directory (ls-style)
	colGray    = "90"   // ignored
	colBoldRed = "1;31" // conflicted / unmerged
	sgrStrike  = "9"    // struck through (deleted ghost rows)
)

// item is one row in the listing. It represents either a real directory entry
// or a synthesized "ghost" — a path git reports (e.g. a deletion) that no
// longer exists on disk, so os.ReadDir never returns it.
type item struct {
	name  string
	isDir bool
	info  fs.FileInfo // nil for ghosts
	code  string      // 2-char git status code, "" when clean
	ghost bool        // not present on disk
}

func main() {
	var (
		long      = flag.Bool("l", false, "long format: status, mode, size, mtime, name")
		all       = flag.Bool("a", false, "include entries starting with '.'")
		one       = flag.Bool("1", false, "force one entry per line")
		cols      = flag.Bool("C", false, "force multi-column (grid) output")
		colorWhen = flag.String("color", "auto", "colorize output: auto, always, or never")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: gls [-l] [-a] [-1] [-C] [--color=auto|always|never] [path]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}

	useColor := decideColor(*colorWhen)
	// Grid is the compact default on a terminal; line modes stay script-friendly.
	grid := !*long && !*one && (*cols || stdoutIsTTY())

	if err := run(dir, *long, *all, grid, useColor); err != nil {
		fmt.Fprintln(os.Stderr, "gls:", err)
		os.Exit(1)
	}
}

func run(dir string, long, all, grid, useColor bool) error {
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
		printItem(item{
			name:  info.Name(),
			isDir: false,
			info:  info,
			code:  status[abs],
		}, long, useColor)
		return nil
	}

	items, err := collect(abs, status, all)
	if err != nil {
		return err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	if grid {
		printGrid(items, useColor)
		return nil
	}
	for _, it := range items {
		printItem(it, long, useColor)
	}
	return nil
}

// collect builds the list of rows for a directory: every on-disk entry plus
// ghost rows for paths git reports as deleted (gone from disk). Statuses
// deeper than a direct child roll up to the subdirectory that contains them.
func collect(abs string, status map[string]string, all bool) ([]item, error) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	hidden := func(name string) bool { return !all && strings.HasPrefix(name, ".") }

	// One pass over the status map partitions changes by top-level name:
	// deeper paths roll up to their subdirectory; direct deletions (absent
	// from disk) become ghost rows.
	rollup := make(map[string]string) // subdir name -> highest-precedence descendant code
	ghosts := make(map[string]string) // direct child name -> deletion code
	prefix := abs + string(os.PathSeparator)
	for key, code := range status {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rel := key[len(prefix):]
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			if seg := rel[:i]; rank(code) > rank(rollup[seg]) {
				rollup[seg] = code
			}
		} else if strings.ContainsRune(code, 'D') {
			ghosts[rel] = code
		}
	}

	items := make([]item, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		name := e.Name()
		if hidden(name) {
			continue
		}
		seen[name] = true
		fi, _ := e.Info()
		code := status[filepath.Join(abs, name)]
		if e.IsDir() && code == "" {
			code = rollup[name] // "" when nothing rolled up
		}
		items = append(items, item{name: name, isDir: e.IsDir(), info: fi, code: code})
	}

	// Direct children git reports deleted but absent from disk.
	for name, code := range ghosts {
		if seen[name] || hidden(name) {
			continue
		}
		seen[name] = true
		items = append(items, item{name: name, code: code, ghost: true})
	}
	// Whole subdirectories that vanished (every file under them deleted).
	for name, code := range rollup {
		if seen[name] || hidden(name) {
			continue
		}
		seen[name] = true
		items = append(items, item{name: name, isDir: true, code: code, ghost: true})
	}
	return items, nil
}

// rank orders status codes for directory rollup; higher wins. Clean and
// ignored contribute nothing, so they never recolor a tracked directory.
func rank(code string) int {
	switch code {
	case "", "!!":
		return 0
	case "UU":
		return 5
	case "??":
		return 2
	}
	if len(code) >= 2 {
		if code[1] != '.' && code[1] != ' ' {
			return 4 // unstaged worktree change
		}
		if code[0] != '.' && code[0] != ' ' {
			return 3 // staged
		}
	}
	return 0
}

func printItem(it item, long, useColor bool) {
	name := it.name
	if it.isDir {
		name += "/"
	}
	name = render(name, colorFor(it.code, it.isDir), it.ghost, useColor)

	if !long {
		fmt.Printf("%s %s\n", displayCode(it.code), name)
		return
	}

	mode, size, mtime := "----------", int64(0), ""
	if it.info != nil {
		mode = it.info.Mode().String()
		size = it.info.Size()
		mtime = it.info.ModTime().Format("Jan _2 15:04")
	}
	fmt.Printf("%s %s %9d %12s %s\n", displayCode(it.code), mode, size, mtime, name)
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
	if len(code) >= 2 {
		x, y := code[0], code[1]
		if y != '.' && y != ' ' {
			return colYellow // unstaged worktree change (incl. deletion) outstanding
		}
		if x != '.' && x != ' ' {
			return colGreen // staged, nothing unstaged
		}
	}
	if isDir {
		return colBlue
	}
	return ""
}

// render wraps a name in ANSI attributes: an optional color plus strikethrough
// for ghost (deleted) rows. A no-op when color is disabled.
func render(s, col string, strike, useColor bool) string {
	if !useColor {
		return s
	}
	var sgr string
	if strike {
		sgr = sgrStrike
	}
	if col != "" {
		if sgr != "" {
			sgr += ";"
		}
		sgr += col
	}
	if sgr == "" {
		return s
	}
	return "\x1b[" + sgr + "m" + s + colReset
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
		return os.Getenv("NO_COLOR") == "" && stdoutIsTTY()
	}
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// printGrid lays the names out in column-major order, like `ls`: entries run
// down each column then across, packed into as many columns as the terminal
// width allows. Only names (colored, with a trailing slash for dirs) appear —
// status is conveyed by color, keeping the grid compact.
func printGrid(items []item, useColor bool) {
	if len(items) == 0 {
		return
	}
	n := len(items)
	plain := make([]int, n)    // display width of each cell
	colored := make([]string, n)
	for i, it := range items {
		name := it.name
		if it.isDir {
			name += "/"
		}
		plain[i] = utf8.RuneCountInString(name)
		colored[i] = render(name, colorFor(it.code, it.isDir), it.ghost, useColor)
	}

	const gap = 2
	width := termWidth()

	// Pick the largest column count whose packed layout fits the width.
	cols, rows, colw := 1, n, []int{maxWidth(plain)}
	for c := min(n, width); c >= 1; c-- {
		r := (n + c - 1) / c
		w := make([]int, c)
		total := 0
		for j := 0; j < c; j++ {
			for k := j * r; k < (j+1)*r && k < n; k++ {
				if plain[k] > w[j] {
					w[j] = plain[k]
				}
			}
			total += w[j]
			if j > 0 {
				total += gap
			}
		}
		if total <= width {
			cols, rows, colw = c, r, w
			break
		}
	}

	var b strings.Builder
	for r := 0; r < rows; r++ {
		for j := 0; j < cols; j++ {
			idx := j*rows + r
			if idx >= n {
				continue
			}
			b.WriteString(colored[idx])
			if idx+rows < n { // another cell follows to the right
				b.WriteString(strings.Repeat(" ", colw[j]-plain[idx]+gap))
			}
		}
		b.WriteByte('\n')
	}
	fmt.Print(b.String())
}

func maxWidth(ws []int) int {
	m := 0
	for _, w := range ws {
		if w > m {
			m = w
		}
	}
	return m
}

// termWidth returns the terminal column count, preferring $COLUMNS and falling
// back to `stty size` on the controlling terminal, then to 80.
func termWidth() int {
	if c := strings.TrimSpace(os.Getenv("COLUMNS")); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 {
			return n
		}
	}
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		cmd := exec.Command("stty", "size")
		cmd.Stdin = tty
		if out, err := cmd.Output(); err == nil {
			if f := strings.Fields(string(out)); len(f) == 2 {
				if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 80
}
