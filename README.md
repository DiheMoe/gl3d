# gl3d

Pure-Go MPEG Layer III (MP3) decoder — MPEG-1, MPEG-2 and MPEG-2.5, streaming, standard library only, MIT licensed.

## Installation

```sh
go get github.com/DiheMoe/gl3d
```

## Usage

```go
d, err := gl3d.NewDecoder(ctx, r)
if err != nil {
	return err
}
buf := make([]float64, 4096*d.Channels())
for {
	n, err := d.ReadPCM(ctx, buf)
	consume(buf[:n*d.Channels()])
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
}
```

### Borrowed PCM (zero-copy output)

Use `NextPCM` when the consumer can finish with each chunk before asking for the
next one. It returns a read-only view of the decoder's interleaved `float64` PCM,
without copying it into a caller-owned sample buffer:

```go
d, err := gl3d.NewDecoder(ctx, r)
if err != nil {
	return err
}
for {
	pcm, err := d.NextPCM(ctx)
	if err == io.EOF {
		break
	}
	if err != nil {
		return err
	}
	consume(pcm) // Finish synchronously; do not modify or retain pcm.
}
```

The view is valid only until the next `NextPCM` **or** `ReadPCM` call on the same
decoder. Its capacity equals its length. Copy the samples if they must be retained
or passed to an asynchronous consumer. A decoder must be used sequentially.

Both methods share gapless trimming, sample counts and terminal-error handling,
and may be interleaved. `len(pcm)/d.Channels()` is the number of sample frames in a
chunk. Both methods allocate zero memory during steady-state decoding after
initialization. The zero-copy guarantee applies to PCM delivery; decoding still
writes PCM, and compressed-input buffering and reservoir history still require
some copies. Frames with no reservoir backpointer borrow their current main-data
bytes directly.

### Format limitation

MPEG-2.5 at 8 kHz with a mixed short block (`block_type=2`,
`mixed_block_flag=1`) currently returns `UnsupportedError`. Its frequency-band
layout needs further validation. Ordinary short/long blocks at 8 kHz and mixed
blocks at other supported sample rates remain supported.

## Validation

```sh
go test ./...
go test -race -short ./...
cd tools
go vet ./...
go test ./...
```

PCM reference files are generated separately and are not stored in Git. Local
end-to-end tests skip absent references by default. Set `GL3D_REQUIRE_REFERENCE=1`
to make a missing reference fail the test. CI generates references for all 19 short
corpus entries with Ubuntu 24.04's FFmpeg 6.1 release line and requires every
comparison to run. This compatibility check is separate from the reference-tool
versions recorded in `corpus/manifest.json`; it does not certify those frozen
versions.

Run reproducible single-core benchmarks (input is preloaded; initialization is
included):

```sh
go test -run '^$' -bench 'Benchmark(Decode|NextPCM)' -benchmem -cpu=1 -count=5
```
