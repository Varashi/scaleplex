package main

// DefaultMatrix is the axis cross-product the generator emits today.
//
// Faithful-fragment policy: a synthetic cell is only worth committing if
// it's an argv PMS would actually emit, otherwise replay tests the
// rewriter against argvs it will never see (false confidence or false
// failure). So synthesis is restricted to transforms of a real
// sanitized capture (templates/base__*.json) whose result stays a
// documented PMS form:
//
//   - StreamSpec  ordinal -> hex   `:#0xNN` is Plex's high-program-ID
//     selector syntax (confirmed in the organic corpus on m2ts / Plex
//     Versions); the #144 trigger combined it with dash + inlineass.
//   - DecodeCodec av1 / hevc       both decode through the same vaapi
//     hwaccel path; only the `-codec` value differs.
//   - EncodeCodec hevc_vaapi / h264_vaapi  output encoder swap.
//
// 2 × 2 × 2 = 8 cells, all on the external-sidecar-SRT + dash base.
//
// Deliberately NOT synthesized yet (no faithful fragment template — the
// organic corpus has no inlineass cell in these shapes to derive from):
//   - OutputFormat ssegment/segment (HLS) sub-burn tail
//   - SubSource embedded (muxed text/ASS, `-map 0:s:N`, single -i)
//   - HDR tonemap variants (opencl / vaapi tonemap node)
//   - NVENC / AMF cross-backend encoders
//
// Each needs a real captured fragment to graft; tracked as #150
// follow-ups. Adding one means dropping a faithful base capture under
// templates/ and a new axis here — not hand-writing argv.
func DefaultMatrix() []Shape {
	var out []Shape
	for _, spec := range []string{"ordinal", "hex"} {
		for _, dec := range []string{"av1", "hevc"} {
			for _, enc := range []string{"hevc_vaapi", "h264_vaapi"} {
				out = append(out, Shape{
					StreamSpec:  spec,
					DecodeCodec: dec,
					EncodeCodec: enc,
				})
			}
		}
	}
	return out
}
