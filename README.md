# jcat

Read-only JSONL trace viewer. The Go implementation is the main package and uses Bubble Tea, Huh, and Lip Gloss. On a terminal, a named file opens in a two-pane record and field inspector. Pipes, stdin, and redirected output use the streaming formatter.

```sh
go test ./...
go vet ./...
go build -trimpath -ldflags '-s -w' -o jcat-go .
./jcat-go sample.jsonl
```

`--plain` forces formatter mode; `--interactive` requires a named file and terminal input/output. In the inspector, `/` searches the focused pane, `Enter` applies a record filter, `Tab` changes focus, and `r` toggles fields/raw JSON. `j`/`k` or arrows navigate, `n`/`N` visit matches, `?` opens help, and `q` exits. Search is case-insensitive literal text.

Both implementations render Markdown and XML inside JSONL string fields in the inspector. Fields with recognizable Markdown syntax show formatted headings, emphasis, lists, and code. Fields containing XML show highlighted tags and indented elements, with mixed text and `xml:space` preserved. Press `r` to inspect the original strings in raw JSON.

The Rust Ratatui implementation is available in [`rust/`](rust/). Build and run it with:

```sh
cargo test --manifest-path rust/Cargo.toml
cargo build --release --manifest-path rust/Cargo.toml
./rust/target/release/jcat sample.jsonl
```

After local setup, `jcat` runs Rust and `jgcat` runs Go. Formatter mode preserves object order and number precision, skips blank lines while retaining physical line numbers, and reports malformed JSON with its source line.
