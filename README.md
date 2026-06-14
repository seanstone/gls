# gls

`ls`, but each entry is colored by its **git status**.

A single, dependency-light Go binary (standard library + the `git` and `stty`
subprocesses). One `git status --porcelain=v2 --ignored -z` call provides every
file's status in a single pass, regardless of how many files are listed.

## Install

```sh
make install        # builds and copies to ~/.local/bin
```

Override the location with `PREFIX`:

```sh
make install PREFIX=/usr/local   # installs to /usr/local/bin
```

Or just build the binary in place:

```sh
make build          # produces ./gls
```

## Usage

```
gls [-l] [-h] [-a] [-1] [-C] [--color=auto|always|never] [path]
```

| Flag | Meaning |
|------|---------|
| `-l` | long format: status, mode, size, mtime, name |
| `-h` | with `-l`, human-readable sizes (e.g. `1.5K`, `5.0M`) |
| `-a` | include entries starting with `.` |
| `-1` | force one entry per line |
| `-C` | force multi-column (grid) output |
| `--color` | `auto` (default), `always`, or `never` |

Short flags bundle like `ls`, so `gls -lha` works.

On a terminal the default is a compact, colored grid (like `ls`). When piped it
falls back to one entry per line with a 2-char status code (like `git status -s`),
so the status survives without color.

## Colors

| Color | Status |
|-------|--------|
| red | untracked |
| green | staged |
| yellow | modified or deleted in the working tree |
| bold red | conflicted / unmerged |
| gray | ignored |
| cyan | clean directory |

Extras:

- **Directories roll up** their contents: a clean directory containing changes
  takes the color of its highest-precedence descendant (conflict > unstaged >
  staged > untracked).
- **Deleted tracked files** still appear, struck through, even though they are
  gone from disk.

## License

MIT — see [LICENSE](LICENSE).
