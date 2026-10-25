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
some copies.

## Benchmarking

```sh
go test -run '^$' -bench 'Benchmark(Decode|NextPCM)' -benchmem -cpu=1 -count=5
```
