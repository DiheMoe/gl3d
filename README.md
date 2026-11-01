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
