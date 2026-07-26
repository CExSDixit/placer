# placer

*A placer deposit is a sediment bed you pan through to find the few nuggets worth
keeping. That's the workflow: 11,000 files in, a handful out.*

Fuzzy-searchable ADB file browser with vim keybindings, multi-select across
directories, and batch pull to a chosen destination.

**Status: phase 3 complete.** Browse, search, select, transfer, and inline
previews on cursor rest for images (jpeg/png/gif/dng), video (a still frame
grabbed from a sparse head+tail reconstruction) and audio (waveform, ffprobe
metadata, and playback with a full transport) — with quadrant-block rendering
as a universal fallback and a Kitty-protocol path for terminals that support
it. Document previews land in phase 4.

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
| `ffmpeg` | no | video frames, audio waveforms; `ffplay` ships with it and drives playback |

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

In the **Audio** tab, `j`/`k` also plays what the cursor lands on — the
scrub-through-voice-memos flow — and these are bound on top of the above:

| Key | Action |
|---|---|
| `space` | play / pause. **`tab` still selects** — space is transport here and only here |
| `h` `l` | seek ∓5 s |
| `H` `L` | seek ∓30 s |
| `[` `]` | playback speed, 0.5×–2× |

In the **Video** tab, with `:set autoplay on`, the same keys scrub the frame
the preview is grabbed from — a single still at 0:01 identifies some
recordings and not others, so stepping through is what makes the preview
usable for review:

| Key | Action |
|---|---|
| `h` `l` | move the previewed frame ∓5 s |
| `H` `L` | move the previewed frame ∓30 s |
| `0` | back to 0:01 |

Commands: `:dest <path>`, `:mkdir <name>`, `:sort date|name|size`,
`:policy skip|overwrite|rename`, `:filter <q>`, `:bucket <name>` (`:buckets`
browses every album/folder with counts), `:clear`, `:refresh`, `:pull`,
`:set preview|autoplay|audio on|off`, `:set render quadrant|halfblock`,
`:q` `:q!` `:wq`. In command mode `tab`
completes the command and its argument, and up/down walk this session's
history.

### Previews

Previews render on cursor rest, debounced ~120 ms and cancelled the moment the
cursor moves. Everything funnels into one decode → downscale → render chain,
and every tier degrades to a metadata card rather than failing.

| Type | Approach |
|---|---|
| jpeg / png / gif | Go stdlib decode. 99.4% of the library. |
| dng | embedded JPEG extracted from the TIFF container, pure Go |
| video | still frame via sparse reconstruction (below), scrubbable with h/l; **off by default** — `:set autoplay on` |
| audio | `showwavespic` waveform + `ffprobe` metadata, and the file is cached for playback |
| heic, everything else | metadata card |

Video is gated behind `:set autoplay on` deliberately: a frame grab costs
~1.4 s even with the sparse trick, and firing one on every cursor move would
fight `j`/`k`. Off, a video row shows what MediaStore already knows, with no
device round trip at all.

**Preview quality is set by what the terminal can draw**, and the header shows
which path you are on:

| Renderer | Pixels per cell | Where |
|---|---|---|
| `kitty` / `iterm` / `sixel` | a real image | Ghostty, kitty, WezTerm, iTerm2 |
| `quadrant` | 2×2, via `▘▝▀▖▌▞▛…` | the default fallback |
| `halfblock` | 1×2, via `▀` | `:set render halfblock` |

Quadrant blocks are the default where no image protocol exists: they are
legacy code-page glyphs present in every monospace font, so compatibility is
the same as half-blocks for twice the horizontal resolution. Each cell can
carry only two colours, so the renderer tries all 16 foreground/background
partitions of the 2×2 block and keeps the one with the least squared error.
`:set render halfblock` reverts if a font draws them with gaps.

None of that closes the gap with a graphics protocol. **If previews matter,
run placer in Ghostty or kitty** — a third of a window is ~48×35 cells, which
is ~96×70 pixels of quadrant blocks against a real image at Kitty-protocol
resolution.

Rendered previews cache to `~/.cache/placer/thumbs/`, keyed by
path+size+date_added+pane size+protocol+frame; pulled audio caches to
`~/.cache/placer/media/`, so pressing `space` after a preview is instant.
The media cache holds whole audio files — voice memos on the reference device
run 130–300 MB — so it is **capped at 2 GiB and pruned oldest-first at
startup**; thumbs are capped at 256 MiB.

Playback shells out to `ffplay` (or `mpv` if present, `afplay` as a macOS
last resort) with the TUI owning the playhead — pause kills the process and
remembers the offset, seek kills and restarts with a new `-ss`. Crude, but it
needs no platform-specific code and no pure-Go decoder, which would have meant
CGO. `ebitengine/oto` was tested and rejected for exactly that reason.

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
- **Camera MP4s put `moov` at the end** — measured, not assumed. The 894 MB
  reference recording is `ftyp`(28 B) → `mdat`(894 MB) → `moov`(533 KB), so a
  naive frame grab means pulling the whole file. Instead placer `dd`s the head
  of `mdat` and the tail holding `moov`, writes them into a locally sparse file
  at their true offsets, and lets ffmpeg seek between them; the hole is never
  read. Re-measured 2026-07-26 on the current largest video (1,735 MB /
  10m56s): **41.5 s full pull → 1.45 s sparse, 28.7× faster, byte-identical
  frame.**
- **Scrubbing generalises the same trick.** h/l fetch a third region — a window
  positioned from the bitrate around the seek point, backed off far enough to
  include the preceding keyframe — alongside the header (ffmpeg needs `ftyp` at
  offset 0 to identify the container at all) and the tail. A frame 10 minutes
  into that 1,735 MB recording takes 2.9 s.
- **The head is sized from the bitrate, not fixed.** A flat 4 MB head covers
  only 1.45 s of a 2.63 MB/s recording, which drops the decoder into the hole
  immediately after the wanted frame. It still produced a byte-identical frame,
  but relying on "the corruption starts after the bytes we needed" is not a
  design — the head now covers `frameSeek + 2 s` of real video.
- **`dd`'s summary lands in the payload.** `adb exec-out` folds device stderr
  into stdout, so `dd` appends exactly 78 bytes of "4+0 records in" to every
  read. `2>/dev/null` runs *on the device* for this reason; without it a 4 MB
  head arrives as 4,194,382 bytes and the whole reconstruction is offset
  garbage that still decodes.
- Local mtime is set from MediaStore `datetaken`, so pulled photos sort
  correctly in Finder.

Three ADB details that cause silent corruption if ignored, all encoded in the
code and guarded by tests:

1. `adb shell` re-joins argv and the device re-splits it, so a whole
   `content query …` invocation must be **one quoted string**.
2. `adb shell` mangles `\n` → `\r\n`. Anything binary goes through
   **`adb exec-out`**.
3. `adb exec-out` folds *device* stderr into stdout, so any device-side
   command that chats — `dd` does — must redirect it **on the device**.

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
internal/preview/           pull, decode, downscale, cache, render; sparse video grab, waveforms
internal/player/            ffplay/mpv/afplay lifecycle; the TUI owns the playhead
internal/transfer/          worker pool, collision policy, mtime, verification
internal/ui/                Bubble Tea model, vim keymap, views
cmd/capture-fixtures/       record real device output for offline dev
```

Full spec and phasing: `~/git/cookbooks/rabbitholes/`
(`adb-fuzzy-file-browser-implementation-scope.md`, `placer-build-handoff.md`).
