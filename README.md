# adbfz

Fuzzy-searchable ADB file browser with vim keybindings, multi-select across
directories, and batch pull to a chosen destination.

**Status: phase 1 complete.** Browse, search, select, transfer. Previews
(photo/video/audio/documents) land in phases 2–4.

```
 1:Photos(10397)  2:Video(483)  3:Audio(125)  4:Docs(50)  5:All(11055)   23311JEGR07766 · sort:date · skip
 3 selected (11.4M)  → /Users/sdx/Downloads
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

go build -o adbfz . && mv adbfz ~/.local/bin/
```

The binary is fully static and CGO-free on darwin/{arm64,amd64} and
linux/{amd64,arm64} — `make release` builds all four.

## Usage

```sh
adbfz                        # the attached device
adbfz -s 23311JEGR07766      # a specific device
adbfz -fake                  # synthetic library, no device needed
adbfz -fixtures testdata/fixtures
```

### Keys

| Key | Action |
|---|---|
| `j` `k` | down / up |
| `gg` `G` | top / bottom |
| `ctrl+d` `ctrl+u` | half page |
| `1`–`5`, `gt` `gT` | switch tab |
| `tab` `space` | toggle selection |
| `v` | visual mode (`j`/`k` extends, `tab` toggles the range) |
| `V` | select all visible |
| `y` | add to selection without toggling |
| `s` | selection review (`d` removes, `c` clears) |
| `d` | destination picker |
| `p` | pull selection |
| `/` | fuzzy search (`esc` keeps the filter, `ctrl+c` clears) |
| `r` | re-index |
| `?` | help |
| `q` | quit (guards a non-empty selection) |

Commands: `:dest <path>`, `:mkdir <name>`, `:sort date|name|size`,
`:policy skip|overwrite|rename`, `:filter <q>`, `:clear`, `:refresh`, `:pull`,
`:q` `:q!` `:wq`.

Selection is a **manifest**, independent of tab, filter and directory — it
survives all navigation and is persisted to `~/.cache/adbfz/session.json`, so a
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
ADBFZ_SNAPSHOT=1 go test ./internal/ui -run TestSnapshot -v   # eyeball screens
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
(`adb-fuzzy-file-browser-implementation-scope.md`, `adbfz-build-handoff.md`).
