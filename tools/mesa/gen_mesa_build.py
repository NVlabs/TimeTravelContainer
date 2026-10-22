# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Writes the Bazel BUILD file of the part of Mesa that TTC links.

TTC's GPU renderer links Zink, Mesa's Gallium driver on Vulkan, and what it
needs of Mesa (tools/mesa.BUILD). Mesa builds with meson; this reads a meson
build of Mesa for one platform, configured as README.md says, and writes the
same compilations as Bazel's cc_library targets of that platform, and
Mesa's code generators as genrules, which run them by //tools/mesa:run. It
keeps the other platforms' targets that the BUILD file has, so that each
platform's meson build, which runs on that platform or cross-compiles for
it, regenerates its own.

Usage:
  gen_mesa_build.py --source MESA_SRC --build MESON_BUILD --out tools/mesa.BUILD
"""

import argparse
import collections
import json
import os
import re
import shlex
import subprocess
import sys

# The meson static libraries that Zink needs, in link order, and their
# targets' names.
LIBRARIES = [
    ("src/gallium/drivers/zink/libzink.a", "zink_driver"),
    ("src/gallium/auxiliary/libgallium.a", "gallium"),
    ("src/compiler/nir/libnir.a", "nir"),
    ("src/compiler/libcompiler.a", "compiler"),
    ("src/vulkan/util/libvulkan_util.a", "vulkan_util"),
    ("src/util/libxmlconfig.a", "xmlconfig"),
    ("src/util/libmesa_util.a", "util"),
    ("src/util/libmesa_util_simd.a", "util_simd"),
    ("src/util/blake3/libblake3.a", "blake3"),
    ("src/c11/impl/libmesa_util_c11.a", "c11"),
]

# Flags that Bazel's toolchain and compilation mode decide.
DROPPED_FLAGS = re.compile(
    r"^(-O.*|-g.*|-fdiagnostics-color=.*|-Winvalid-pch|-MD|-c|-pipe)$")
DROPPED_FLAGS_WITH_VALUE = {"-MQ", "-MF", "-o"}
PAIRED_FLAGS = {"-x", "-iframework", "-include"}

# Header directories of Mesa's source that the libraries cannot include.
UNUSED_HEADER_DIRS = [
    "src/amd", "src/asahi", "src/broadcom", "src/etnaviv", "src/freedreno",
    "src/imagination", "src/intel", "src/kosmickrisp", "src/microsoft",
    "src/nouveau", "src/panfrost", "src/poly",
    "src/gallium/drivers/asahi", "src/gallium/drivers/crocus",
    "src/gallium/drivers/d3d12", "src/gallium/drivers/etnaviv",
    "src/gallium/drivers/freedreno", "src/gallium/drivers/i915",
    "src/gallium/drivers/iris", "src/gallium/drivers/lima",
    "src/gallium/drivers/llvmpipe", "src/gallium/drivers/nouveau",
    "src/gallium/drivers/panfrost", "src/gallium/drivers/r300",
    "src/gallium/drivers/r600", "src/gallium/drivers/radeonsi",
    "src/gallium/drivers/softpipe", "src/gallium/drivers/svga",
    "src/gallium/drivers/v3d", "src/gallium/drivers/vc4",
    "src/gallium/drivers/virgl", "src/gallium/drivers/zink/ci",
    "src/gallium/frontends", "src/gallium/targets",
]

# The directories of what TTC's renderer includes: Gallium's interface,
# Zink's, NIR's, and virgl's protocol, which Mesa's guest driver shares.
API_INCLUDES = [
    "include",
    "src",
    "src/compiler",
    "src/compiler/nir",
    "src/gallium/include",
    "src/gallium/auxiliary",
    "src/gallium/drivers/zink",
    "src/util/format",
    "src/virtio/virtio-gpu",
]

# The platforms that the BUILD file builds Mesa for, each of a meson build of
# its own, and their constraints.
PLATFORMS = collections.OrderedDict([
    ("macos", ["@platforms//os:macos"]),
    ("linux_x86_64", ["@platforms//os:linux", "@platforms//cpu:x86_64"]),
    ("linux_aarch64", ["@platforms//os:linux", "@platforms//cpu:aarch64"]),
])

# What Zink links, besides Mesa's libraries.
LINKOPTS = {
    "macos": [
        "-framework CoreFoundation",
        "-framework Foundation",
        "-framework IOSurface",
        "-framework Metal",
        "-framework QuartzCore",
        "-lc++",
        "-lz",
    ],
    # Zink's C++ needs nothing of libstdc++ (WHOLE_ARCHIVE_DROPPED). libdl
    # and libpthread are runsc/ttc/glibccompat's.
    "linux": ["-lm"],
}

# Headers of other libraries that Zink's compilations need.
ZINK_DEPS = {
    "macos": ["@moltenvk//:mvk_headers"],
    "linux": [],
}

# The meson builds' flags and defines that Linux's targets drop:
# -mtls-dialect=gnu2, for shared libraries' TLS, which the renderer, an
# executable, has no use for; functions that glibc 2.17, the oldest that
# the renderer runs with, lacks, of which Mesa has its own; the shader
# cache's, which is disabled; and libraries that the builder has but the
# renderer does not link.
DROPPED = {
    "linux": {
        "-mtls-dialect=gnu2",
        # off_t is 64 bits: the define binds fcntl to glibc 2.28's fcntl64.
        "_FILE_OFFSET_BITS=64",
        "HAVE_REALLOCARRAY",  # glibc 2.26
        "HAVE_GETRANDOM",  # glibc 2.25
        "HAVE_ISSIGNALING",  # glibc 2.18
        "HAVE_FMEMOPEN",  # a version of glibc 2.22
        "HAVE_DLADDR",  # libc's since glibc 2.34; the shader cache's
        "HAVE_PTHREAD_SETAFFINITY",  # libc's since glibc 2.34
        "HAVE_OPENMP",
        "USE_LIBELF",
    },
}


# rules_go links each of cgo's C libraries whole on Linux (cgo.bzl), so that
# every object of every library is linked, rather than those that resolve a
# symbol, as on macOS: Linux's targets compile a source that meson compiles
# into two libraries into the first only, and leave out sources whose
# objects call what Zink does not link, and which nothing that it links
# calls.
WHOLE_ARCHIVE_DROPPED = {
    # vk_spec_info_to_nir_spirv calls spirv_to_nir's specialization.
    "src/vulkan/util/vk_util.c",
    # The ASTC decoder, which alone of what is linked needs libstdc++, which
    # the renderer would then need of a version that it ran with.
    "src/util/texcompress_astc.cpp",
    "src/util/texcompress_astc_luts.cpp",
    "src/util/texcompress_astc_luts_wrap.cpp",
}


def platform_kind(platform):
    return "macos" if platform == "macos" else "linux"


def build_platform(build):
    """Returns the platform that a meson build is of."""
    machines = json.load(open(os.path.join(build, "meson-info", "intro-machines.json")))
    host = machines["host"]
    if host["system"] == "darwin":
        return "macos"
    platform = f'{host["system"]}_{host["cpu_family"]}'
    if platform not in PLATFORMS:
        sys.exit(f"no platform {platform} in PLATFORMS")
    return platform


def ninja_unescape(s):
    out = []
    i = 0
    while i < len(s):
        c = s[i]
        if c == "$" and i + 1 < len(s):
            out.append(s[i + 1])
            i += 2
            continue
        out.append(c)
        i += 1
    return "".join(out)


def ninja_split(s):
    """Splits a ninja build line on unescaped spaces."""
    tokens, cur, i = [], [], 0
    while i < len(s):
        c = s[i]
        if c == "$" and i + 1 < len(s):
            cur.append(s[i:i + 2])
            i += 2
            continue
        if c == " ":
            if cur:
                tokens.append("".join(cur))
                cur = []
            i += 1
            continue
        cur.append(c)
        i += 1
    if cur:
        tokens.append("".join(cur))
    return tokens


class Edge:
    def __init__(self, outputs, rule, inputs, implicit, order_only):
        self.outputs = outputs
        self.rule = rule
        self.inputs = inputs
        self.implicit = implicit
        self.order_only = order_only
        self.vars = {}


def parse_ninja(path):
    edges = []
    lines = open(path).read().replace("$\n", "").split("\n")
    edge = None
    for line in lines:
        if line.startswith("build "):
            head, _, rest = line[len("build "):].partition(": ")
            # ':' in paths is escaped, so the first ': ' ends the outputs.
            outputs = [ninja_unescape(t) for t in ninja_split(head)]
            outputs = [o for o in outputs if o != "|"]
            toks = ninja_split(rest)
            rule, toks = toks[0], toks[1:]
            inputs, implicit, order_only = [], [], []
            dest = inputs
            for t in toks:
                if t == "|":
                    dest = implicit
                elif t == "||":
                    dest = order_only
                elif t == "|@":
                    dest = []
                else:
                    dest.append(ninja_unescape(t))
            edge = Edge(outputs, rule, inputs, implicit, order_only)
            edges.append(edge)
        elif line.startswith(" ") and edge is not None:
            key, _, value = line.strip().partition(" = ")
            edge.vars[key] = value
        else:
            edge = None
    return edges


class Mesa:
    def __init__(self, source, build):
        self.source = os.path.realpath(source)
        self.build = os.path.realpath(build)
        self.src_rel = os.path.relpath(self.source, self.build)
        self.edges = parse_ninja(os.path.join(self.build, "build.ninja"))
        self.producer = {}
        for e in self.edges:
            if e.rule == "CUSTOM_COMMAND":
                for o in e.outputs:
                    self.producer[o] = e
        compdb = subprocess.run(["ninja", "-C", self.build, "-t", "compdb"],
                                check=True, capture_output=True, text=True).stdout
        self.compdb = json.loads(compdb)

    def source_path(self, p):
        """Returns p's path in Mesa's source, or None."""
        if p.startswith(self.src_rel + "/"):
            return os.path.normpath(p[len(self.src_rel) + 1:])
        if os.path.isabs(p) and os.path.realpath(p).startswith(self.source + "/"):
            return os.path.relpath(os.path.realpath(p), self.source)
        return None

    def generated(self, p):
        return p in self.producer


def compile_groups(mesa, dropped):
    """Returns the compilations of each library, grouped by their flags, less
    the flags and defines dropped."""
    groups = collections.OrderedDict()
    order_only = set()
    objects = {}
    for e in mesa.edges:
        if e.rule.endswith("_COMPILER"):
            for o in e.outputs:
                objects[o] = e
    for lib, name in LIBRARIES:
        prefix = lib + ".p/"
        entries = [x for x in mesa.compdb if x["output"].startswith(prefix)]
        if not entries:
            sys.exit(f"no compilations of {lib}: build it first")
        for entry in entries:
            edge = objects.get(entry["output"])
            if edge is not None:
                order_only.update(edge.order_only)
                order_only.update(edge.implicit)
            argv = shlex.split(entry["command"])
            while argv and not argv[0].startswith("-") and os.path.basename(argv[0]) not in ("cc", "c++", "clang", "clang++"):
                argv = argv[1:]
            compiler, argv = argv[0], argv[1:]
            lang = "cxx" if compiler.endswith("++") else "c"
            includes, defines, flags = [], [], []
            i = 0
            while i < len(argv):
                a = argv[i]
                if a in DROPPED_FLAGS_WITH_VALUE:
                    i += 2
                    continue
                if a in PAIRED_FLAGS:
                    flags.append(a + " " + argv[i + 1])
                    i += 2
                    continue
                i += 1
                if a.startswith("-I"):
                    d = a[2:]
                    if d.endswith(".a.p"):
                        continue
                    sp = mesa.source_path(d)
                    if sp is not None:
                        includes.append(sp)
                    elif not os.path.isabs(d):
                        includes.append(os.path.normpath(d))
                    # Other absolute directories are dependencies' headers
                    # (MoltenVK's), which their targets provide.
                    continue
                if a.startswith("-D"):
                    if a[2:] not in dropped:
                        defines.append(a[2:])
                    continue
                if DROPPED_FLAGS.match(a) or a in dropped:
                    continue
                if not a.startswith("-"):
                    continue  # the source file
                flags.append(a)
            src = entry["file"]
            sp = mesa.source_path(src)
            if sp is None:
                if not mesa.generated(src):
                    sys.exit(f"{src} is neither a source nor generated")
                sp = src
            includes = list(collections.OrderedDict.fromkeys(includes))
            key = (name, lang, tuple(includes), tuple(defines), tuple(flags))
            groups.setdefault(key, []).append(sp)
    return groups, order_only


def needed_commands(mesa, groups, order_only):
    wanted = set(o for o in order_only if mesa.generated(o))
    for srcs in groups.values():
        wanted.update(s for s in srcs if mesa.generated(s))
    edges, seen = [], set()
    todo = list(wanted)
    while todo:
        out = todo.pop()
        e = mesa.producer[out]
        if id(e) in seen:
            continue
        seen.add(id(e))
        edges.append(e)
        for i in e.inputs + e.implicit:
            if mesa.generated(i):
                todo.append(i)
    edges.sort(key=lambda e: e.outputs[0])
    return edges


def included_sources(mesa, groups, commands):
    """Returns the sources that others include, as generated files do."""
    include_dirs = set()
    files = []
    for (lib, lang, includes, defines, flags), srcs in groups.items():
        include_dirs.update(includes)
        files += srcs
    for e in commands:
        files += e.outputs
    found = set()
    pattern = re.compile(r'^\s*#\s*include\s+"([^"]+\.(?:c|cpp|in))"', re.M)
    for f in files:
        path = os.path.join(mesa.build if mesa.generated(f) else mesa.source, f)
        try:
            text = open(path, errors="replace").read()
        except FileNotFoundError:
            continue
        for inc in pattern.findall(text):
            for d in [os.path.dirname(f)] + sorted(include_dirs):
                candidate = os.path.normpath(os.path.join(d, inc))
                if os.path.exists(os.path.join(mesa.source, candidate)):
                    found.add(candidate)
                    break
    return sorted(found)


def genrule(mesa, e):
    srcs = []
    for i in e.inputs + e.implicit:
        sp = mesa.source_path(i)
        if sp is not None:
            srcs.append(sp)
        elif mesa.generated(i):
            srcs.append(i)
    srcs = list(collections.OrderedDict.fromkeys(srcs))
    argv = shlex.split(e.vars["COMMAND"].replace("$$", "$"))
    capture = None
    if os.path.basename(argv[0]) == "meson" and argv[1:3] == ["--internal", "exe"]:
        rest = argv[3:]
        sep = rest.index("--")
        opts, argv = rest[:sep], rest[sep + 1:]
        j = 0
        while j < len(opts):
            if opts[j] == "--capture":
                capture = opts[j + 1]
                j += 2
            else:
                sys.exit(f"unexpected meson exe option {opts[j]} in {e.outputs}")
    if not re.search(r"python[0-9.]*$", os.path.basename(argv[0])):
        sys.exit(f"{e.outputs} are not generated by Python: {argv}")
    # The modules beside the generator, which it may import though Mesa's
    # build does not say so.
    script = mesa.source_path(argv[1])
    if script is not None:
        d = os.path.dirname(script)
        for f in sorted(os.listdir(os.path.join(mesa.source, d))):
            if f.endswith(".py") and os.path.join(d, f) not in srcs:
                srcs.append(os.path.join(d, f))
    words = ["$(execpath @@//tools/mesa:run)"]
    if capture is not None:
        words += ["--capture", f"$(execpath {capture})"]
    for a in argv[1:]:
        words.append(translate_arg(mesa, a, srcs, e))
    name = "gen_" + re.sub(r"[^A-Za-z0-9]+", "_", e.outputs[0])
    return name, srcs, e.outputs, " ".join(words)


def translate_arg(mesa, a, srcs, e):
    if "=" in a and a.startswith("-"):
        k, v = a.split("=", 1)
        t = translate_path(mesa, v, srcs, e)
        return f"{k}={t}" if t is not None else shlex.quote(a)
    t = translate_path(mesa, a, srcs, e)
    return t if t is not None else shlex.quote(a)


def translate_path(mesa, a, srcs, e):
    if a in e.outputs or (mesa.generated(a) and a in srcs):
        return f"$(execpath {a})"
    sp = mesa.source_path(a.rstrip("/"))
    if sp is None:
        return None
    if sp in srcs:
        return f"$(execpath {sp})"
    if os.path.isdir(os.path.join(mesa.source, sp)):
        # A directory: name it by a file of it that the rule takes.
        for s in srcs:
            if s.startswith(sp + "/") and "/" not in s[len(sp) + 1:]:
                return f"$$(dirname $(execpath {s}))" + ("/" if a.endswith("/") else "")
        sys.exit(f"{e.outputs}: no input in directory {sp}")
    srcs.append(sp)
    return f"$(execpath {sp})"


def shell_word(s):
    """Quotes s for Bazel, which tokenizes defines as a shell does."""
    return shlex.quote(s) if re.search(r"[\s\"'\\$]", s) else s


def starlark_list(items, indent=8):
    if not items:
        return "[]"
    pad = " " * indent
    body = "".join(f"{pad}{json.dumps(i)},\n" for i in items)
    return "[\n" + body + " " * (indent - 4) + "]"


def platform_targets(mesa, platform):
    """Returns the genrules, the headers, and the libraries of a platform's
    meson build."""
    kind = platform_kind(platform)
    groups, order_only = compile_groups(mesa, DROPPED.get(kind, set()))
    if kind == "linux":
        linked = set(WHOLE_ARCHIVE_DROPPED)
        for key in list(groups):
            srcs = [src for src in groups[key] if src not in linked]
            linked.update(srcs)
            if srcs:
                groups[key] = srcs
            else:
                del groups[key]
    commands = needed_commands(mesa, groups, order_only)
    genrules = collections.OrderedDict()
    generated_headers = []
    for e in commands:
        name, srcs, outs, cmd = genrule(mesa, e)
        generated_headers += [o for o in outs if o.endswith(".h")]
        genrules[name] = {"srcs": srcs, "outs": outs, "cmd": cmd}
    libraries = []
    api_defines = None
    counts = collections.Counter(k[0] for k in groups)
    seen = collections.Counter()
    for (lib, lang, includes, defines, flags), srcs in groups.items():
        seen[lib] += 1
        name = lib if counts[lib] == 1 else f"{lib}_{seen[lib]}"
        if lib == "zink_driver" and lang == "c" and api_defines is None:
            api_defines = defines
        deps = [":headers"]
        if lib == "zink_driver":
            deps += ZINK_DEPS[kind]
        libraries.append({
            "name": name,
            "opts": "conlyopts" if lang == "c" else "cxxopts",
            "srcs": sorted(srcs),
            "flags": list(flags),
            "includes": list(includes),
            "local_defines": [shell_word(d) for d in defines],
            "deps": deps,
        })
    # Those of Zink's defines that configure Mesa's headers, which code that
    # includes them must agree with.
    api_defines = [shell_word(d) for d in api_defines
                   if re.match(r"^(HAVE_|USE_)", d) and not d.startswith(("HAVE_OPENGL", "HAVE_ZINK"))]
    return {
        "genrules": genrules,
        "generated_headers": sorted(set(generated_headers)),
        "textual": included_sources(mesa, groups, commands),
        "libraries": libraries,
        "linkopts": LINKOPTS[kind],
        "api_defines": api_defines,
    }


class Select:
    def __init__(self, branches):
        self.branches = branches


def read_build(path):
    """Returns the platforms' targets that the BUILD file at path has, as
    platform_targets returns them."""
    try:
        text = open(path).read()
    except FileNotFoundError:
        return collections.OrderedDict()
    rules = []

    def rule(kind):
        return lambda **kw: rules.append((kind, kw))

    def none(*args, **kw):
        return None

    env = {
        "load": none, "package": none, "licenses": none, "exports_files": none,
        "glob": lambda *args, **kw: [], "select": Select,
        "genrule": rule("genrule"), "cc_library": rule("cc_library"),
        "config_setting": rule("config_setting"),
    }
    exec(compile(text, path, "exec"), env)

    def branch(value, platform):
        """Returns platform's branch of value, a select by platform or a
        value of every platform."""
        if isinstance(value, Select):
            return value.branches.get(":" + platform)
        return value

    # A library's target is NAME_PLATFORM; a BUILD file of one platform,
    # of the first generator, named it NAME, and that platform is its
    # constraints'.
    platforms = collections.OrderedDict()
    constraints = {tuple(v): k for k, v in PLATFORMS.items()}
    genrules = collections.OrderedDict()
    headers = None
    libraries = []
    public = {}
    for kind, kw in rules:
        if kind == "genrule":
            genrules[kw["name"]] = {"srcs": kw["srcs"], "outs": kw["outs"], "cmd": kw["cmd"]}
        elif kind == "cc_library" and kw["name"] == "headers":
            headers = kw
        elif kind == "cc_library" and kw["name"] in ("zink", "gallium_api"):
            public[kw["name"]] = kw
        elif kind == "cc_library":
            libraries.append(kw)
    for kw in libraries:
        name = kw["name"]
        platform = next((p for p in PLATFORMS if name.endswith("_" + p)), None)
        if platform is not None:
            name = name[:-len(platform) - 1]
        else:
            platform = constraints[tuple(kw["target_compatible_with"])]
        opts = "conlyopts" if "conlyopts" in kw else "cxxopts"
        t = platforms.setdefault(platform, {"libraries": []})
        t["libraries"].append({
            "name": name,
            "opts": opts,
            "srcs": kw["srcs"],
            "flags": kw.get(opts, []),
            "includes": kw["includes"],
            "local_defines": kw["local_defines"],
            "deps": kw["deps"],
        })
    for platform, t in platforms.items():
        t["genrules"] = genrules
        t["generated_headers"] = [h for h in headers["hdrs"]]
        t["textual"] = headers["textual_hdrs"]
        t["linkopts"] = branch(public["zink"]["linkopts"], platform)
        t["api_defines"] = branch(public["gallium_api"]["defines"], platform)
    return platforms


def merge_genrules(platforms):
    """Returns the genrules of every platform, which must agree: Mesa's code
    generators are the same Python on every platform."""
    merged = collections.OrderedDict()
    for platform, t in platforms.items():
        for name, g in t["genrules"].items():
            if name in merged and merged[name] != g:
                sys.exit(f"genrule {name} of {platform} differs from another platform's: "
                         "regenerate every platform's targets")
            merged[name] = g
    return collections.OrderedDict(sorted(merged.items(), key=lambda kv: kv[1]["outs"][0]))


def select(branches, default=None):
    """Returns a Starlark select of branches, by platform."""
    lines = ["select({"]
    for platform, value in branches.items():
        lines.append(f'        ":{platform}": {indented(value, 8)},')
    if default is not None:
        lines.append(f'        "//conditions:default": {indented(default, 8)},')
    lines.append("    })")
    return "\n".join(lines)


def indented(value, indent):
    if isinstance(value, list):
        return starlark_list(value, indent + 4)
    return json.dumps(value)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--source", required=True)
    p.add_argument("--build", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--version", required=True)
    args = p.parse_args()
    mesa = Mesa(args.source, args.build)
    platform = build_platform(mesa.build)
    platforms = read_build(args.out)
    kept = [p for p in platforms if p != platform]
    platforms[platform] = platform_targets(mesa, platform)
    platforms = collections.OrderedDict((p, platforms[p]) for p in PLATFORMS if p in platforms)
    genrules = merge_genrules(platforms)

    out = []
    out.append(f"""# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by tools/mesa/gen_mesa_build.py from meson builds of Mesa
# {args.version}, one of each platform; see tools/mesa/README.md. Do not edit.
#
# Zink, Mesa's Gallium driver on Vulkan, and what it needs of Mesa, which
# TTC's GPU renderer links.

load("@rules_cc//cc:defs.bzl", "cc_library")

package(default_visibility = ["//visibility:private"])

licenses(["notice"])

exports_files(["docs/license.rst"])
""")
    for name, constraints in PLATFORMS.items():
        if name in platforms:
            out.append(f"""config_setting(
    name = {json.dumps(name)},
    constraint_values = {starlark_list(constraints)},
)
""")
    for name, g in genrules.items():
        out.append(f"""genrule(
    name = {json.dumps(name)},
    srcs = {starlark_list(g["srcs"])},
    outs = {starlark_list(g["outs"])},
    cmd = {json.dumps(g["cmd"])},
    tools = ["@@//tools/mesa:run"],
)
""")
    excludes = [d + "/**" for d in UNUSED_HEADER_DIRS]
    generated_headers = sorted(set(h for t in platforms.values() for h in t["generated_headers"]))
    textual = sorted(set(h for t in platforms.values() for h in t["textual"]))
    out.append(f"""cc_library(
    name = "headers",
    hdrs = glob(
        [
            "include/**/*.h",
            "src/**/*.h",
            "src/**/*.hpp",
        ],
        exclude = {starlark_list(excludes, 12)},
    ) + {starlark_list(generated_headers)},
    textual_hdrs = {starlark_list(textual)},
)
""")
    names = collections.OrderedDict()
    for p, t in platforms.items():
        names[p] = []
        for lib in t["libraries"]:
            name = f'{lib["name"]}_{p}'
            names[p].append(":" + name)
            out.append(f"""cc_library(
    name = {json.dumps(name)},
    srcs = {starlark_list(lib["srcs"])},
    {lib["opts"]} = {starlark_list(lib["flags"])},
    includes = {starlark_list(lib["includes"])},
    local_defines = {starlark_list(lib["local_defines"])},
    target_compatible_with = {starlark_list(PLATFORMS[p])},
    deps = {json.dumps(lib["deps"])},
)
""")
    compatible = select(collections.OrderedDict((p, []) for p in platforms),
                        default=["@platforms//:incompatible"])
    out.append(f"""cc_library(
    name = "zink",
    linkopts = {select(collections.OrderedDict((p, t["linkopts"]) for p, t in platforms.items()))},
    target_compatible_with = {compatible},
    visibility = ["//visibility:public"],
    deps = {select(names)},
)
""")
    out.append(f"""# Gallium's interface, Zink's, NIR's, and virgl's protocol, for TTC's
# renderer.
cc_library(
    name = "gallium_api",
    defines = {select(collections.OrderedDict((p, t["api_defines"]) for p, t in platforms.items()))},
    includes = {starlark_list(API_INCLUDES)},
    target_compatible_with = {compatible},
    visibility = ["//visibility:public"],
    deps = [":headers"],
)
""")
    with open(args.out, "w") as f:
        f.write("\n".join(out))
    t = platforms[platform]
    print(f"{platform}: {len(t['genrules'])} genrules, {len(t['libraries'])} libraries, "
          f"{sum(len(l['srcs']) for l in t['libraries'])} sources; kept {', '.join(kept) or 'no other platform'}",
          file=sys.stderr)


if __name__ == "__main__":
    main()
