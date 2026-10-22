# A Go backend for TTC's virgl renderer

Status: in progress (2026-10-23). Stage 1, the shader translator, is done;
stage 2 is under way: the decoder renders on the Go backend where
`TTC_VIRGL_BACKEND` is `vkgl`, and on Zink otherwise. Stage 3 is planned.

## Goal

The renderer serves guests' OpenGL, which Mesa's virgl driver encodes as
Gallium state and TGSI shaders, with Mesa's Zink: Gallium on Vulkan, linked
into ttcd's renderer. Zink works, but it is a large C library on the
renderer's guest-facing path, with costs of its own:

*   a Mesa build of its own, generated for each platform
    (`tools/mesa.BUILD`), and glibc shims so that the renderer still runs on
    glibc 2.17 (`gfxrenderer/glibccompat`);
*   about 15 MB of ttcd;
*   C that parses and compiles what guests send;
*   forks that clone Gallium's state through Zink's objects.

The backend replaces Zink with Go, as the Venus renderer replaced gfxstream:
the decoder stays, and calls a backend that translates guests' TGSI to
SPIR-V and their Gallium state to Vulkan, on the Vulkan that the Venus
renderer already calls. It is not a port of Zink, which compiles TGSI to NIR
and NIR to SPIR-V, and implements all of OpenGL 4.6: it translates TGSI to
SPIR-V directly, as virglrenderer translates TGSI to GLSL, for the state
that virgl's guests send.

## What guests send

Mesa's virgl driver compiles shaders to NIR, and NIR to TGSI with
`nir_to_tgsi` (`ntt`), which `virgl_tgsi_transform` then edits for
virglrenderer's sake, and sends TGSI's text, `tgsi_dump_str`'s, floats
printed as their bits in hex. The dialect is narrower than TGSI:

*   registers are those that `ntt` allocates: temporaries, address
    registers that indirect addressing loads (`UARL`), arrays of
    temporaries (`ARRAY(n)`) that it indexes;
*   uniforms are constant buffers, `CONST[n][i]`, of vec4s; the default
    block's, buffer 0, loses its dimension (`CONST[i]`);
*   control flow is `IF`/`UIF`/`ELSE`/`ENDIF` and `BGNLOOP`/`ENDLOOP` with
    `BRK` and `CONT`, structured, with no subroutines;
*   texture instructions pack coordinates, the reference, and the bias or
    LOD into source 0, and the rest into source 1 (`TEX2`, `TXB2`, `TXL2`),
    as `nir_to_tgsi_lower_tex` does;
*   source modifiers are typed by their instruction, as
    `tgsi_opcode_infer_src_type` types them;
*   geometry and tessellation shaders' per-vertex inputs, and tessellation
    control shaders' outputs, are two-dimensional (`IN[v][i]`).

The caps that the renderer reports decide much of the rest: what the guest
lowers (indirect inputs, atomic counters, doubles, 64-bit integers, subgroup
operations) and what it sends. Guests lower none of OpenGL's fixed-function
state, though: flat shading of colors (`TGSI_INTERPOLATE_COLOR`), two-sided
colors, alpha testing, user clip planes, and point sprites are the host's,
as Gallium drivers implement them. Android's OpenGL ES has none of them;
desktop OpenGL's compatibility profile does.

## The translator

Three packages, `gfxrenderer/virgl/{tgsi,spirv,tospirv}`:

*   **tgsi** parses the text, and prints it as Mesa does, byte for byte, so
    that tests and fuzzing check the parser against Mesa's printer.
*   **spirv** builds SPIR-V 1.3 modules: types and constants, uniqued;
    decorations; functions whose variables go to their entry blocks. Its
    enumerations are generated from Khronos's grammar.
*   **tospirv** translates a shader, for the pipeline state that it depends
    on (`Options`).

TGSI's registers are typeless vectors of four 32-bit channels, which each
instruction takes as its operands' type. Every register is a Private `uvec4`
(an array of them, for an `ARRAY`); an instruction loads its sources,
swizzles them, bitcasts them to its type, modifies them, and writes its
result's bits to the channels of its write mask. Drivers' compilers remove
the variables and bitcasts.

The shader's body is a function of its own, which `main` calls between a
prologue, which copies inputs and system values to registers, and an
epilogue, which copies output registers to outputs, so that `RET` returns
from the body and every path passes the epilogue. Registers that the
prologue cannot copy are accessed in the interface instead: per-vertex
inputs, and a tessellation control shader's outputs, which its invocations
share. A geometry shader's `EMIT` copies the outputs before it emits.

Control flow is SPIR-V's structured control flow: every `IF` has an else
block, empty if TGSI's has none, and every loop a continue block; code after
`BRK`, `CONT`, `KILL` or `RET` begins a block of its own.

Resources bind by stage and slot: a descriptor set of each kind (uniform
buffers 0, sampled textures 1, storage buffers 2, storage images 3), whose
binding `stage*32 + slot` is a stage's slot (`tospirv.Binding`). A shader's
`Result` lists the resources that it uses, and each pipeline's layout is of
its shaders' resources. Values of each draw that shaders read, such as the
framebuffer's height (for TGSI's lower-left fragment coordinates) and the
draw's first instance (which GL's instance IDs exclude and Vulkan's include),
are push constants.

Varyings get provisional locations; `Result` gives each varying's semantic
and the word of its `Location` decoration, so that a pipeline links a
stage's outputs to the next's inputs by semantic, patching the words,
without translating again.

The translator matches Zink's semantics where TGSI leaves a choice, as
`tgsi_to_nir` makes it: modifiers by `tgsi_opcode_infer_src_type`,
saturation of every result as floats, `MOD` as a signed remainder, shift
counts masked, and indexes clamped to their arrays, as robust drivers read.

## The state tracker

Stage 2 implements the decoder's backend (`virgl.h`'s operations) in Go:

*   **Resources:** buffers and images in device memory, of Vulkan formats
    that `vkgl`'s table gives virgl's, emulating alpha, luminance and
    intensity formats with views' swizzles; transfers through staging
    buffers.
*   **Objects:** blend, rasterizer, depth-stencil-alpha, and sampler states,
    vertex elements, and sampler views, as the Vulkan state that they are;
    shaders as their parsed TGSI, translated per pipeline.
*   **Draws:** dynamic rendering; pipelines keyed by their shaders and
    state, in a cache, with descriptor sets written per draw from the
    bound resources; Gallium's viewports, scissors, and depth range;
    primitive restart; indirect draws.
*   **The rest:** clears, blits and copies, mipmap generation, resolves,
    queries and conditional rendering, transform feedback (emulated, as the
    decoder emulates it today), compute.
*   **Forks:** a context's state is Go's, which a clone copies, sharing its
    pipelines and shader modules.

## Validation

*   Unit tests of each package; `spirv-val` of the translator's output.
*   A corpus: the TGSI that Debian's virgl driver sends of shader-db's
    shaders (some 4,000 programs of games, browsers, and toolkits), and
    that Android's sends of SurfaceFlinger's and apps' shaders, captured by
    a renderer with a diagnostic hook; `tospirv`'s `TestCorpus` translates
    each, and `spirv-val` validates the results.
*   Pixels: the virgl tests' and the e2e suites' images, rendered by the Go
    backend and by Zink, compared.

## Milestones

1.  The translator: every stage, resources, and the corpus translating and
    validating. (Done, 2026-10-23: the corpus's 4,058 shaders, of Redroid
    and of shader-db, of which none is a geometry, tessellation or compute
    shader, translate, print as they parse, and validate.)
2.  The state tracker, behind the decoder's backend interface, with Zink as
    the other backend: the virgl tests, `e2e_gfx.sh`'s OpenGL ES, and
    redroid, compared with Zink's pixels. (Under way, 2026-10-23: the
    decoder executes on a backend interface, and `vkgl` renders draws,
    clears, copies, blits, queries, transform feedback and clones; the
    virgl tests pass on both backends, of no errors of Vulkan's validation
    layer of vkgl's, and the corpus's shaders create on both. On lavapipe,
    `e2e_gfx.sh`'s OpenGL ES and redroid pass on `vkgl`: SurfaceFlinger
    and Settings render, record and fork, and the Settings screen is
    Zink's to the pixel. Not yet, nor offered to guests: shader buffers
    and images, compute, geometry and tessellation, indirect draws,
    texture buffers.)
3.  Mesa removed: no `tools/mesa.BUILD`, no glibc shims for it.
