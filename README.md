# placer

*A placer deposit is a sediment bed you pan through to find the few nuggets
worth keeping. That's the workflow: 11,000 files in, a handful out.*

**placer** is a terminal file browser for getting photos, videos, and voice
memos off an Android phone and onto your Mac (or Linux box) — without Android
File Transfer, without cloud round-trips, without clicking through MTP folder
trees. Plug the phone in, fuzzy-search across the whole media library, preview
files inline in the terminal, cherry-pick with vim keys, and batch-pull to
wherever you want.

```
 1:Photos(10397)  2:Video(483)  3:Audio(125)  4:Docs(50)  5:All(11055)   R58M20XXXXX · sort:date · skip
 3 selected (11.4M)  → /Users/you/Downloads
    name                                          size     date             type
 ▸✓ PXL_20260725_180427752.jpg                     2.0M     2026-07-25 18:04 jpeg
    PXL_20260725_175739170.jpg                     2.7M     2026-07-25 17:57 jpeg
    Recording_003.wav                              1.2M     2026-07-24 09:12 3:41
```

## Highlights

- **One index, instant search.** The phone's MediaStore is loaded in one bulk
  query per collection; after that every fuzzy filter, sort, and tab switch is
  local. Search never touches the device.
- **Inline previews, right in the terminal.** Images render as real pixels in
  Ghostty/kitty/WezTerm/iTerm2, and as Unicode quadrant-block mosaics
  everywhere else. Videos preview as a scrubbable still frame; audio gets a
  waveform, metadata, and full playback with seek and speed controls.
- **Fast video previews from multi-GB files.** A frame from a 1.7 GB recording
  takes **1.45 s instead of 41.5 s** — placer reconstructs a sparse local copy
  from just the byte ranges ffmpeg needs (details below).
- **Selection that survives everything.** The selection is a manifest,
  independent of tab, filter, and directory — and it's persisted to disk, so a
  crash never loses curation work.
- **Batch transfer done right.** Worker pool, collision policies
  (skip/overwrite/rename), and local mtime set from the photo's actual capture
  time so pulls sort correctly in Finder.

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
linux/{amd64,arm64} — `make release` builds all four. Enable USB debugging on
the phone, plug it in, run `placer`.

## Usage

```sh
placer                     # the attached device
placer -s <serial>         # a specific device
placer -fake               # synthetic library, no device needed
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
`:set preview|autoplay|audio on|off`, `:set render quadrant|halfblock|auto`,
`:q` `:q!` `:wq`. In command mode `tab` completes the command and its
argument, and up/down walk this session's history.

### Previews

Previews render on cursor rest, debounced ~120 ms and cancelled the moment the
cursor moves. Everything funnels into one decode → downscale → render chain,
and every tier degrades to a metadata card rather than failing.

| Type | Approach |
|---|---|
| jpeg / png / gif | Go stdlib decode — 99.4% of a typical library |
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

Graphics-protocol previews are sized from the terminal's real pixels-per-cell,
queried via TIOCGWINSZ rather than assumed — a fixed guess is half the truth
on a HiDPI display, and the terminal then upscales the result into something
visibly soft. **If previews matter, run placer in Ghostty or kitty** — a real
image beats any block mosaic.

Rendered previews cache to `~/.cache/placer/thumbs/` (capped at 256 MiB), and
pulled audio caches to `~/.cache/placer/media/` so pressing `space` after a
preview is instant. Voice memos can run hundreds of MB, so the media cache is
capped at 2 GiB and pruned oldest-first at startup.

Playback shells out to `ffplay` (or `mpv` if present, `afplay` as a macOS
last resort) with the TUI owning the playhead — pause kills the process and
remembers the offset, seek kills and restarts with a new `-ss`. Crude, but it
needs no platform-specific code and no pure-Go decoder, which would have meant
CGO.

## Engineering notes

Everything here was measured against a real phone (a Pixel 6a with an
11,000-file library), and the numbers drove the design:

- **`adb pull` runs at 23–41 MB/s**; a 2.8 MB photo lands in ~120 ms. An
  earlier plan to fetch only the EXIF thumbnail from the first 128 KB was
  cut — it saved ~30 ms and returned a worse image.
- **Camera MP4s put `moov` at the end** — measured, not assumed. An 894 MB
  recording is `ftyp`(28 B) → `mdat`(894 MB) → `moov`(533 KB), so a naive
  frame grab means pulling the whole file. Instead placer `dd`s the head of
  `mdat` and the tail holding `moov`, writes them into a locally sparse file
  at their true offsets, and lets ffmpeg seek between them; the hole is never
  read. On a 1,735 MB / 10m56s recording: **41.5 s full pull → 1.45 s sparse,
  28.7× faster, byte-identical frame.**
- **Scrubbing generalises the same trick.** h/l fetch a third region — a
  window positioned from the bitrate around the seek point, backed off far
  enough to include the preceding keyframe — alongside the header (ffmpeg
  needs `ftyp` at offset 0 to identify the container at all) and the tail. A
  frame 10 minutes into that 1,735 MB recording takes 2.9 s.
- **The head is sized from the bitrate, not fixed.** A flat 4 MB head covers
  only 1.45 s of a 2.63 MB/s recording, which drops the decoder into the hole
  immediately after the wanted frame — the head covers `frameSeek + 2 s` of
  real video instead.

Three ADB details that cause silent corruption if ignored, all encoded in the
code and guarded by tests:

1. `adb shell` re-joins argv and the device re-splits it, so a whole
   `content query …` invocation must be **one quoted string**.
2. `adb shell` mangles `\n` → `\r\n`. Anything binary goes through
   **`adb exec-out`**.
3. `adb exec-out` folds *device* stderr into stdout, so any device-side
   command that chats — `dd` does — must redirect it **on the device**.
   Without `2>/dev/null` *on the device*, `dd` appends 78 bytes of
   "4+0 records in" to every read and the sparse reconstruction is offset
   garbage that still decodes.

And two terminal-graphics details that fail silently if ignored:

- **A multiplexer leaks the host terminal's identity but not its
  capabilities.** A pane launched from Ghostty reports
  `TERM_PROGRAM=ghostty`, so capability sniffing calls it kitty-capable — but
  many multiplexers don't forward APC graphics escapes, and the preview pane
  is simply blank with no error. placer checks for a multiplexer before
  trusting the environment, and `-render kitty` forces graphics for one that
  does pass them through.
- **A kitty graphics command carrying an image id gets acknowledged on
  stdin** — exactly where Bubble Tea reads keystrokes, so one APC reply lands
  in the key parser per repaint. Every kitty command placer emits is
  therefore response-free (no id, or `q=2`), which
  `TestEveryKittyCommandIsResponseFree` enforces.

## Development without a device

Every ADB interaction sits behind the `device.Device` interface, so the TUI,
indexer and transfer engine are fully testable with no phone attached:

```sh
make run                 # synthetic library shaped like a real phone
make test                # unit tests
make check               # vet + test + race + gofmt
make fixtures            # record real device output (device required)
PLACER_SNAPSHOT=1 go test ./internal/ui -run TestSnapshot -v   # eyeball screens
```

`internal/device/parse.go` is the highest-risk code in the project: `content
query` output is *not* safely comma-splittable, because display names and
album names contain `", "`. It parses on `, <key>=` boundaries built from the
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

## Roadmap

Next up: document previews (PDF and text), so the Docs tab gets the same
treatment as everything else.
