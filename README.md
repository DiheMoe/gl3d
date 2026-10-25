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
