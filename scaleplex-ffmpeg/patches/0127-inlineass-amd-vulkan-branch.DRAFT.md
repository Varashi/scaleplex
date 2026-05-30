# 0127 — vf_inlineass AMD-Vulkan branch (design)

**Status:** DRAFT — design + code blocks below; unified-diff conversion + SKW-Build compile cycle pending. Tracks #123 phase 2.

## Why

AMD radeonsi has no `overlay_vaapi` (Intel iHD-only) and no `tonemap_vaapi`.
The Intel path's `vf_inlineass` VAAPI branch (patch 0115) consumes a VAAPI
surface and uses `overlay_vaapi` internally for the libass band blend; that
fails at init on radeonsi. The NVIDIA CUDA branch (patch 0126) is non-portable.

Tested AMD on Mesa 25.2.8 / RADV NAVI21 / jellyfin-ffmpeg 7.1.3 (RX 6800):
- `overlay_vulkan` → `VK_ERROR_DEVICE_LOST` + amdgpu GPU reset
- `libplacebo inputs=2` (multi-input filter mode) → libplacebo Vulkan backend
  asserts `vk_tex_barrier: !tex_vk->held`

Both fail at the driver/library plumbing level, not the filter-graph
descriptor. The recipe-tuning report explored 14 shapes; none worked.

The working blueprint is **mpv `vo_gpu_next`** (`video/out/vo_gpu_next.c:update_overlays()`),
which uses libplacebo's `pl_overlay` API directly via `pl_render_image()`
(single-image render with overlay parts attached). This is a **different
libplacebo code path** (`draw_overlays()` in `src/renderer.c`) than the
broken `inputs=2` filter front-door. mpv ships this on AMD RADV in
production today.

## Architecture

Add a third branch to `vf_inlineass` (alongside SW + HW-VAAPI-VPP):

```
incoming AVFrame format / device vendor
├── AV_PIX_FMT_VAAPI + Intel iHD       → HW-VAAPI-VPP branch (patch 0115)
├── AV_PIX_FMT_CUDA                    → CUDA branch (patch 0126)
├── AV_PIX_FMT_VAAPI + AMD radeonsi    → AMD-Vulkan branch (this patch)
└── SW pix fmt (nv12/yuv420p/...)      → SW libass+FFDraw branch (original)
```

Branch picked at `config_input_props()` time from the negotiated input
frame format + a one-shot VAAPI vendor probe (read libva vendor string from
`VADisplay` via `vaQueryVendorString()`; cache).

The AMD-Vulkan branch keeps the VAAPI surface format throughout — input and
output are both `AV_PIX_FMT_VAAPI`, same surface pool — so the downstream
`h264_vaapi`/`hevc_vaapi` encoder is happy. The Vulkan blend happens via
libplacebo's `pl_render_image()` with the source VAAPI surface mapped to a
`pl_frame` via `pl_map_avframe_ex()` (libplacebo handles the VAAPI →
DMA-BUF → Vulkan import transparently) and an `pl_overlay[]` carrying the
libass-rendered RGBA band.

The existing `inlineass_lock`-protected libass calls + the band staging
buffer are **shared** with the HW-VAAPI-VPP branch; only the upload-to-GPU
and blend dispatch are new.

## Files touched

1. `libavfilter/vf_inlineass.h` — branch state struct additions
2. `libavfilter/vf_inlineass.c` — branch init, per-frame, cleanup, dispatch
3. `libavfilter/Makefile` — pull in libplacebo + hwcontext_vulkan deps when
   `CONFIG_LIBPLACEBO_FILTER=yes` (the same guard vf_libplacebo uses; on
   when libplacebo + Vulkan SDK are present at configure time, which is
   true for the scaleplex-ffmpeg deps image since 0094-lineage builds with
   `--enable-libplacebo --enable-vulkan`).

## Branch state — added to `AssContext` (vf_inlineass.h)

```c
#if CONFIG_LIBPLACEBO_FILTER
    /* ---- HW (AMD-Vulkan) branch state ---------------------------------- *
     * Active when input is AV_PIX_FMT_VAAPI AND the VAAPI vendor probe
     * reports AMD (radeonsi). vf_inlineass owns a libplacebo Vulkan device
     * derived from the input filter's VAAPI device + a pl_renderer.
     * Per-frame: render libass band (shared with SW/VAAPI-VPP branches) →
     * upload to a cached pl_tex → attach as pl_overlay on the source
     * pl_frame → pl_render_image() blends in-place over the mapped VAAPI
     * surface. Output AVFrame is the same VAAPI surface (in-place blend).
     */
    AVBufferRef     *amd_vk_device_ref; /* derived AVHWDeviceContext (Vulkan) */
    pl_log           amd_pl_log;
    pl_vulkan        amd_pl_vk;         /* libplacebo Vulkan ctx (imports the AVF Vulkan device) */
    pl_renderer      amd_pl_rr;
    pl_tex           amd_overlay_tex;   /* cached RGBA band texture; recreated on size change */
    int              amd_overlay_w;     /* current band dims for cache invalidation */
    int              amd_overlay_h;
    int              amd_have_overlay;  /* 1 = amd_overlay_tex carries a visible band */
    /* Reuses the existing `cpu` (BGRA staging) field from the VAAPI-VPP
     * branch for libass render — same band, different upload destination. */
#endif /* CONFIG_LIBPLACEBO_FILTER */
```

## Branch detection — `config_input_props()` (vf_inlineass.c)

Add after the existing VAAPI-VPP gate, BEFORE it succeeds:

```c
if (inlink->format == AV_PIX_FMT_VAAPI) {
#if CONFIG_LIBPLACEBO_FILTER
    /* Probe VAAPI vendor once. Intel/Mesa iHD → existing VAAPI-VPP branch;
     * AMD/Mesa radeonsi → new Vulkan branch. */
    AVHWFramesContext *frames = (AVHWFramesContext *)inlink->hw_frames_ctx->data;
    AVHWDeviceContext *device = (AVHWDeviceContext *)frames->device_ref->data;
    AVVAAPIDeviceContext *vactx = device->hwctx;
    const char *vendor = vaQueryVendorString(vactx->display);
    if (vendor && strstr(vendor, "Mesa")) {
        /* Mesa = radeonsi (Mesa's libva backend on AMD). Could also be
         * Mesa-on-Intel via iris/Crocus, but Plex deploys on Arc which uses
         * iHD; if some operator picks Mesa-iris the AMD branch is still
         * format-correct, just unused on radeonsi-only filter calls. */
        s->amd_active = 1;
        return inlineass_amd_vk_config(avctx, inlink);
    }
#endif
    return inlineass_vaapi_vpp_config(avctx, inlink);  /* existing patch 0115 path */
}
```

## Init — `inlineass_amd_vk_config()` (new in vf_inlineass.c, gated)

```c
#if CONFIG_LIBPLACEBO_FILTER
#include <libplacebo/renderer.h>
#include <libplacebo/utils/libav.h>
#include <libavutil/hwcontext_vulkan.h>

static int inlineass_amd_vk_config(AVFilterContext *avctx, AVFilterLink *inlink)
{
    AssContext *s = avctx->priv;
    AVHWFramesContext *in_frames = (AVHWFramesContext *)inlink->hw_frames_ctx->data;
    int ret;

    /* Derive a Vulkan AVHWDeviceContext from the input VAAPI one. The
     * VAAPI→Vulkan derive on jellyfin-ffmpeg7 is broken (the same recipe-
     * tuning effort that proved overlay_vulkan unreliable); but the
     * DRM→Vulkan derive works and the input device's VAAPI display has a
     * DRM fd we can route through. ffmpeg's av_hwdevice_ctx_create_derived
     * with type=DRM→VULKAN handles this chain for us. */
    AVBufferRef *drm_ref = NULL;
    if ((ret = av_hwdevice_ctx_create_derived(&drm_ref, AV_HWDEVICE_TYPE_DRM,
                                              in_frames->device_ref, 0)) < 0) {
        av_log(avctx, AV_LOG_ERROR, "AMD-Vulkan branch: derive DRM device failed: %s\n", av_err2str(ret));
        return ret;
    }
    ret = av_hwdevice_ctx_create_derived(&s->amd_vk_device_ref, AV_HWDEVICE_TYPE_VULKAN,
                                         drm_ref, 0);
    av_buffer_unref(&drm_ref);
    if (ret < 0) {
        av_log(avctx, AV_LOG_ERROR, "AMD-Vulkan branch: derive Vulkan device failed: %s\n", av_err2str(ret));
        return ret;
    }

    /* Import the derived AVF Vulkan device into libplacebo. Pattern cribbed
     * from vf_libplacebo init_vulkan(); see PL_API_VER >= 278 path. */
    AVHWDeviceContext *vk_device = (AVHWDeviceContext *)s->amd_vk_device_ref->data;
    AVVulkanDeviceContext *vkctx = vk_device->hwctx;

    s->amd_pl_log = pl_log_create(PL_API_VER, pl_log_params(
        .log_cb = inlineass_amd_pl_log_cb,
        .log_priv = avctx,
        .log_level = PL_LOG_WARN,
    ));

    struct pl_vulkan_import_params import = {
        .instance        = vkctx->inst,
        .get_proc_addr   = vkctx->get_proc_addr,
        .phys_device     = vkctx->phys_dev,
        .device          = vkctx->act_dev,
        .extensions      = vkctx->enabled_dev_extensions,
        .num_extensions  = vkctx->nb_enabled_dev_extensions,
        .features        = &vkctx->device_features,
        .lock_queue      = inlineass_amd_lock_queue,    /* TODO: bridge to vkctx->lock_queue */
        .unlock_queue    = inlineass_amd_unlock_queue,
        .queue_ctx       = vk_device,
        .max_api_version = VK_API_VERSION_1_3,
        /* TODO: populate queue family indices from vkctx — vf_libplacebo
         * copies these from AVVulkanDeviceContext.queue_family_index etc.
         * See vf_libplacebo init_vulkan() for the full populate loop. */
    };
    s->amd_pl_vk = pl_vulkan_import(s->amd_pl_log, &import);
    if (!s->amd_pl_vk) {
        av_log(avctx, AV_LOG_ERROR, "AMD-Vulkan branch: pl_vulkan_import failed\n");
        return AVERROR(EIO);
    }
    s->amd_pl_rr = pl_renderer_create(s->amd_pl_log, s->amd_pl_vk->gpu);
    if (!s->amd_pl_rr) {
        av_log(avctx, AV_LOG_ERROR, "AMD-Vulkan branch: pl_renderer_create failed\n");
        return AVERROR(ENOMEM);
    }

    /* Output frames ctx = same shape as input (in-place blend). The existing
     * VAAPI-VPP branch's frames-ctx setup is overkill (it allocates a fresh
     * pool sized for the encoder); for AMD-Vulkan we pass through the input
     * frames ref unchanged. */
    outlink->hw_frames_ctx = av_buffer_ref(inlink->hw_frames_ctx);
    outlink->format        = AV_PIX_FMT_VAAPI;
    outlink->w             = inlink->w;
    outlink->h             = inlink->h;

    /* libass init (track + renderer) — exact same call sequence as the SW +
     * VAAPI-VPP branches; refactor the common init into a helper if cleaner. */
    /* ... (see existing inlineass_init_libass in patch 0115/SW path) ... */

    return 0;
}
#endif
```

## Per-frame — `inlineass_amd_vk_filter_frame()` (new)

```c
#if CONFIG_LIBPLACEBO_FILTER
static int inlineass_amd_vk_filter_frame(AVFilterLink *inlink, AVFrame *in)
{
    AVFilterContext *avctx = inlink->dst;
    AssContext      *s     = avctx->priv;
    AVFilterLink    *outlink = avctx->outputs[0];
    int ret;

    /* 1. Render the libass band (or feed a bitmap sub) to s->cpu — the BGRA
     *    premult staging buffer. Reuse render_ass_images() from patch 0115;
     *    same lock (inlineass_lock), same output. */
    pthread_mutex_lock(&inlineass_lock);
    ret = inlineass_render_band_to_cpu(avctx, in->pts);   /* sets s->have_overlay, s->ow/oh */
    pthread_mutex_unlock(&inlineass_lock);
    if (ret < 0)
        return ret;

    /* 2. If no visible cue this frame, pass-through unchanged. */
    if (!s->have_overlay) {
        return ff_filter_frame(outlink, in);
    }

    /* 3. Upload the BGRA band to a cached pl_tex. Mirrors mpv's
     *    update_overlays() — pl_tex_recreate handles the case where dims
     *    grew (e.g. larger cue) and dropping the old tex. */
    pl_fmt bgra_fmt = pl_find_named_fmt(s->amd_pl_vk->gpu, "bgra8");
    if (!pl_tex_recreate(s->amd_pl_vk->gpu, &s->amd_overlay_tex, &(struct pl_tex_params) {
        .format        = bgra_fmt,
        .w             = s->cpu->width,
        .h             = s->cpu->height,
        .host_writable = true,
        .sampleable    = true,
    })) {
        av_log(avctx, AV_LOG_ERROR, "pl_tex_recreate failed\n");
        return AVERROR(EIO);
    }
    if (!pl_tex_upload(s->amd_pl_vk->gpu, &(struct pl_tex_transfer_params) {
        .tex       = s->amd_overlay_tex,
        .row_pitch = s->cpu->linesize[0],
        .ptr       = s->cpu->data[0],
    })) {
        av_log(avctx, AV_LOG_ERROR, "pl_tex_upload failed\n");
        return AVERROR(EIO);
    }

    /* 4. Allocate an output VAAPI frame (in-place not possible — input may
     *    be referenced by the upstream decoder pool). Could be a no-op
     *    av_frame_ref if we're sure the input is unique; TODO: investigate
     *    if ff_get_video_buffer(outlink) is required. */
    AVFrame *out = av_frame_alloc();
    if (!out)
        return AVERROR(ENOMEM);
    out->format = AV_PIX_FMT_VAAPI;
    out->hw_frames_ctx = av_buffer_ref(in->hw_frames_ctx);
    if ((ret = av_hwframe_get_buffer(out->hw_frames_ctx, out, 0)) < 0) {
        av_frame_free(&out);
        return ret;
    }
    av_frame_copy_props(out, in);

    /* 5. Map src + target VAAPI surfaces as pl_frame via pl_map_avframe_ex.
     *    libplacebo handles VAAPI → DMA-BUF → Vulkan import transparently. */
    struct pl_frame pl_src = {0}, pl_tgt = {0};
    if (!pl_map_avframe_ex(s->amd_pl_vk->gpu, &pl_src, pl_avframe_params(.frame = in,  .tex = NULL))) {
        av_frame_free(&out);
        return AVERROR(EIO);
    }
    if (!pl_map_avframe_ex(s->amd_pl_vk->gpu, &pl_tgt, pl_avframe_params(.frame = out, .tex = NULL))) {
        pl_unmap_avframe(s->amd_pl_vk->gpu, &pl_src);
        av_frame_free(&out);
        return AVERROR(EIO);
    }

    /* 6. Attach the overlay band as a pl_overlay on the source frame.
     *    PL_OVERLAY_NORMAL = full-color BGRA, PL_ALPHA_PREMULTIPLIED matches
     *    the libass premult we render in render_ass_images. Single overlay
     *    part covering the full band, dst rect = same dims (no scaling). */
    struct pl_overlay_part part = {
        .src = { 0, 0, s->cpu->width, s->cpu->height },
        .dst = { 0, 0, s->cpu->width, s->cpu->height },
        .color = {1, 1, 1, 1},
    };
    struct pl_overlay ol = {
        .tex       = s->amd_overlay_tex,
        .mode      = PL_OVERLAY_NORMAL,
        .coords    = PL_OVERLAY_COORDS_DST_FRAME,
        .repr      = { .alpha = PL_ALPHA_PREMULTIPLIED },
        .color     = pl_color_space_srgb,
        .parts     = &part,
        .num_parts = 1,
    };
    pl_src.overlays     = &ol;
    pl_src.num_overlays = 1;

    /* 7. Render. This is THE call that dodges overlay_vulkan +
     *    libplacebo inputs=2 — pl_render_image() uses libplacebo's own
     *    draw_overlays() dispatch which is proven working on RADV via mpv. */
    bool ok = pl_render_image(s->amd_pl_rr, &pl_src, &pl_tgt,
                              &pl_render_default_params);

    /* 8. Unmap (releases the DMA-BUF handles) and forward. */
    pl_unmap_avframe(s->amd_pl_vk->gpu, &pl_src);
    pl_unmap_avframe(s->amd_pl_vk->gpu, &pl_tgt);
    av_frame_free(&in);

    if (!ok) {
        av_log(avctx, AV_LOG_ERROR, "pl_render_image failed\n");
        av_frame_free(&out);
        return AVERROR(EIO);
    }
    return ff_filter_frame(outlink, out);
}
#endif
```

## Cleanup — `uninit()` additions

```c
#if CONFIG_LIBPLACEBO_FILTER
    if (s->amd_overlay_tex) pl_tex_destroy(s->amd_pl_vk->gpu, &s->amd_overlay_tex);
    if (s->amd_pl_rr)       pl_renderer_destroy(&s->amd_pl_rr);
    if (s->amd_pl_vk)       pl_vulkan_destroy(&s->amd_pl_vk);
    if (s->amd_pl_log)      pl_log_destroy(&s->amd_pl_log);
    av_buffer_unref(&s->amd_vk_device_ref);
#endif
```

## Makefile

```makefile
# libavfilter/Makefile — add to the existing OBJS-$(CONFIG_INLINEASS_FILTER) line:
# (libplacebo + Vulkan are pulled in via vf_libplacebo's existing link deps
# when CONFIG_LIBPLACEBO_FILTER=yes; no new .o files, just header includes)
OBJS-$(CONFIG_INLINEASS_FILTER) += vf_inlineass.o
ifeq ($(CONFIG_LIBPLACEBO_FILTER), yes)
OBJS-$(CONFIG_INLINEASS_FILTER) += vf_inlineass_amd_vk.o  # (if split out)
endif
```

(Alternatively: keep the AMD-Vulkan code inline in vf_inlineass.c under
`#if CONFIG_LIBPLACEBO_FILTER` like patch 0126 does for CUDA — single OBJS
line stays. Cleaner; matches existing pattern.)

## TODOs that need on-hardware iteration

1. **Queue family bridging.** `vkctx` exposes `queue_family_index`,
   `queue_family_tx_index`, `queue_family_comp_index`. `pl_vulkan_import_params`
   takes per-family arrays. Need to translate. Crib from vf_libplacebo's
   `init_vulkan()` populate loop verbatim.
2. **`lock_queue`/`unlock_queue` bridging.** `vkctx->lock_queue` and
   `vkctx->unlock_queue` have a different signature than
   `pl_vulkan_import_params`. Need a thunk. vf_libplacebo has working ones.
3. **In-place vs new output frame.** Step 4 always allocates a new VAAPI
   surface. If `ff_filter_frame` upstream holds the only ref, we could
   blend in-place. Profile on the rig.
4. **`pl_map_avframe_ex` on VAAPI input.** Theoretically libplacebo handles
   the DMA-BUF chain transparently, but radeonsi's `vaExportSurfaceHandle`
   behavior + libplacebo's expectations need empirical confirmation.
   Mpv works on AMD, so the path exists; ffmpeg's wrapper layer may have a
   gap. If `pl_map_avframe_ex` fails on a VAAPI AVFrame, fall back to
   manual `vaExportSurfaceHandle` + `pl_vulkan_wrap` (the path libplacebo
   uses internally; we replicate it).
5. **Vendor probe order.** `vaQueryVendorString()` on radeonsi returns
   something like `"Mesa Gallium driver 25.2.8 for AMD Radeon RX 6800"` —
   the `"Mesa"` substring match is the simplest gate, but if Mesa-on-Intel
   (iris/crocus) is in the wild on someone's tester, it'd take the AMD
   path too. The Intel/AMD divergence on iris would need testing; in
   practice all scaleplex Intel deployments use iHD (Intel-binary), not
   Mesa, so this is safe. Defensive: probe `vendor_id` from sysfs as
   well (the existing scaleplex `detectVAAPIDriver` does this) — pass
   the resolved vendor in via env var if the fork-side probe is fragile.
6. **HDR sub colorspace.** For HDR-source sessions, the libass band should
   be flagged with the source's HDR metadata so the renderer maps it
   correctly. mpv does this conditionally
   (`pl_color_transfer_is_hdr(frame->color.transfer)` → set
   `ol->color.hdr.max_luma`). For Plex's argv shape we always tonemap
   BEFORE the burn (existing rewriter chain), so the band sits over an
   SDR surface — likely don't need the HDR path. Verify on live HDR clip.

## Build + validate workflow

1. Power-on SKW-Build VM (`govc vm.power -on /SKW/vm/Boeye.Net/SKW-Build`)
2. Apply the patch under `~/scaleplex/scaleplex-ffmpeg/patches/`,
   `DEB_BUILD_OPTIONS=nostrip ./build.sh`. ~5-10 min.
3. SCP the resulting `.deb` to the AMD test rig (172.16.10.19).
4. `docker cp` deb into the running `scaleplex-amd-worker` container,
   `dpkg -i` it, restart agent.
5. Manual ffmpeg test with the test clips: SDR sub-burn + HDR sub-burn.
6. Performance: target 5–6× realtime on 4K HDR + sub-burn (vs current 2.83×
   with the recipe-#14 hwdownload/hwupload roundtrip; pure-no-sub baseline
   is 6.41×).
7. Power-off SKW-Build (`govc vm.power -s`).
8. If green, convert this DRAFT.md to a unified-diff `.patch` file with
   real hunks against the in-tree `vf_inlineass.c` / `.h` / Makefile and
   land on the branch.

## Worker integration (separate, not in this patch)

After this fork patch lands, the rewriter's `composeBurn` for AMD vendor
sessions can emit the same `vf_inlineass` node it already does for Intel
(no AMD-specific compose path needed — the filter is format-adaptive).
That's a small Go change in `worker/agent/rewriter.go` to be done in a
follow-up PR once this filter is live.
