# placer

*A placer deposit is a sediment bed you pan through to find the few nuggets worth
keeping. That's the workflow: 11,000 files in, a handful out.*

Fuzzy-searchable ADB file browser with vim keybindings, multi-select across
directories, and batch pull to a chosen destination.

**Status: phase 2 complete.** Browse, search, select, transfer, and inline
image previews (jpeg/png/gif/dng) on cursor rest, with a Unicode half-block
fallback everywhere and a Kitty-protocol path for terminals that support it.
Video/audio previews and document previews land in phases 3–4.

```
 1:Photos(10397)  2:Video(483)  3:Audio(125)  4:Docs(50)  5:All(11055)   R58M20XXXXX · sort:date · skip
 3 selected (11.4M)  → /Users/you/Downloads
    name                                          size     date             type
 ▸✓ PXL_20260725_180427752.jpg                     2.0M     2026-07-25 18:04 jpeg
    PXL_20260725_175739170.jpg                     2.7M     2026-07-25 17:57 jpeg
    Recording_003.wav                              1.2M     2026-07-24 09:12 3:41
```

## Install

Two external dependencies, and only the first is required:

| | Required | Purpose |
|---|---|---|
| `adb` | yes | everything |
| `ffmpeg` | no (phases 2–4) | video frames, audio playback, waveforms |

```sh
brew install android-platform-tools ffmpeg          # macOS
sudo apt install android-sdk-platform-tools ffmpeg  # Linux

go build -o placer . && mv placer ~/.local/bin/
```

The binary is fully static and CGO-free on darwin/{arm64,amd64} and
linux/{amd64,arm64} — `make release` builds all four.

## Usage

```sh
placer                        # the attached device
placer -s R58M20XXXXX      # a specific device
placer -fake                  # synthetic library, no device needed
placer -fixtures testdata/fixtures
```

### Keys

| Key | Action |
|---|---|
| `j` `k` | down / up |
| `gg` `G` | top / bottom |
| `ctrl+d` `ctrl+u` | half page |
| `1`–`5`, `gt` `gT` | switch tab |
| `tab` `space` | toggle selection |
| `v` `V` | anchor a range at the cursor (`j`/`k` extends, any select key commits) |
| `ctrl+a` / `ctrl+x` | select / clear all visible |
| `*` | select everything matching the current filter |
| `y` | add to selection without toggling |
| `s` | selection review (`d` removes, `c` clears) |
| `d` | destination picker |
| `p` | pull selection |
| `/` | fuzzy search (`esc` keeps the filter, `ctrl+c` clears) |
| `B` | quick Camera-only bucket toggle |
| `r` | re-index |
| `?` | help |
| `q` | quit (guards a non-empty selection) |

Commands: `:dest <path>`, `:mkdir <name>`, `:sort date|name|size`,
`:policy skip|overwrite|rename`, `:filter <q>`, `:bucket <name>` (`:buckets`
browses every album/folder with counts), `:clear`, `:refresh`, `:pull`,
`:set preview|autoplay|audio on|off`, `:q` `:q!` `:wq`.

Image previews render on cursor rest, debounced ~120ms and cancelled the
moment the cursor moves. jpeg/png/gif decode via Go's stdlib; dng previews
are extracted pure Go from the embedded JPEG in the TIFF container; heic
shows a metadata card (no pure-Go decoder, not worth a dependency for a
handful of files). Cached to `~/.cache/placer/thumbs/`, keyed by
path+size+date_added+pane size+protocol, so nothing is ever re-fetched or
re-rendered needlessly.

Selection is a **manifest**, independent of tab, filter and directory — it
survives all navigation and is persisted to `~/.cache/placer/session.json`, so a
crash never loses curation work.

## Design notes

Measured against a Pixel 6a, and these numbers drove the design:

- **`adb pull` runs at 23–41 MB/s**; a 2.8 MB photo lands in ~120 ms. An
  earlier plan to fetch only the EXIF thumbnail from the first 128 KB was cut —
  it saved ~30 ms and returned a worse image.
- **The index loads once**, one bulk `content query` per collection, and all
  filtering happens locally, so fuzzy search never touches the phone.
- **Camera MP4s put `moov` at the end**, so phase 3's video previews use a
  sparse head+tail reconstruction: 42 s → 1.23 s on a 1.7 GB file, producing a
  byte-identical frame.
- Local mtime is set from MediaStore `datetaken`, so pulled photos sort
  correctly in Finder.

Two ADB details that cause silent corruption if ignored, both encoded in the
code:

1. `adb shell` re-joins argv and the device re-splits it, so a whole
   `content query …` invocation must be **one quoted string**.
2. `adb shell` mangles `\n` → `\r\n`. Anything binary goes through
   **`adb exec-out`**.

## Development without a device

Every ADB interaction sits behind the `device.Device` interface, so the TUI,
indexer and transfer engine are fully testable with no phone attached:

```sh
make run                 # synthetic library shaped like the real Pixel 6a
make test                # unit tests
make check               # vet + test + race + gofmt
make fixtures            # record real device output (device required)
PLACER_SNAPSHOT=1 go test ./internal/ui -run TestSnapshot -v   # eyeball screens
```

`internal/device/parse.go` is the highest-risk code in the project: `content
query` output is *not* safely comma-splittable, because display names and album
names contain `", "`. It parses on `, <key>=` boundaries built from the
projection instead. See `parse_test.go` for the adversarial cases.

## Layout

```
main.go                     flags, device selection
internal/device/            Device interface, ADB impl, fake, content-query parser
internal/index/             bulk load, dedupe, sort, fuzzy filter
internal/session/           selection manifest + config, both persisted
internal/transfer/          worker pool, collision policy, mtime, verification
internal/ui/                Bubble Tea model, vim keymap, views
cmd/capture-fixtures/       record real device output for offline dev
```

Full spec and phasing: `~/git/cookbooks/rabbitholes/`
(`adb-fuzzy-file-browser-implementation-scope.md`, `placer-build-handoff.md`).
