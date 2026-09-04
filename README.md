# shiny-goggles

See what the go command is actually doing — while it does it.

`go build` tells you nothing until it finishes. `shiny-goggles` attaches to it
and shows which packages are compiling, which ones are slow, which one everything
else is waiting for, and — with `-monitor` — what your program does once it
starts.

It needs nothing from the project it profiles: no imports, no code changes, no
build tags.

![demo.gif](demo.gif)

or

![verbose.gif](verbose.gif)

## Install

```sh
go install github.com/fortael/shiny-goggles/cmd/shiny-goggles@latest
```

That builds the binary locally and puts `shiny-goggles` in
`$(go env GOPATH)/bin` — add it to your `PATH` if it is not there yet.
Needs Go 1.25 or newer.

## Use

The `go` is implied, so any go command works as-is:

```sh
shiny-goggles build ./...
shiny-goggles -verbose test ./internal/...
shiny-goggles -monitor run . serve
```

## Screens

| Flag       | What you get                                                                                                |
|------------|-------------------------------------------------------------------------------------------------------------|
| *(none)*   | A spinner, a progress bar and the packages compiling right now. Four lines, nothing else.                     |
| `-verbose` | Slowest packages, what blocked the build, a timeline, and a report with the critical path. Kept on screen.    |
| `-clear`   | The same screen while building, erased when it ends — only a one line summary and your program are left.      |
| `-monitor` | Builds, then stays with the running program: memory, GC, cpu, goroutines and its output on tabs. Needs `run`. |
| `-plain`   | Never takes over the terminal, prints plain progress lines. For CI and pipes.                                 |

## Flags

| Flag                | Default | What it does                                                                       |
|---------------------|---------|------------------------------------------------------------------------------------|
| `-top N`            | `10`    | Rows reserved for the slowest-packages chart (5–10).                                |
| `-blocking N`       | `5`     | Rows for the packages that compiled alone while everything else waited.             |
| `-recent N`         | `5`     | Rows for the list of packages that just finished.                                   |
| `-hide-std`         | off     | Leave standard library packages out of the lists.                                   |
| `-ignore PATTERN`   | —       | Keep packages out of every list and graph (see *Notes*). Repeatable.                |
| `-trace FILE`       | —       | Write a trace of the build; open it at [ui.perfetto.dev](https://ui.perfetto.dev).  |
| `-go-trace FILE`    | —       | The go command's own `-debug-trace` instead (see *Notes*).                          |
| `-actiongraph FILE` | —       | Dump the build action graph (`-debug-actiongraph`).                                 |
| `-pprof ADDR`       | —       | Monitor: read live profiles from a program serving `net/http/pprof`.                |
| `-goroutines`       | off     | Monitor: count goroutines without pprof, via `GODEBUG=scheddetail` (see *Notes*).   |
| `-no-history`       | off     | Do not read or write the build duration cache.                                      |

## Keys

| Key                | Verbose          | Monitor                                        |
|--------------------|------------------|------------------------------------------------|
| `q`, `ctrl+c`      | Stop the build   | Ask the program to stop; again to stop waiting |
| `v`                | Show/hide stdlib | —                                              |
| `tab`, `1` `2` `3` | —                | Switch runtime / output / profile              |
| `↑ ↓ pgup pgdn`    | —                | Scroll the output; `g` follows it again        |

## How it works

The go command can run every compile, assemble and link step through a wrapper
of your choosing. `shiny-goggles` makes itself that wrapper, so each tool
invocation reports when it started and how long it took, over a unix socket.

A few consequences worth knowing:

- **The progress bar is honest.** `go build -n` is asked, alongside the real
  build, what it is *about* to compile — cached packages are simply not in that
  answer. The denominator is the real one, not a guess.
- **The build cache is untouched.** Tool identities (`-V=full`) are passed
  through unchanged, so builds with and without `shiny-goggles` share cache
  entries. Running it never costs you a rebuild.
- **Overhead is about 6%** on a cold 258-package build, and milliseconds on an
  incremental one.

## Notes

- `-ignore` hides packages from the lists, charts, timeline and trace, but not
  from the totals — they were built, so the counters and the progress bar still
  include them. `*` spans slashes (`example.com/org/*` covers a whole tree) and a
  pattern without one names a package and everything under it. Set
  `SHINY_GOGGLES_IGNORE` instead of the flag to keep private paths off a command
  line that ends up in a screenshot or a recording.
- `-monitor` gets its numbers from `GODEBUG` traces and from the operating
  system, so it works on any Go program. `-pprof` adds goroutine stacks and cpu
  and heap tables on top, and needs the program to serve `net/http/pprof`.
- `-goroutines` makes the runtime walk and print every goroutine under the
  scheduler lock twice a second. That is fine for a few hundred, less so for
  tens of thousands.
- `-go-trace` writes the go command's own trace. Perfetto reports thousands of
  `FLOW_NO_ENCLOSING_SLICE` errors on it, and with `go run` it keeps recording
  while your program runs. `-trace` avoids both.
- Compiler phase timings and `scheddetail` rely on internal toolchain flags with
  no compatibility promise. If a future Go changes their output, those sections
  go quiet — nothing else breaks.

## License

MIT
