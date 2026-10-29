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

## Performance

The hot-path optimization shares Huffman prefix tables, caches exact quarter-step
dequantization gains, synthesizes directly into destination PCM, and avoids copying
current-frame reservoir data when there is no backpointer.

The `pcmdiff` tool compares PCM streams using fixed-size blocks rather than loading
both files into memory. This changes memory growth from proportional to audio duration
to a fixed buffer plus per-channel statistics.

To compare decode throughput on the same host, Go version and corpus:

```sh
go test -run '^$' -bench '^BenchmarkDecode$' -benchmem -cpu=1 -count=5
```

## Format limitation

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

PCM reference files are generated separately and are not stored in Git. End-to-end
tests skip absent references by default; `GL3D_REQUIRE_REFERENCE=1` makes a
missing reference fail. Reference-tool versions are recorded in
`corpus/manifest.json`.
