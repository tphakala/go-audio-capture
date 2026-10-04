# AGENTS.md

Orientation for coding agents working on this repository. README.md is the user-facing reference (API examples, device id formats, benchmarks); this file is the map for changing the code.

## What this is

`github.com/tphakala/go-audio-capture` (package `capture`) is a pure-Go, cgo-free, capture-only audio library. It is the planned replacement for the malgo/miniaudio capture path in BirdNET-Go.

| Platform | Backend | Status |
|---|---|---|
| Linux (LP64: amd64, arm64, riscv64, loong64; ILP32: 386, arm) | ALSA via raw kernel ioctls on `/dev/snd`, no libasound | implemented |
| Windows (amd64, arm64) | WASAPI exclusive mode via hand-rolled COM over `golang.org/x/sys/windows` | implemented |
| macOS | CoreAudio via purego | planned, not started |

Non-goals: playback, mobile, miniaudio parity, pro-audio latency.

## Design rules you must not break

These are the reason the library exists. A change that violates one is wrong even if tests pass.

1. **No silent conversion.** The requested rate, channel count and sample format are negotiated exactly or `Open` fails with a typed error (`*BadRateError`, `*BadFormatError`, `*ConfigError`). Never resample, up/down-mix, convert formats, or fall back to a "close enough" rate. `Stream.Negotiated` reports what the hardware actually agreed to.
2. **No userspace audio layers.** Linux is `hw:`-level only: no `plug`, `dsnoop`, `dmix`, `default`. Windows is exclusive mode only: no shared mode, never `AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM`.
3. **No cgo, ever.** Everything must build with `CGO_ENABLED=0`. The only runtime dependency is `golang.org/x/sys`. (`go-ruleguard/dsl` is lint-only, behind a build tag.)
4. **Typed, specific errors.** Errors name the failing ioctl or COM call and wrap errno/HRESULT. Map conditions to the sentinels in `errors.go` (`ErrClosed`, `ErrDeviceGone`, `ErrDeviceInUse`, `ErrExclusiveNotAllowed`, `ErrCapabilitiesUnsupported`) so callers can use `errors.Is`/`errors.As`. Never surface an opaque "invalid argument".
5. **Stable device ids.** On Linux `DeviceInfo.ID` is derived from sysfs (USB serial, USB port, or `hw:CARD=<id>,DEV=<n>`), not the probe-order card index. `HWAddr` (`hw:N,D`) is display-only. A stable id is resolved on every `Open`/`SupportedRates*`/`Resolve` call, never cached, and re-verified after the device is opened. Ambiguity (two units with one serial) is an error, never a guess.
6. **ABI correctness over convenience.** `internal/alsa` mirrors `sound/asound.h`. Struct layouts and size-encoded ioctl numbers differ between LP64 and ILP32 and are pinned in layout tests. Unsupported GOARCHes (big-endian, PowerPC, MIPS) must fail to build via the `unsupported_GOARCH` sentinel in `abi_unsupported.go`, not compile with wrong numbers.
7. **Zero allocations in steady-state `Read`.** Both backends are allocation-free on the capture path; alloc tests guard this.
8. **Concurrency contract.** `Read` is single-consumer and blocking. `Close` may be called from another goroutine and must unblock a parked `Read`, which then returns `ErrClosed`. Xruns are recovered internally and counted in `Stream.Xruns()`; recovery is bounded per `Read`, and a stall or recovery loop returns `ErrDeviceStalled`.

## Layout

```
capture.go            Public types: Format, ParseFormat, DeviceInfo, USBInfo, RateSupport, Config
errors.go             Sentinel errors and typed error structs (shared by all platforms)
doc.go                Package godoc (keep in sync with README when scope changes)

devices_linux.go      Devices(): parses /proc/asound, builds DeviceInfo (procRoot/sysRoot vars)
deviceid_linux.go     sysfs-derived stable ids, escaping, Resolve(), post-open re-verification
stream_linux.go       Open/Stream on Linux; drives the `pcm` interface (openPCM seam)
capabilities_linux.go SupportedRates (HW_REFINE only) and SupportedRatesVerified (HW_PARAMS probe)
capabilities_other.go Non-Linux stubs returning ErrCapabilitiesUnsupported

devices_windows.go    Devices() via WASAPI endpoint enumeration
stream_windows.go     Open/Stream on Windows (openDevice seam)

internal/alsa/        Kernel ABI: hwparams/swparams structs, ioctl numbers, PCM (Negotiate,
                      Start, ReadI, Recover, Close), rate probing. abi_lp64.go / abi_ilp32.go
                      pick word-width types; abi_unsupported.go is the build guard.
internal/wasapi/      COM vtables (com.go), enumeration, IAudioClient setup and format
                      negotiation (client.go, format.go), HRESULT mapping (errors.go)

cmd/gac-rec/          Debug recorder for hardware validation (-list, -d, -r, -c, -f, -t, -o)
rules/rules.go        gocritic ruleguard matchers (build tag `ruleguard`, lint-only)
testdata/proc_asound/ Committed /proc/asound fixtures
```

Platform split is by filename suffix and build tags (`_linux.go`, `_windows.go`, `//go:build windows && (amd64 || arm64)`). The root package API is platform-neutral; when adding a public function, provide it on every platform (a stub returning a sentinel is fine, see `capabilities_other.go`).

## Testing approach

Tests run without audio hardware. Each layer has an injection seam:

- `internal/alsa`: `PCM` holds an `ioctlFunc`; tests construct it with `newPCM(fd, fake)` and fake devices such as `fakeRateDevice`, `fakeCommitDevice` (`rates_test.go`).
- Root package, Linux: package-level function vars `openPCM` (stream) and `openRatePCM` (capabilities) are swapped for fakes (`fakePCM` in `stream_linux_test.go`).
- Device identity: `devicesFrom(procDir, sysDir)` takes roots; `deviceid_fixture_linux_test.go` builds throwaway `/proc/asound` and `/sys` trees with symlinks under `t.TempDir()` (sysfs symlinks cannot be committed).
- Windows: `openDevice` var in `stream_windows.go`; WASAPI fill and format logic are tested directly.
- Layout tests (`layout_lp64_test.go`, `layout_ilp32_test.go`) assert C-verified struct sizes, offsets and ioctl numbers. ILP32 assertions only execute under `GOARCH=386`.
- Hardware tests are opt-in: `GAC_HW_TEST=hw:1,0 go test -run TestHardwareSupportedRates -v`. They never run in CI.

New behaviour gets a test through the relevant seam. Bug fixes get a regression test that fails before the fix.

## Commands

Go 1.27, golangci-lint v2.13.2 (pinned in CI). Tasks are in `Taskfile.yml`.

```
task check        # full local gate: cross-builds (amd64/arm64/arm/386/riscv64/loong64, CGO off),
                  # vet (4 arches), golangci-lint, gofmt, race tests, GOARCH=386 test run
task test:race    # go test -race ./...
task test:ilp32   # whole suite as 386 binaries (needs 32-bit exec on an x86_64 host)
task lint
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...   # Windows cross-compile check
GOOS=windows golangci-lint run ./...                     # lint Windows-tagged files
```

Run `task check` before pushing. Linux-only tooling does not compile or lint the `_windows.go` files, so after touching anything Windows-side also run the Windows build and lint above. CI additionally verifies that `s390x` and `ppc64le` builds fail at the guard sentinel.

## Conventions

- Lint config is `.golangci.yaml` (gocritic with ruleguard, revive, errorlint, exhaustive with `default` counting as exhaustive, gocognit 50). Use `errors.New` for constant messages, not `fmt.Errorf`.
- Error strings are prefixed `capture:` in the root package. Wrap with `%w`.
- Comments explain why (kernel or WASAPI behaviour, the failure being avoided), and name the ioctl, struct or COM call involved. Match the existing density.
- Adding a sample format touches: `Format` constants and `ParseFormat`/`String`/`BytesPerSample`/`IsFloat` in `capture.go`, the ALSA format mapping in `internal/alsa`, the WASAPI `SampleFormat` mapping in `internal/wasapi/format.go` (or an explicit `*ConfigError` rejection), `cmd/gac-rec` WAV header handling, tests on each side, and the README "Sample formats" section.
- Adding a Linux architecture means verifying the layout against `sound/asound.h` in C, adding the GOARCH to the `abi_*.go` build tags and layout test tags, and adding a cross-build to `Taskfile.yml` and CI.
- Commits follow Conventional Commits with a scope: `feat(alsa): ...`, `fix(wasapi): ...`, `docs: ...`.
- Text style: no em or en dashes anywhere (code, comments, docs, commit messages); use commas, colons, parentheses or a plain hyphen.

## Things that do not belong in the repo

- Design docs, plans and specs: `/docs/` is gitignored and kept private. Do not commit planning documents anywhere in the tree.
- Raw captures (`*.wav`, `*.raw`, `/testdata/captures/`): may contain LAN details. Commit only scrubbed fixtures under `testdata/`.
- Editor and agent state (`.serena/`, `.claude/`, `.codegraph/`) and build outputs (`*.test`, `*.exe`).
