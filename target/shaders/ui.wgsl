// ui-rhi — uber-shader for UI quads and atlas text.
//
// Phase 1 (CSS-parity): per-instance state is split between a slim
// `QuadInstance` (geometry + atlas UV + paint id) and a per-frame
// `PaintRecord` SSBO (fill / gradient / stroke / tier-1 effects). One
// pipeline serves every tier-1 UI effect — see
// `docs/ui-shader-architecture.md` for the hybrid-pipeline rationale.
// Per-instance `shader_id` selects the top-level coverage path:
//
//   0 = SDF rounded rect. Paint flags further select per-draw:
//         bit 0 (PAINT_FLAG_FILL_GRADIENT) — linear gradient between
//                 `color_a` and `color_b` along `gradient_dir`.
//         bit 1 (PAINT_FLAG_STROKE) — paint `stroke` at the SDF edge.
//   1 = alpha atlas sample × `color_a` (glyph quads from the text layer).
//
// Paint state (colour, stroke, scanline / noise / roll-bar tunables)
// lives in `PaintRecord` so the per-instance buffer stays small as the
// effect surface grows. Records are content-addressed CPU-side: many
// glyphs from the same run share one record id.

struct Push {
    // Logical-px extent of the current pass's render target.
    // Root pass = swapchain logical size; save-layer transient = its
    // bbox extent (P3.4a step 2 — see docs/ui-css-parity-phase3-
    // offscreen-step2.md).
    screen_size: vec2<f32>,
    // Logical-px top-left of the current pass's bbox. Subtracted from
    // each instance's `pos` before NDC mapping so save-layer
    // instances at full-screen coords land at `(0, 0)` of their
    // transient. Root pass = `(0, 0)` — identity.
    pass_origin: vec2<f32>,
    // Wall-clock time in seconds (forwarded by `UiPainter::prepare`).
    // Drives animated scanline scroll + per-frame noise reshuffle.
    time: f32,
    // `device_pixel_ratio * ui_scale`. Converts physical `FragCoord.y`
    // back to logical-px for the scanline period math — keeps the
    // pattern visually consistent on 1080p / HiDPI / 4K.
    effective_scale: f32,
};
var<immediate> push: Push;

// 64-byte instance, all-scalar fields. Geometry + atlas UV +
// paint-table indirection + block bbox (run-wide gradient UV).
// Layout mirrors `QuadInstance` in `lib.rs`. The block_origin /
// block_extent fields drive the gradient-reference rectangle: rect
// quads pass `(pos, size)`; glyph quads pass the whole text run's
// bounding box so a CSS `background-clip: text` gradient spans the
// entire heading.
struct QuadInstance {
    pos_x: f32,
    pos_y: f32,
    size_x: f32,
    size_y: f32,
    uv_x: f32,
    uv_y: f32,
    uv_w: f32,
    uv_h: f32,
    shader_id: u32,
    paint_id: u32,
    radius: f32,
    /// Convex-polygon clip handle (Phase 3.1). `0` = no clip; otherwise
    /// indexes into the per-frame `clips` SSBO for a half-plane SDF mask.
    clip_id: u32,
    block_origin_x: f32,
    block_origin_y: f32,
    block_extent_x: f32,
    block_extent_y: f32,
};

/// Convex-polygon clip record. Each edge is `(nx, ny, c, _pad)` where
/// `(nx, ny)` is the outward unit normal and `c = -dot(normal, vertex)`.
/// A pixel `p` is inside iff `nx*p.x + ny*p.y + c <= 0` for every edge
/// `i ∈ 0..count`. Up to `CLIP_MAX_EDGES` (8) edges. Layout mirrors
/// `ClipRecord` in `crates/ui-rt/src/paint.rs`.
struct ClipRecord {
    count: u32,
    _pad0: u32,
    _pad1: u32,
    _pad2: u32,
    edges: array<vec4<f32>, 8>,
};

// 112-byte paint record, all-scalar fields. Mirrors `PaintRecord` in
// `crates/ui-rt/src/paint.rs`. `flags` packs the `PAINT_FLAG_*` bits
// (1=stroke, 2=fill-gradient, 4=scanline, 8=noise, 16=roll-bar; see
// the constants in paint.rs). Trailing `_pad_composite[3]` keeps
// the std430 array stride at 112 (16-byte aligned for the vec4
// members) and reserves room for future composite parameters.
struct PaintRecord {
    kind: u32,
    flags: u32,
    color_a_r: f32,
    color_a_g: f32,
    color_a_b: f32,
    color_a_a: f32,
    color_b_r: f32,
    color_b_g: f32,
    color_b_b: f32,
    color_b_a: f32,
    stroke_r: f32,
    stroke_g: f32,
    stroke_b: f32,
    stroke_a: f32,
    stroke_width: f32,
    /// Per-side stroke mask. CSS-order TRBL bits: 1 = top, 2 = right,
    /// 4 = bottom, 8 = left. `0xF` = all four sides.
    stroke_sides: u32,
    gradient_dir_x: f32,
    gradient_dir_y: f32,
    radial_radius_or_sweep_start: f32,
    scanline_freq: f32,
    scanline_intensity: f32,
    noise_intensity: f32,
    roll_period_s: f32,
    roll_intensity: f32,
    /// Bindless transient index — picks the slot in
    /// `transient_tex[16]` that a composite quad samples. Unused on
    /// non-composite paints (set to 0; the default 1×1-black slot).
    transient_idx: u32,
    // Reserved composite slots, reinterpreted as arc parameters when the
    // paint carries `PAINT_FLAG_ARC` (bit 8). Same 12 bytes / offsets as
    // the former `_pad_composite[3]`; the CPU packs `f32` bits here.
    arc_inner_radius: f32,
    arc_angle_start: f32,
    arc_angle_sweep: f32,
};

@group(0) @binding(0) var<storage, read> instances: array<QuadInstance>;
@group(0) @binding(1) var<storage, read> paints: array<PaintRecord>;
@group(0) @binding(2) var<storage, read> clips: array<ClipRecord>;
@group(1) @binding(0) var atlas_tex: texture_2d<f32>;
@group(1) @binding(1) var atlas_sampler: sampler;
@group(2) @binding(0) var msdf_tex: texture_2d<f32>;
@group(2) @binding(1) var msdf_sampler: sampler;

// Composite-source binding at @group(3). One of the two variants
// below is active per build of this shader, picked by
// `build_shader_source` from `CompositeMode`. The host-side bind
// group layout matches the active variant.
//
// - `Bindless`: 16-entry texture array, indexed per-fragment via
//   `paint.transient_idx`. Single bind group covers all save-layers
//   in the frame; one draw call samples any slot.
// - `PerGroup`: single sampled texture. One bind group per
//   composite source, switched at draw time. No per-fragment
//   indexing — `paint.transient_idx` is unused.
@group(3) @binding(0) var transient_tex: texture_2d<f32>;
@group(3) @binding(1) var transient_sampler: sampler;

struct V2F {
    @builtin(position) clip_pos: vec4<f32>,
    @location(0) color: vec4<f32>,
    @location(1) local_pos: vec2<f32>,
    @location(2) half_size: vec2<f32>,
    @location(3) atlas_uv: vec2<f32>,
    @location(4) @interpolate(flat) shader_id: u32,
    @location(5) @interpolate(flat) radius: f32,
    @location(6) @interpolate(flat) feature_flags: u32,
    @location(7) @interpolate(flat) color_b: vec4<f32>,
    @location(8) @interpolate(flat) stroke_color: vec4<f32>,
    @location(9) @interpolate(flat) stroke_width: f32,
    @location(10) @interpolate(flat) gradient_dir: vec2<f32>,
    /// Normalised rect-interior coords, `[0, 0]` at top-left to
    /// `[1, 1]` at bottom-right. Linearly interpolated across the quad.
    /// Used by the gradient path — division-free, and independent of
    /// the (also-interpolated) `half_size` varying.
    @location(11) local_uv: vec2<f32>,
    /// Packed tier-1 effect scalars, flat-interpolated:
    ///   x = scanline_freq,  y = scanline_intensity,
    ///   z = noise_intensity, w = reserved.
    /// Pack to one location to stay well under `maxFragmentInputLocations`.
    @location(12) @interpolate(flat) effects: vec4<f32>,
    /// Roll-bar: x = period_seconds, y = intensity, z+w reserved.
    @location(13) @interpolate(flat) roll: vec4<f32>,
    /// Run-wide gradient UV (`[0, 0]` at the gradient-reference
    /// rect's top-left, `[1, 1]` at its bottom-right). For rect quads
    /// equals `local_uv`; for glyph quads spans the whole text run so
    /// a CSS `background-clip: text` linear gradient flows
    /// continuously across glyphs.
    @location(14) block_uv: vec2<f32>,
    /// MSDF shadow expansion radius (normalised distance, `0..0.5`).
    /// Read from `paints[paint_id].radial_radius_or_sweep_start`,
    /// which is overloaded with `radial_radius` / `sweep_start` for
    /// gradient brushes. Mutually exclusive: text shadows don't use
    /// gradients. Zero for non-shadow paints (no effect).
    @location(15) @interpolate(flat) shadow_radius: f32,
    /// Convex-polygon clip id (Phase 3.1). Indexes into `clips[]`; `0`
    /// is the identity record (no mask). Sampled in fragment so the
    /// per-pixel local position can be tested against half-planes.
    @location(16) @interpolate(flat) clip_id: u32,
    /// Per-side stroke mask (TRBL bits) — see `BorderSides`. Zero
    /// disables stroke entirely; `0xF` strokes all four sides.
    @location(17) @interpolate(flat) stroke_sides: u32,
    /// Bindless transient slot for `shader_id == 3` composite quads.
    /// Read in fragment as `transient_tex[transient_idx]`. Zero on
    /// non-composite paints (the default 1×1 black slot).
    @location(18) @interpolate(flat) transient_idx: u32,
    /// Arc / annulus parameters (`FEAT_ARC`, bit 8) — or, aliased, the
    /// capsule parameters for `FEAT_LINE` (bit 9): `half_delta.x`,
    /// `half_delta.y`, `thickness`. Read from the paint record's reserved
    /// composite slots. For arc: outer radius is `half_size.x`; these
    /// carry the hole radius (logical px) and the start / sweep angles
    /// (radians, sweep signed). Ignored unless one of the flags is set.
    @location(19) @interpolate(flat) arc_inner_radius: f32,
    @location(20) @interpolate(flat) arc_angle_start: f32,
    @location(21) @interpolate(flat) arc_angle_sweep: f32,
};

/// sRGB (gamma) → linear. UI colours are authored as sRGB (CSS hex /
/// 255), but this pipeline composites in linear and applies the sRGB
/// OETF on output — hardware on an `_SRGB` swap chain, the manual
/// `#SRGB_OUT` block on a UNORM target, or the engine's present pass for
/// a float scene target. Decoding at the single point colours enter the
/// shader is what makes an authored `#7dd3fc` display as exactly
/// `#7dd3fc`, and makes translucent compositing physically correct
/// (linear-space alpha blend). Alpha is coverage, not gamma-encoded, so
/// callers pass only `.rgb` and keep alpha untouched. This is colour
/// correctness, not tone mapping (UI has no tone mapping — it is not a
/// physically-based luminance signal).
fn srgb_to_linear(c: vec3<f32>) -> vec3<f32> {
    let lo = c / 12.92;
    let hi = pow((c + vec3<f32>(0.055)) / 1.055, vec3<f32>(2.4));
    return select(hi, lo, c <= vec3<f32>(0.04045));
}

@vertex
fn vs(
    @builtin(vertex_index) vid: u32,
    @builtin(instance_index) iid: u32,
) -> V2F {
    // Tri-list quad, 6 vertices: TL, BL, TR, TR, BL, BR.
    let ux = f32((vid == 2u) || (vid == 3u) || (vid == 5u));
    let uy = f32((vid == 1u) || (vid == 4u) || (vid == 5u));
    let local_uv = vec2<f32>(ux, uy);

    // Field-direct reads — see egui-rhi shader comment for the spirv-val
    // reason against `let inst = instances[iid];`.
    let pos = vec2<f32>(instances[iid].pos_x, instances[iid].pos_y);
    let size = vec2<f32>(instances[iid].size_x, instances[iid].size_y);
    let uv_base = vec2<f32>(instances[iid].uv_x, instances[iid].uv_y);
    let uv_extent = vec2<f32>(instances[iid].uv_w, instances[iid].uv_h);
    let shader_id = instances[iid].shader_id;
    let paint_id = instances[iid].paint_id;
    let radius = instances[iid].radius;
    let clip_id = instances[iid].clip_id;
    let block_origin = vec2<f32>(
        instances[iid].block_origin_x,
        instances[iid].block_origin_y,
    );
    let block_extent = vec2<f32>(
        instances[iid].block_extent_x,
        instances[iid].block_extent_y,
    );

    // Linearize authored sRGB colours once, here, before interpolation /
    // compositing (see `srgb_to_linear`). Alpha stays as-is.
    let color = vec4<f32>(
        srgb_to_linear(vec3<f32>(
            paints[paint_id].color_a_r,
            paints[paint_id].color_a_g,
            paints[paint_id].color_a_b,
        )),
        paints[paint_id].color_a_a,
    );
    let color_b = vec4<f32>(
        srgb_to_linear(vec3<f32>(
            paints[paint_id].color_b_r,
            paints[paint_id].color_b_g,
            paints[paint_id].color_b_b,
        )),
        paints[paint_id].color_b_a,
    );
    let stroke_color = vec4<f32>(
        srgb_to_linear(vec3<f32>(
            paints[paint_id].stroke_r,
            paints[paint_id].stroke_g,
            paints[paint_id].stroke_b,
        )),
        paints[paint_id].stroke_a,
    );
    let feature_flags = paints[paint_id].flags;
    let stroke_width = paints[paint_id].stroke_width;
    let stroke_sides = paints[paint_id].stroke_sides;
    let transient_idx = paints[paint_id].transient_idx;
    // Gradient direction is precomputed CPU-side (`cos/sin` once per
    // record, not per-vertex).
    let gradient_dir = vec2<f32>(
        paints[paint_id].gradient_dir_x,
        paints[paint_id].gradient_dir_y,
    );
    let effects = vec4<f32>(
        paints[paint_id].scanline_freq,
        paints[paint_id].scanline_intensity,
        paints[paint_id].noise_intensity,
        0.0,
    );
    let roll = vec4<f32>(
        paints[paint_id].roll_period_s,
        paints[paint_id].roll_intensity,
        0.0,
        0.0,
    );
    let shadow_radius = paints[paint_id].radial_radius_or_sweep_start;
    let arc_inner_radius = paints[paint_id].arc_inner_radius;
    let arc_angle_start = paints[paint_id].arc_angle_start;
    let arc_angle_sweep = paints[paint_id].arc_angle_sweep;

    // `pass_origin` is `(0, 0)` for the root pass (identity); for
    // save-layer passes it's the bbox top-left, so an instance at
    // logical `(pos.x, pos.y) = (200, 200)` rendering into a
    // transient whose bbox starts at `(150, 180)` lands at
    // pass-local `(50, 20)` and maps to clip space against the
    // bbox extent.
    let pixel = pos - push.pass_origin + local_uv * size;
    let clip = vec2<f32>(
        pixel.x / push.screen_size.x * 2.0 - 1.0,
        pixel.y / push.screen_size.y * 2.0 - 1.0,
    );

    let local_pos = (local_uv - vec2<f32>(0.5)) * size;
    let half_size = size * 0.5;
    let atlas_uv = uv_base + local_uv * uv_extent;

    // Run-wide gradient UV. For rects: block == (pos, size) so this
    // collapses to local_uv. For glyph quads: block is the whole
    // text run's bbox, so a heading like "NEON RIDER" carries one
    // continuous gradient across all its glyphs.
    let block_uv = (pixel - block_origin) / block_extent;

    return V2F(
        vec4<f32>(clip, 0.0, 1.0),
        color,
        local_pos,
        half_size,
        atlas_uv,
        shader_id,
        radius,
        feature_flags,
        color_b,
        stroke_color,
        stroke_width,
        gradient_dir,
        local_uv,
        effects,
        roll,
        block_uv,
        shadow_radius,
        clip_id,
        stroke_sides,
        transient_idx,
        arc_inner_radius,
        arc_angle_start,
        arc_angle_sweep,
    );
}

/// Convex-polygon clip mask. Returns a `[0, 1]` coverage where `1` is
/// fully inside every half-plane and `0` fully outside any one. AA
/// width is one screen pixel via `fwidth` on the worst (max) signed
/// distance — keeps edges crisp at all DPI scales without requiring a
/// derivative-free path. `local_pos` is in element-local px (the rect
/// vertex coords used to build the record), `(0, 0)` at top-left.
fn clip_mask(local_pos: vec2<f32>, cid: u32) -> f32 {
    if (cid == 0u) {
        return 1.0;
    }
    let count = clips[cid].count;
    if (count == 0u) {
        return 1.0;
    }
    var max_d: f32 = -1.0e9;
    for (var i: u32 = 0u; i < count; i = i + 1u) {
        let e = clips[cid].edges[i];
        let d = e.x * local_pos.x + e.y * local_pos.y + e.z;
        max_d = max(max_d, d);
    }
    let aa = max(fwidth(max_d), 1e-5);
    return 1.0 - smoothstep(-aa, aa, max_d);
}

fn sdf_round_rect(p: vec2<f32>, half_size: vec2<f32>, r: f32) -> f32 {
    let q = abs(p) - half_size + vec2<f32>(r);
    return length(max(q, vec2<f32>(0.0))) + min(max(q.x, q.y), 0.0) - r;
}

/// Convex-polygon SDF (Phase 3.2 helper for box-shadow). Same edge
/// half-plane test as [`clip_mask`] but returns the raw signed
/// distance rather than a smoothstep coverage — lets the shadow
/// branch widen the falloff window itself. Negative inside, positive
/// outside. Returns a large negative sentinel when the clip slot is
/// unset so callers can `max()` it with a real SDF without effect.
fn clip_sdf(local_pos: vec2<f32>, cid: u32) -> f32 {
    if (cid == 0u) {
        return -1.0e9;
    }
    let count = clips[cid].count;
    if (count == 0u) {
        return -1.0e9;
    }
    var max_d: f32 = -1.0e9;
    for (var i: u32 = 0u; i < count; i = i + 1u) {
        let e = clips[cid].edges[i];
        let d = e.x * local_pos.x + e.y * local_pos.y + e.z;
        max_d = max(max_d, d);
    }
    return max_d;
}

/// Linear-gradient fill. `uv` is the rect-interior coord (0 at top-
/// left, 1 at bottom-right), `dir` the precomputed gradient direction
/// `(cos θ, sin θ)` — `θ = 0` is left→right, `θ = π/2` is top→bottom.
/// Project (uv - 0.5) onto dir and shift to `[0, 1]` as the blend factor.
fn gradient_fill(
    uv: vec2<f32>,
    color_a: vec4<f32>,
    color_b: vec4<f32>,
    dir: vec2<f32>,
) -> vec4<f32> {
    let t = clamp(dot(uv - vec2<f32>(0.5), dir) + 0.5, 0.0, 1.0);
    return mix(color_a, color_b, t);
}

/// Radial-gradient fill (`PAINT_FLAG_FILL_RADIAL`, bit 7). `uv` is the
/// rect-interior coord (0 at top-left, 1 at bottom-right). `t = 0` at the
/// centre, `1` at the inscribed circle's edge (rect corners clamp past 1
/// → fully `color_b`), so a square quad with `color_b` transparent reads
/// as a soft circle — CSS `radial-gradient(circle, color_a, color_b)`.
fn radial_fill(
    uv: vec2<f32>,
    color_a: vec4<f32>,
    color_b: vec4<f32>,
) -> vec4<f32> {
    let t = clamp(length(uv - vec2<f32>(0.5)) * 2.0, 0.0, 1.0);
    return mix(color_a, color_b, t);
}

@fragment
fn fs(in: V2F) -> @location(0) vec4<f32> {
    var c: vec4<f32>;
    if (in.shader_id == 0u) {
        // Coverage SDF. Normally the rounded-rect field; when the paint
        // carries `FEAT_ARC` (bit 8 = 256) swap in the IQ arc SDF so the
        // quad renders as a filled ring segment. Outer radius is the
        // quad half-extent (`half_size.x`); inner radius + start/sweep
        // angles come from the paint's arc slots. Everything downstream
        // (`aa`, shadow, gradient/stroke fill) then runs unchanged.
        var d: f32;
        if ((in.feature_flags & 512u) != 0u) {
            // FEAT_LINE (bit 9 = 512): analytic capsule / thick line
            // segment. Aliases the arc composite slots — here they carry
            // `half_delta.x`, `half_delta.y`, `thickness`. The quad's
            // bbox is the segment's bbox padded uniformly by `thickness`,
            // so its centre is the segment midpoint and the endpoints in
            // centred local space are exactly `±half_delta`. IQ 2D
            // capsule SDF (round caps).
            let half_delta = vec2<f32>(in.arc_inner_radius, in.arc_angle_start);
            let thickness = in.arc_angle_sweep;
            let a = -half_delta;
            let b =  half_delta;
            let pa = in.local_pos - a;
            let ba = b - a;
            let h = clamp(dot(pa, ba) / dot(ba, ba), 0.0, 1.0);
            d = length(pa - ba * h) - thickness;
        } else if ((in.feature_flags & 256u) != 0u) {
            let r_outer = in.half_size.x;
            let r_inner = in.arc_inner_radius;
            let ra = (r_outer + r_inner) * 0.5;       // mid radius
            let rb = (r_outer - r_inner) * 0.5;       // half thickness
            let half = abs(in.arc_angle_sweep) * 0.5; // half aperture
            let mid  = in.arc_angle_start + in.arc_angle_sweep * 0.5;
            // Rotate local_pos so the arc is symmetric about +Y (the IQ
            // sdArc convention): map the wedge's mid angle onto +Y.
            let a = mid - 1.5707963;
            let ca = cos(a);
            let sa = sin(a);
            let p = vec2<f32>(
                in.local_pos.x * ca - in.local_pos.y * sa,
                in.local_pos.x * sa + in.local_pos.y * ca,
            );
            // IQ arc SDF (round caps): sc = (sin, cos) of half-aperture.
            let sc = vec2<f32>(sin(half), cos(half));
            let pm = vec2<f32>(abs(p.x), p.y);
            let k = select(
                abs(length(pm) - ra),
                length(pm - sc * ra),
                sc.y * pm.x > sc.x * pm.y,
            );
            d = k - rb;
        } else {
            d = sdf_round_rect(in.local_pos, in.half_size, in.radius);
        }
        let aa = fwidth(d);

        // Phase 3.2 — rect-shadow branch. `PAINT_FLAG_SHADOW` (bit 6)
        // turns the regular sharp-edge rect coverage into a soft
        // halo: the smoothstep window widens from `[-aa, 0]` to
        // `[-r, +r]` where `r = shadow_radius` in source pixels.
        // Renders a Gaussian-like falloff with peak at the rect's
        // SDF-zero edge, fading to 0 at distance `r` outside and
        // saturating to full opacity inside `r`. Skips gradient,
        // stroke, and clip — shadows are pure tinted halos.
        //
        // The CPU side inflates the shadow's bounding quad by
        // `radius` on each side, so we recompute the SDF against
        // the ORIGINAL rect (half_size shrunk by shadow_radius)
        // rather than the inflated quad — keeps the SDF zero-
        // isoline aligned with the shadow source rect's edge.
        if ((in.feature_flags & 64u) != 0u) {
            let r = max(in.shadow_radius, aa);
            let inner_half = max(in.half_size - vec2<f32>(in.shadow_radius), vec2<f32>(0.0));
            let rect_d = sdf_round_rect(in.local_pos, inner_half, in.radius);

            // If the source rect carried a convex-polygon clip
            // (Phase 3.1, e.g. `hex_clip`), include its SDF so the
            // halo follows the polygon's edges instead of the
            // rounded-rect bounding box. Polygon edges live in
            // rect-top-left coords (`(0..w, 0..h)`); the inflated
            // shadow quad places the original rect's top-left at
            // `local_pos = -inner_half`, so we shift by `inner_half`
            // to get back into rect-local px.
            let rect_local_px = in.local_pos + inner_half;
            let clip_d = clip_sdf(rect_local_px, in.clip_id);
            let combined_d = max(rect_d, clip_d);

            let shadow_alpha = 1.0 - smoothstep(-r, r, combined_d);
            c = vec4<f32>(in.color.rgb, in.color.a * shadow_alpha);
            return c;
        }

        // Rect path. Start from solid fill; optionally replace with
        // gradient; optionally blend stroke over the fill at the edge.
        // For rects, `block_uv` collapses to `local_uv` because
        // block == (pos, size).
        var fill: vec4<f32> = in.color;
        if ((in.feature_flags & 1u) != 0u) {
            if ((in.feature_flags & 128u) != 0u) {
                fill = radial_fill(in.block_uv, in.color, in.color_b);
            } else {
                fill = gradient_fill(
                    in.block_uv,
                    in.color,
                    in.color_b,
                    in.gradient_dir,
                );
            }
        }

        // Outer coverage = 1 at rect interior, 0 beyond the edge.
        var outer_alpha = 1.0 - smoothstep(-aa, 0.0, d);

        // Apply convex-polygon clip (Phase 3.1). Polygon vertices live
        // in element-local pixel space — `(0, 0)` at the rect's top-left,
        // so reconstruct from `local_uv * size` (size = 2 * half_size).
        let local_px = in.local_uv * (in.half_size * 2.0);
        outer_alpha = outer_alpha * clip_mask(local_px, in.clip_id);

        var rgb: vec3<f32> = fill.rgb;
        var alpha: f32 = fill.a * outer_alpha;
        if ((in.feature_flags & 2u) != 0u) {
            // `in_stroke` ramps 0 → 1 across the stroke band
            // (`d` in `[-stroke_width, 0]`). Blend stroke over fill;
            // both channels fade out with `outer_alpha` at the edge.
            let w = in.stroke_width;
            var in_stroke = smoothstep(-w - aa, -w + aa, d);

            // Per-side mask (`BorderSides` TRBL bits). The fragment's
            // dominant edge is whichever of the four rect sides is
            // closest — the mask asks: is THAT edge enabled? Inside
            // the rounded-rect's corner-arc the test still works
            // because we measure edge-axis distance, not SDF distance.
            let sides = in.stroke_sides;
            if (sides != 0xFu) {
                let dx_left   = in.local_pos.x + in.half_size.x;
                let dx_right  = in.half_size.x - in.local_pos.x;
                let dy_top    = in.local_pos.y + in.half_size.y;
                let dy_bottom = in.half_size.y - in.local_pos.y;
                let m = min(min(dx_left, dx_right), min(dy_top, dy_bottom));
                var enabled: bool = false;
                if (m == dy_top    && (sides & 1u) != 0u) { enabled = true; }
                if (m == dx_right  && (sides & 2u) != 0u) { enabled = true; }
                if (m == dy_bottom && (sides & 4u) != 0u) { enabled = true; }
                if (m == dx_left   && (sides & 8u) != 0u) { enabled = true; }
                if (!enabled) {
                    in_stroke = 0.0;
                }
            }

            rgb = mix(fill.rgb, in.stroke_color.rgb, in_stroke);
            alpha = mix(fill.a, in.stroke_color.a, in_stroke) * outer_alpha;
        }

        c = vec4<f32>(rgb, alpha);
    } else if (in.shader_id == 1u) {
        // Bitmap-atlas text: atlas stores per-glyph coverage in the
        // red channel. When the paint carries
        // `PAINT_FLAG_FILL_GRADIENT`, sample the linear gradient at
        // `block_uv` (run-wide bbox) and use the atlas alpha as
        // coverage. This is Skia's `background-clip: text` model —
        // alpha mask × shader brush via SrcIn
        // (Flutter `canvas.cc:2017-2019`).
        let coverage = textureSample(atlas_tex, atlas_sampler, in.atlas_uv).r;
        var fill: vec4<f32> = in.color;
        if ((in.feature_flags & 1u) != 0u) {
            if ((in.feature_flags & 128u) != 0u) {
                fill = radial_fill(in.block_uv, in.color, in.color_b);
            } else {
                fill = gradient_fill(
                    in.block_uv,
                    in.color,
                    in.color_b,
                    in.gradient_dir,
                );
            }
        }
        c = vec4<f32>(fill.rgb, fill.a * coverage);
    } else if (in.shader_id == 3u) {
        // Save-layer composite (Phase 3.4a step 2.6). Sample the
        // sub-pass's transient at this quad's `local_uv`, apply the
        // paint's tint + opacity. Sub-pass writes the transient
        // through standard SrcAlpha blending into a transparent
        // clear, so the sample is PRE-MULTIPLIED. To re-use the
        // outer pass's same SrcAlpha blend state we un-premultiply
        // here, output straight-alpha colour, and let the blend
        // pre-multiply once more on output. `select` guards against
        // div-by-zero at fully-transparent samples — `inv_a = 0`
        // there means rgb is zero anyway, so the mismatch is
        // invisible.
                        let s = textureSample(transient_tex, transient_sampler, in.local_uv);
                let inv_a = select(1.0 / max(s.a, 1.0e-6), 0.0, s.a <= 1.0e-6);
        let straight_rgb = s.rgb * inv_a;
        c = vec4<f32>(in.color.rgb * straight_rgb, in.color.a * s.a);
    } else {
        // MSDF text (shader_id == 2). Canonical msdfgen reference
        // shader: per-pixel median of the three SDF channels +
        // smoothstep around 0.5 for screen-pixel-correct AA.
        // msdfgen's `correct_msdf_error` cleans up channel
        // disagreements at sharp inside corners during rasterisation,
        // so the shader doesn't need a corrective post-process.
        let s = textureSample(msdf_tex, msdf_sampler, in.atlas_uv).rgb;
        let median_value = max(min(s.r, s.g), min(max(s.r, s.g), s.b));
        let aa = max(fwidth(median_value), 1e-5);

        // Phase 2.5: when the paint is a drop-shadow / glow layer
        // (`PAINT_FLAG_SHADOW`, bit 6), widen the smoothstep window
        // inward from the glyph edge by `shadow_radius` (normalised
        // SDF distance). The MSDF distance field already encodes
        // per-pixel signed distance, so a wider AA window IS a soft
        // distance-based glow — visually close to CSS
        // `drop-shadow(... blur())` for retrowave-style neon, far
        // cheaper than a real Gaussian blur pass.
        var coverage: f32;
        if ((in.feature_flags & 64u) != 0u) {
            let r = clamp(in.shadow_radius, 0.0, 0.5);
            // `inner_thresh` is where the glow starts fading in;
            // `0.5 + aa` is the original (interior) edge. Pixels
            // with median > 0.5 get full opacity; pixels deep in
            // the exterior (< inner_thresh) stay transparent.
            let inner_thresh = 0.5 - r;
            coverage = smoothstep(inner_thresh - aa, 0.5 + aa, median_value);
        } else {
            coverage = smoothstep(0.5 - aa, 0.5 + aa, median_value);
        }

        var fill: vec4<f32> = in.color;
        if ((in.feature_flags & 1u) != 0u) {
            fill = gradient_fill(
                in.block_uv,
                in.color,
                in.color_b,
                in.gradient_dir,
            );
        }
        c = vec4<f32>(fill.rgb, fill.a * coverage);
    }

    // Tier-1 post-effects — applied uniformly to BOTH rect and text
    // fragments AFTER the per-shader-id colour compose. Both branches
    // use screen-space `clip_pos.y` so a CRT scanline pattern is
    // continuous across rects, button labels, icon glyphs, headlines.
    // Alpha is left untouched so compositing stays clean.
    if ((in.feature_flags & 4u) != 0u) {
        // Soft sinusoidal scanlines, scrolling downward at ~30 logical
        // px/s. `freq` is in logical px; convert physical FragCoord.y
        // to logical via `effective_scale` so the period stays the
        // same visual size on 1080p, HiDPI 2×, and 4K (ui_scale 1.75).
        let freq = max(in.effects.x, 1e-5);
        let logical_y = in.clip_pos.y / push.effective_scale + push.time * 30.0;
        let scan = 0.5 + 0.5 * sin(logical_y * 3.14159265 / freq);
        c = vec4<f32>(c.rgb * (1.0 - in.effects.y * scan), c.a);
    }
    if ((in.feature_flags & 8u) != 0u) {
        // Screen-space hash grain keyed off raw `FragCoord` (no scale
        // conversion needed — hash is unitless). Time-varying phase
        // reshuffles per frame for classic analog-TV static.
        let seed = dot(in.clip_pos.xy, vec2<f32>(12.9898, 78.233)) + push.time * 60.0;
        let n = fract(sin(seed) * 43758.5453);
        c = vec4<f32>(mix(c.rgb, vec3<f32>(n), in.effects.z * 0.3), c.a);
    }
    if ((in.feature_flags & 16u) != 0u) {
        // Roll-bar — a "glitchy CRT" band drifting down the LOGICAL
        // screen every `period_seconds`. INSIDE the band, instead
        // of plain brightening, we run per-channel scanlines with a
        // small Y offset between R/G/B → high-contrast horizontal
        // stripes that fringe red on top + blue on bottom, like a
        // TV losing sync. Outside the band: no effect.
        let period = max(in.roll.x, 1e-3);
        let logical_y = in.clip_pos.y / push.effective_scale;
        let logical_screen_h = push.screen_size.y;
        let roll_y = fract(push.time / period) * logical_screen_h;
        let raw_dist = abs(logical_y - roll_y);
        // Wrap the distance so the band cycles seamlessly when it
        // leaves the bottom of the screen.
        let dist = min(raw_dist, logical_screen_h - raw_dist);
        // ~60 logical-px-tall band (vs the prior 25-px brightener) —
        // big enough to fit a few scanlines inside the soft envelope.
        let band_radius = 60.0;
        let band = smoothstep(band_radius, 0.0, dist);
        if (band > 0.0) {
            // Inside the band: dense per-channel scanlines (4-logical-px
            // period) with R/G/B offset in Y by ±1.5 logical px. Mix
            // weighted by the band envelope so the edges fade out.
            let inside_freq = 4.0;
            let ca = 1.5;
            let phase = 3.14159265 / inside_freq;
            let scan_r = 0.5 + 0.5 * sin((logical_y - ca) * phase);
            let scan_g = 0.5 + 0.5 * sin( logical_y       * phase);
            let scan_b = 0.5 + 0.5 * sin((logical_y + ca) * phase);
            let intensity = in.roll.y;
            let glitched = vec3<f32>(
                c.r * (1.0 - intensity * scan_r),
                c.g * (1.0 - intensity * scan_g),
                c.b * (1.0 - intensity * scan_b),
            );
            c = vec4<f32>(mix(c.rgb, glitched, band), c.a);
        }
    }
    // #SRGB_OUT_BEGIN
    c = vec4<f32>(pow(c.rgb, vec3<f32>(1.0 / 2.2)), c.a);
    // #SRGB_OUT_END
    return c;
}
