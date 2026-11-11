# gl3d

Pure-Go MPEG Layer III (MP3) decoder — MPEG-1, MPEG-2 and MPEG-2.5, streaming, standard library only, MIT licensed.

## Installation

```sh
go get github.com/DiheMoe/gl3d
```

gl3d requires Go 1.27 or later.

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

`NewDecoder` reads from any `io.Reader` and never seeks. `ReadPCM` writes interleaved `float64` samples at full scale 1.0 and returns the number of sample frames it wrote. Samples are not clipped and may exceed ±1. `SampleRate` and `Channels` report the stream's own sample rate and channel count, because gl3d does not resample or remix.

## Input and decoding modes

Decoding is lenient by default, so problems in the stream are skipped or concealed. With `gl3d.Strict()` they are errors instead. The table lists what each mode does with each kind of input.

| Input | Lenient (default) | Strict |
|---|---|---|
| Layer III of MPEG-1 (32, 44.1, 48 kHz), MPEG-2 (16, 22.05, 24 kHz) and MPEG-2.5 (8, 11.025, 12 kHz) | Decoded | Decoded |
| All Layer III bit rates. CBR, VBR and ABR. Bit rate and padding may change from frame to frame | Decoded | Decoded |
| Mono, stereo, joint stereo (MS and intensity) and dual channel | Decoded | Decoded |
| Long, short, start, stop and mixed blocks. Bit reservoir across frames | Decoded | Decoded |
| Frames with or without CRC | Decoded | Decoded |
| ID3v2.2, 2.3 and 2.4 tags at the start | Skipped | Skipped |
| ID3v1, TAG+, APEv1/v2, Lyrics3 v1/v2 and ID3v2 tags after the audio | Skipped and listed in `Report.TrailingTags` | Same |
| Xing/Info or VBRI header in the first frame | Frame count read. The frame is not decoded as audio | Same |
| LAME tag, including the one FFmpeg writes | Encoder delay trimmed from the start. Padding trimmed from the end when the frame count is known | Same |
| LAME tag whose CRC does not match | Frame count and LAME fields ignored, so the output keeps the encoder delay and padding. FFmpeg and mpg123 still trim them | Same |
| VBRI delay field and iTunSMPB | Ignored | Ignored |
| Up to 64 KiB of non-tag data before the first frame | Skipped | Skipped |
| Stream starting mid-way, as cuts made without re-encoding do | Granules whose main data begins before the start decode as silence, so the length is kept | Same |
| Empty input, only tags, or less than 64 KiB of other data and no frame | `ErrNoAudio` | `ErrNoAudio` |
| No frame within the first 64 KiB of non-tag data | `LimitError` | `LimitError` |
| Layer I, Layer II or free format | `UnsupportedError` when the first frame is one. Later frames skipped | `UnsupportedError` |
| Non-zero emphasis | Decoded without de-emphasis, as FFmpeg does. The first frame's value is in `Report.Emphasis` | `UnsupportedError` |
| Change of MPEG version, sample rate or channel count mid-stream | Frames whose format differs from the first frame skipped | `UnsupportedError` |
| Junk between frames or after the audio, including damaged frame headers and ICY metadata blocks | Skipped. Decoding resumes at the next frame or tag | `CorruptError` |
| MP4, MPEG-TS, WAV and other containers | Not parsed. Container bytes count as data before the first frame or as junk | Same |
| Tags between frames, including the Xing/Info/VBRI frames of concatenated files | Skipped | `CorruptError` |
| Frame whose CRC does not match | Decoded anyway | `CorruptError` |
| Invalid side information, or `part2_3_length` adding up to more bits than the main data holds | Frame dropped | `CorruptError` |
| Damaged scalefactor or Huffman data | Spectral lines from the damage onwards zeroed in that granule and channel | `CorruptError` |
| Frame with bytes missing, so that the next frame starts before its declared end | Frame dropped. Decoding resumes at the next frame | `CorruptError` |
| Data ending inside a frame, a frame header or a tag | Incomplete part dropped | `TruncatedError` |
| Decoded frame count different from the Xing/VBRI frame count | Decoded as usual | `TruncatedError` at the end |
| LAME padding longer than the audio left after the start trim | No end trim | No end trim, `CorruptError` at the end |
| Malformed Xing/Info/VBRI header in the first frame | Frame skipped and its fields ignored | `CorruptError` |
| ID3v2 header whose version or revision byte is 0xFF | Treated as junk | `CorruptError` |
| ID3v2 header with an unknown major version or undefined flags | Tag skipped by its size | `CorruptError` |
| Malformed APE or Lyrics3 tag | Skipped as junk | `CorruptError` |
| Tags larger than 16 MiB in total | Skipped | `LimitError` |

Where only strict mode returns an error, lenient mode records the problem in `Decoder.Report` and `Report.Clean` returns false. When lenient decoding ends normally and `Report.Clean` returns true, strict decoding of the same stream succeeds with identical output. Lenient mode returns `ErrNoAudio` when it has dropped, skipped or cut off data and decoded no frame.

Strict mode returns problems found while opening the stream from `NewDecoder` and the rest from `ReadPCM`. Opening reads the leading tags, the first frame header and the Xing/Info/VBRI header. gl3d skips tags without reading their contents. The package documentation describes the full error contract.

## Verification status

- Intensity stereo (MPEG-1 and LSF) is covered only by unit tests and constructed streams. Neither the test corpus nor the ISO 11172-4 compliance streams contain it.
- LSF intensity positions and gains follow ISO 13818-3. Two cases that the standards leave open follow FFmpeg and mpg123. One is the position of the top scalefactor band when the band below it is not intensity coded. The other is a separate intensity bound for each short window in MPEG-1.
- The scalefactor band widths for 16, 22.05 and 24 kHz, and the LSF scalefactor lengths and partitions, match ISO 13818-3.

## License

MIT. See [LICENSE.txt](LICENSE.txt).
