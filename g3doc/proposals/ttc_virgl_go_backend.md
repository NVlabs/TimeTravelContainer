# A Go backend for TTC's virgl renderer

Status: done (2026-10-23), but for OpenGL ES 3.1. Stages 1 to 3, the shader
translator, the state tracker, and Mesa's removal, are done: the renderer
renders on the Go backend, vkgl, on Linux and on macOS, which passed [the
Mac's tests](#the-macs-tests), and links no Mesa. vkgl offers guests OpenGL
ES 3.0, where Zink offered 3.1: [OpenGL ES 3.1](#opengl-es-31) is to do.

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
saturation of every result as floats, shift counts masked, and indexes
clamped to their arrays, as robust drivers read. `MOD` is NIR's `imod`, of
which guests' `nir_to_tgsi` prints it, floored, of the divisor's sign, on
every device: Vulkan leaves `OpSMod` and `OpSRem` of a negative operand
undefined, NVIDIA's takes the operands as unsigned, and `tgsi_to_nir` gave
Zink `umod`.

## The state tracker

Stage 2 implements the decoder's backend (`virgl/backend.go`'s operations) in
Go:

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
    texture buffers. On an NVIDIA GB300, of driver 610 on arm64, the
    virgl tests pass on vkgl, 27 of 27, where Zink failed 5 (textures,
    uniform buffers and transform feedback rendered nothing), and so do
    vkgl's, and the corpus's 3,995 vertex and fragment shaders, all but
    the 63 of FBFETCH, compile into pipelines, of no errors of Vulkan's
    validation layer 1.4.341; `MOD` of negative operands was the one
    difference. The node runs no containers, of no root: redroid
    ran on lavapipe. Done on Linux since: vkgl is its backend, and its
    build has no Mesa, of which ttcd is 3.3 MB smaller and the renderer
    7.0 MB of 24.8, of glibc's functions of 2.4.)
3.  Mesa removed: no `tools/mesa.BUILD`, no glibc shims for it. (Done,
    2026-10-23: Linux's build first, and then macOS's, of the Mac's tests,
    of which ttcd is 14.2 MB smaller there, 61.6 MB of 75.8.)

## The Mac's tests

vkgl renders on MoltenVK as on any Vulkan 1.3 device of dynamic rendering,
but for what MoltenVK lacks, which it works around
(`vkgl/device_unsafe.go`):

*   `VK_KHR_portability_subset`'s triangle fans, which its capability sets
    then leave guests' Mesa to draw as triangles (`u_primconvert`); polygons
    drawn as points, which it draws as lines; samplers' LOD bias, which
    OpenGL ES has none of; stencil references of each face, of which it
    sets the front's; and multisampled arrays, which it refuses. A device
    of no swizzles of image views, which textures' swizzles and its
    emulated formats are, it refuses.
*   Apple's GPUs' 24-bit depths, of which it renders to 32-bit floats, as
    Zink does: Mesa has no other format of `GL_DEPTH24_STENCIL8`, which
    Android's EGL configurations are of. Transfers and clears of
    `Z24X8` convert the guest's depths.
*   Indexes of a byte, which Metal has none of, where the device has no
    `VK_KHR_index_type_uint8`: a draw's are read back and widened to 16
    bits, as Zink widens them.
*   `VK_EXT_provoking_vertex`, which MoltenVK lacks: flat varyings are
    the first vertex's, Metal's, rather than OpenGL's last.
*   robustness2's `nullDescriptor`: vkgl binds a texture or buffer of its
    own to every slot that a shader reads and a guest binds none to.

`vkgl`'s `TestPortabilitySubset` forces the workarounds on the device it
runs on. The Mac's tests ran on an Apple M5 Pro, of MoltenVK 1.4.2, on
2026-10-23, before Mesa left, and passed:

1.  The unit tests on vkgl, `//runsc/ttc/gfxrenderer/virgl:virgl_test` and
    `//runsc/ttc/gfxrenderer/virgl/vkgl:vkgl_test`. None had run on a Mac:
    vkgl asked MoltenVK, which the renderer links without Vulkan's loader,
    for the loader's `VK_KHR_portability_enumeration`, and found no device;
    it asks only where Vulkan has it now. Of the decoder's tests,
    `TestSubContextsAndZombies` drew nothing in one run of four, as a
    sub-context drew from a buffer whose upload another's unflushed batch
    held: switching sub-contexts flushes the one left now.
2.  A corpus of guests' TGSI captured on the Mac (see
    [Validation](#validation)): shader-db's shaders through Debian's virgl
    driver, and redroid's SurfaceFlinger's and Settings'. Its 3,647
    shaders create (`VIRGL_TGSI_CORPUS=DIR`'s `TestTGSICorpus`), and
    compile into pipelines (`VKGL_TGSI_CORPUS=DIR`'s
    `TestCorpusPipelines`), of none of FBFETCH, which Zink offered guests
    none of on MoltenVK.
3.  `runsc/ttc/test/e2e_gfx.sh` and `runsc/ttc/test/e2e_redroid.sh`, on
    vkgl, of the renderer's sandbox, and on Zink: redroid's Settings, of
    each branch of its live fork, is Zink's to the pixel, but for the
    status bar's clock.

Then vkgl became every platform's backend (`virgl/backend.go`'s
`initBackend`), and Mesa left: `tools/mesa.BUILD`, `tools/mesa/`, `@mesa`
and the Python modules of its code generators in `MODULE.bazel`, the
decoder's C and `zink_unsafe.go`, and Mesa's components in
`THIRD_PARTY_NOTICES.md`. The checks above passed again of the build of no
Mesa, as did the macOS check of CI.

## OpenGL ES 3.1

TODO. vkgl's capability sets offer guests what it renders, of which
Mesa's virgl driver gives them OpenGL ES 3.0 (`vkgl/caps_unsafe.go`).
Zink gave them 3.1, which some of Android's apps require. Mesa's OpenGL ES
3.1 (`src/mesa/main/version.c`) needs, beyond 3.0, what vkgl refuses
today (`virgl/vkgl_unsafe.go`'s `errVkgl`):

*   compute shaders: compute pipelines of `tospirv`'s compute shaders,
    which it translates, `LAUNCH_GRID`, and its indirect form;
*   shader buffers (`SET_SHADER_BUFFERS`) and atomic counters
    (`SET_ATOMIC_BUFFERS`, `HWATOMIC`), as storage buffers, in fragment
    and compute shaders at least;
*   shader images (`SET_SHADER_IMAGES`), as storage images, of the
    formats that the device stores to;
*   indirect draws, of `DRAW_VBO`'s indirect buffer, and their counts;
*   memory barriers (`MEMORY_BARRIER`), as Vulkan's, which vkgl ignores
    while shaders store nothing;
*   checks of what 3.1 adds to 3.0's state: multisampled textures and
    their sample masks, and stencil sampled of depth-stencil textures.

Then the capability sets offer them, which `caps_unsafe.go` now clears.

Two gaps of what guests get already: rendering to 3D textures' slices,
which OpenGL ES 3.0 allows, and vkgl's surfaces refuse, as Vulkan views a
3D image's slices only of images created to be viewed so; and conditional
rendering, which desktop OpenGL 3.0 requires, and which the capability
sets do not offer, of which desktop OpenGL guests get 2.1 (Linux's use Zink
in the guest by default). OpenGL ES 3.2 and OpenGL 3.2 and later need,
besides, geometry and tessellation shaders, which `tospirv` translates,
texture buffers, and framebuffer fetch (`FBFETCH`, of advanced blending),
whose input attachments pipelines' layouts refuse.
