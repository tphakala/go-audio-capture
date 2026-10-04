# AGENTS.md

Orientation for coding agents and automated reviewers working on this repository. README.md is the user-facing reference (API examples, device id formats, benchmarks); this file is the map for changing and reviewing the code.

## What this is

`github.com/tphakala/go-audio-capture` (package `capture`) gives Go applications audio capture without cgo. It aims to:

1. **Build anywhere Go builds.** `CGO_ENABLED=0`, a static binary, no libasound, no C toolchain, no audio library to install on the target.
2. **Deliver exactly what the hardware captures.** The requested rate, channel count and sample format are negotiated with the device as is or `Open` fails with a typed error. No resampling, mixing or format conversion, so ultrasonic and high-rate capture stays intact.
3. **Survive the real world.** Consumers are typically long-running, unattended capture services. Every failure a device can produce (busy, unplugged, overrun, suspend, stall) is either recovered inside the library and counted, or surfaced as a specific typed error that tells the caller what to do next. A caller should never hang, never see an opaque errno, and never have to parse error strings.

It is capture-only. Post-processing (resampling, conversion, filtering) belongs in separate libraries.

| Platform | Backend | Status |
|---|---|---|
| Linux (LP64: amd64, arm64, riscv64, loong64; ILP32: 386, arm) | ALSA via raw kernel ioctls on `/dev/snd`, no libasound | implemented |
| Windows (amd64, arm64) | WASAPI exclusive mode via hand-rolled COM over `golang.org/x/sys/windows` | implemented |
| macOS | CoreAudio via purego | planned, not started |

Non-goals: playback, mobile, parity with general-purpose audio libraries, pro-audio latency.

### Hardware range

The target is any USB Audio Class capture device, from low-cost USB sound cards and USB microphones to studio audio interfaces, plus special hardware such as ultrasonic recorders. Devices differ a lot in what they accept: some offer one rate and one format only (the AudioMoth is S16 mono at 384 kHz only; the Scarlett Solo 4th Gen captures S32_LE 4-channel only), some reject period sizes that are not aligned the way the driver wants, some enumerate with no USB serial. Code must handle the narrow devices as well as the flexible ones, and must report an unsupported combination as a typed error rather than adapting to it.

Validated on real hardware so far: 384 kHz AudioMoth (ultrasonic), ZOOM AMS-24, Focusrite Scarlett Solo 4th Gen, and the `snd-aloop` loopback on Linux (amd64, arm64 on a Raspberry Pi 4, and 32-bit builds); Sound Blaster ZxR and Solid State Logic SSL 2 MkII on Windows.

### API stability

The public API is not stable before v1.0.0. Exported types, functions, struct fields and error types may change in any v0.x release when a cleaner design calls for it, and such a change ships as a minor version bump with a note in the release. Prefer the better API over compatibility: do not add deprecated aliases, compatibility shims or wrapper functions to keep an old signature working, and do not bend a design to avoid a breaking change. A changed signature still needs every caller in this repo updated, and a changed error type needs the failure-mode table, README and godoc updated with it.

The design rules below are not part of this: they are the library's contract and hold across every release.

## Design rules you must not break

These are the reason the library exists. A change that violates one is wrong even if tests pass.

1. **No silent conversion.** The requested rate, channel count and sample format are negotiated exactly or `Open` fails with a typed error (`*BadRateError`, `*BadFormatError`, `*ConfigError`). Never resample, up/down-mix, convert formats, or fall back to a "close enough" rate. `Stream.Negotiated` reports what the hardware actually agreed to. Buffering parameters (period size, period count) are not audio conversion and may be adjusted to what the driver accepts, as long as `Negotiated` reports the result.
2. **No userspace audio layers.** Linux is `hw:`-level only: no `plug`, `dsnoop`, `dmix`, `default`. Windows is exclusive mode only: no shared mode, never `AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM`.
3. **No cgo, ever.** Everything must build with `CGO_ENABLED=0`. The only runtime dependency is `golang.org/x/sys`. (`go-ruleguard/dsl` is lint-only, behind a build tag.)
4. **Typed, specific errors.** Errors name the failing ioctl or COM call and wrap errno/HRESULT. Map conditions to the sentinels in `errors.go` (`ErrClosed`, `ErrDeviceGone`, `ErrDeviceInUse`, `ErrDeviceStalled`, `ErrExclusiveNotAllowed`, `ErrCapabilitiesUnsupported`) so callers can use `errors.Is`/`errors.As`. Never surface an opaque "invalid argument".
5. **Stable device ids.** On Linux `DeviceInfo.ID` is derived from sysfs (USB serial, USB port, or `hw:CARD=<id>,DEV=<n>`), not the probe-order card index. `HWAddr` (`hw:N,D`) is display-only. A stable id is resolved on every `Open`/`SupportedRates*`/`Resolve` call, never cached, and re-verified after the device is opened. Ambiguity (two units with one serial) is an error, never a guess.
6. **ABI correctness over convenience.** `internal/alsa` mirrors `sound/asound.h`. Struct layouts and size-encoded ioctl numbers differ between LP64 and ILP32 and are pinned in layout tests. Unsupported GOARCHes (big-endian, PowerPC, MIPS) must fail to build via the `unsupported_GOARCH` sentinel in `abi_unsupported.go`, not compile with wrong numbers.
7. **Zero allocations in steady-state `Read`.** Both backends are allocation-free on the capture path; alloc tests guard this. Error paths may allocate.
8. **Concurrency contract.** `Read` is single-consumer and blocking. `Close` may be called from another goroutine and must unblock a parked `Read`, which then returns `ErrClosed`. A `Close` always wins over any other classification. No ioctl or COM call may run on a handle after it is closed.
9. **Never hang, never spin.** Every wait, retry and recovery loop is bounded or wakes on `Close`. A busy device fails `Open` at once rather than blocking until it is free.

## Failure modes: what the caller sees

This is the robustness contract. A change to any row is a behaviour change and needs a test through the relevant seam.

| Condition | Linux | Windows | What the caller should do |
|---|---|---|---|
| Malformed device id | `*BadDeviceError` | n/a (endpoint ids are opaque) | fix the configuration |
| Stable id matches no present device | `*DeviceNotFoundError` (unwraps to `ErrDeviceGone`) | `ErrDeviceGone` | wait for the device to reappear, then reopen |
| Two units report the same serial | `*AmbiguousDeviceError` | n/a | pin one with a listed id |
| Rate not supported | `*BadRateError` (with the supported range) | `*BadRateError` | pick a supported rate (`SupportedRates`) |
| Channel/format combination not supported | `*BadFormatError` | `*BadFormatError` | pick another format or channel count |
| Device held by another application | `ErrDeviceInUse` at once from `Open` and `SupportedRates*` | `ErrDeviceInUse` | retry later with backoff |
| Exclusive access disabled for the endpoint | n/a | `ErrExclusiveNotAllowed` | user changes the endpoint setting |
| Overrun (consumer too slow) | recovered inside `Read`, counted in `Xruns()` | counted in `Xruns()` | nothing; watch the counter |
| System suspend/resume | resumed or re-prepared inside `Read`, counted | n/a | nothing |
| Read stall (driver stops delivering, kernel read timeout `EIO`) | one restart inside `Read`, counted; a second stall returns `*StallError` (`ErrDeviceStalled`) | not detected yet | close and reopen |
| Recovery keeps failing with no frames (9th recoverable failure in one gap) | `*StallError` (`ErrDeviceStalled`) | n/a | close and reopen |
| Device unplugged mid-stream | `ErrDeviceGone`, including while `Read` is parked | `ErrDeviceGone` once `GetBuffer` sees the invalidation; a parked `Read` is not woken yet | close; wait for the device to reappear |
| `Close` from another goroutine | `ErrClosed` | `ErrClosed` | stop reading |

Any error returned by `Read` leaves the stream unusable; the caller must `Close` it. Platform gaps in the Windows column are tracked in GitHub issues and should not be copied as intended behaviour.

## Layout

```
capture.go            Public types: Format, ParseFormat, DeviceInfo, USBInfo, RateSupport, Config
errors.go             Sentinel errors and typed error structs (shared by all platforms)
doc.go                Package godoc (keep in sync with README when scope changes)

devices_linux.go      Devices(): parses /proc/asound, builds DeviceInfo (procRoot/sysRoot vars)
deviceid_linux.go     sysfs-derived stable ids, escaping, Resolve(), post-open re-verification
stream_linux.go       Open/Stream on Linux; drives the `pcm` interface (openPCM seam); Read's
                      recovery budget and terminalError classification
capabilities_linux.go SupportedRates (HW_REFINE only) and SupportedRatesVerified (HW_PARAMS probe)
capabilities_other.go Non-Linux stubs returning ErrCapabilitiesUnsupported

devices_windows.go    Devices() via WASAPI endpoint enumeration
stream_windows.go     Open/Stream on Windows (openDevice seam)

internal/alsa/        Kernel ABI: hwparams/swparams structs, ioctl numbers, PCM (OpenPCM,
                      Negotiate, Start, ReadI, Recover, Probe, Close), IsDeviceGone and
                      IsRecoverable errno sets, rate probing. abi_lp64.go / abi_ilp32.go pick
                      word-width types; abi_unsupported.go is the build guard.
internal/wasapi/      COM vtables (com.go), enumeration, IAudioClient setup and format
                      negotiation (client.go, format.go), HRESULT mapping (errors.go)

cmd/gac-rec/          Debug recorder for hardware validation (-list, -d, -r, -c, -f, -t, -o)
rules/rules.go        gocritic ruleguard matchers (build tag `ruleguard`, lint-only)
testdata/proc_asound/ Committed /proc/asound fixtures
```

Platform split is by filename suffix and build tags (`_linux.go`, `_windows.go`, `//go:build windows && (amd64 || arm64)`). The root package API is platform-neutral; when adding a public function, provide it on every platform (a stub returning a sentinel is fine, see `capabilities_other.go`).

## Linux kernel behaviour the code relies on

These explain code that otherwise looks odd. Check against `sound/core/pcm_native.c` and `pcm_lib.c` before changing the related code.

- A capture open without `O_NONBLOCK` sleeps while every substream is busy; with it the kernel returns `EBUSY`. `OpenPCM` always opens non-blocking and clears the flag with `fcntl` before `Negotiate`, because the read path takes its blocking mode from a copy of the flags that is refreshed only by `PREPARE`.
- A successful `RESUME` leaves the stream running, and `PREPARE` on a running stream fails with `EBUSY`, so `Recover` returns after a good resume.
- A read timeout (`EIO`) leaves the stream running, so stall recovery issues `DROP` before `PREPARE` and `START`.
- A reader parked in `READI_FRAMES` is woken with `EBADFD` when the PCM is disconnected; after the card disconnects every ioctl on the fd returns `ENODEV`. One `PVERSION` probe tells an unplug from an ordinary state error. `Close`'s own `DROP` also wakes a parked reader with `EBADFD`, which is why `s.closed` is checked first.
- The capture stop threshold is the buffer size, so an overrun stops the stream with `EPIPE` and is counted, instead of the hardware silently overwriting unread audio.

## Testing approach

Tests run without audio hardware. Each layer has an injection seam:

- `internal/alsa`: `PCM` holds an `ioctlFunc`; tests construct it with `newPCM(fd, fake)`. `fakeKernel` in `lifecycle_test.go` models the PCM state machine so a test cannot claim a recovery the kernel would refuse; `fakeRateDevice` and `fakeCommitDevice` (`rates_test.go`) cover negotiation. `sysOpen`, `sysSetNonblock` and `resumeSleep` are package seams for the open flags and the RESUME retry wait.
- Root package, Linux: package-level function vars `openPCM` (stream) and `openRatePCM` (capabilities) are swapped for fakes (`fakePCM` in `stream_linux_test.go`; lifecycle and recovery-budget cases in `stream_lifecycle_linux_test.go`).
- Device identity: `devicesFrom(procDir, sysDir)` takes roots; `deviceid_fixture_linux_test.go` builds throwaway `/proc/asound` and `/sys` trees with symlinks under `t.TempDir()` (sysfs symlinks cannot be committed).
- Windows: `openDevice` var in `stream_windows.go`; WASAPI fill and format logic are tested directly.
- Layout tests (`layout_lp64_test.go`, `layout_ilp32_test.go`) assert C-verified struct sizes, offsets and ioctl numbers. ILP32 assertions only execute under `GOARCH=386`.
- Hardware tests are opt-in: `GAC_HW_TEST=hw:1,0 go test -run TestHardwareSupportedRates -v`. They never run in CI.

New behaviour gets a test through the relevant seam. Bug fixes get a regression test that fails before the fix. A test must be able to fail: check that the assertion would break if the fixed line were reverted, and avoid fixtures where every path produces the same result.

Failure modes can be reproduced on real Linux hardware with `cmd/gac-rec`: hold a device with one `gac-rec` and open it with another (busy), write the USB port id to `/sys/bus/usb/drivers/usb/unbind` while capturing (unplug, then `bind` to restore), or `SIGSTOP` the recorder for longer than the buffer and `SIGCONT` it (overrun).

## Commands

Go 1.27, golangci-lint v2.14.0 (pinned in CI). Tasks are in `Taskfile.yml`.

The project targets the latest stable Go release and the latest tooling, not the oldest version that still works:

- `go.mod` and CI track the newest Go minor release. CI's `go-version: '1.27'` resolves to the newest patch on every run; when a new minor ships, bump `go.mod`, `GO_VERSION` in `.github/workflows/ci.yml` and the `go-version` in `govulncheck.yml` together.
- golangci-lint is pinned to its newest release in `GOLANGCI_LINT_VERSION` in `ci.yml` and in this file, and bumped by hand when a release ships (Dependabot does not see that string). Fix any new findings in the same change rather than disabling the linter.
- Dependabot keeps Go module dependencies and GitHub Actions current; merge its PRs once CI passes.
- New and changed code uses current language and standard library features where they make it simpler (`go fix -diff ./...`, also with `GOOS=windows`, lists older idioms the modernizers would replace). Do not add shims or build tags for older Go releases.

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
- Error strings are prefixed `capture:` in the root package, `alsa:` and `wasapi:` in the internal packages. Wrap with `%w`.
- Comments explain why (kernel or WASAPI behaviour, the failure being avoided), and name the ioctl, struct or COM call involved. Match the existing density.
- Errno and HRESULT sets that classify errors live in one place each (`alsa.IsDeviceGone`, `alsa.IsRecoverable`, `hresultError.Unwrap`); extend those rather than adding a second list.
- Behaviour that exists on one platform only is documented as such ("On Linux ...") in README, godoc and here.
- Adding a sample format touches: `Format` constants and `ParseFormat`/`String`/`BytesPerSample`/`IsFloat` in `capture.go`, the ALSA format mapping in `internal/alsa`, the WASAPI `SampleFormat` mapping in `internal/wasapi/format.go` (or an explicit `*ConfigError` rejection), `cmd/gac-rec` WAV header handling, tests on each side, and the README "Sample formats" section.
- Adding a Linux architecture means verifying the layout against `sound/asound.h` in C, adding the GOARCH to the `abi_*.go` build tags and layout test tags, and adding a cross-build to `Taskfile.yml` and CI.
- Commits follow Conventional Commits with a scope: `feat(alsa): ...`, `fix(wasapi): ...`, `docs: ...`.
- Text style: no em or en dashes anywhere (code, comments, docs, commit messages); use commas, colons, parentheses or a plain hyphen.

## Review guidance

For CodeRabbit, Copilot and human reviewers. Rate findings on what a caller of the library would see.

Flag these, they are real defects here:

- Any break of the design rules above, including a "helpful" fallback: a substituted rate, a converted format, a retry against `default`/`plug`, shared-mode WASAPI.
- A path where `Open`, `Start` or `Read` can block forever, or a retry or recovery loop with no bound and no `Close` wake-up.
- An ioctl or COM call that can run after `Close` has released the handle, or a `Close` that does not unblock a parked `Read`.
- A new error path that returns a raw errno, HRESULT or `fmt.Errorf` string where a sentinel or typed error from the table above applies, or an error mapped to the wrong sentinel (an unplug reported as a stall, a close reported as a device loss).
- An allocation on the steady-state `Read` path.
- An ioctl struct or number change without a matching layout test update for both LP64 and ILP32.
- A test that cannot fail on the behaviour it names, or a doc sentence that is false for some platform.

Intentional, do not flag:

- `unsafe.Pointer` conversions when passing structs to ioctls and COM vtables; layouts are pinned by the layout tests.
- Package-level function variables (`openPCM`, `openRatePCM`, `openDevice`, `sysOpen`, `sysSetNonblock`, `resumeSleep`) used as test seams.
- Ignored errors from best-effort cleanup (`_ = p.Close()` on an error path, `Close`'s `DROP`).
- Mutexes that are never held across a blocking syscall; `Close` coordinates through the in-flight count instead.
- Magic numbers in `internal/alsa` and `internal/wasapi` that mirror kernel or Windows headers.
- The absence of resampling, channel mixing or format conversion helpers.
- Typed errors that duplicate their internal counterparts (`alsa.BadRateError` and `capture.BadRateError`); the root package keeps callers from importing internal packages.
- Breaking changes to the exported API before v1.0.0 (see API stability), and the absence of deprecated aliases or shims for the old form.

## Things that do not belong in the repo

- Raw captures (`*.wav`, `*.raw`, `/testdata/captures/`): may contain LAN details. Commit only scrubbed fixtures under `testdata/`.
- Editor and agent state (`.serena/`, `.claude/`, `.codegraph/`) and build outputs (`*.test`, `*.exe`).
