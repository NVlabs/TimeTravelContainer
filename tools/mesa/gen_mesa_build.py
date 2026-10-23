# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Writes the Bazel BUILD file of the part of Mesa that TTC links.

TTC's GPU renderer links Zink, Mesa's Gallium driver on Vulkan, and what it
needs of Mesa (tools/mesa.BUILD). Mesa builds with meson; this reads a meson
build of Mesa, configured as README.md says, and writes the same
compilations as Bazel's cc_library targets, and Mesa's code generators as
genrules, which run them by //tools/mesa:run.

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

LINKOPTS = [
    "-framework CoreFoundation",
    "-framework Foundation",
    "-framework IOSurface",
    "-framework Metal",
    "-framework QuartzCore",
    "-lc++",
    "-lz",
]


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


def compile_groups(mesa):
    """Returns the compilations of each library, grouped by their flags."""
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
                    defines.append(a[2:])
                    continue
                if DROPPED_FLAGS.match(a):
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


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--source", required=True)
    p.add_argument("--build", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--version", required=True)
    args = p.parse_args()
    mesa = Mesa(args.source, args.build)
    groups, order_only = compile_groups(mesa)
    commands = needed_commands(mesa, groups, order_only)

    out = []
    out.append(f"""# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# GENERATED by tools/mesa/gen_mesa_build.py from a meson build of Mesa
# {args.version}; see tools/mesa/README.md. Do not edit.
#
# Zink, Mesa's Gallium driver on Vulkan, and what it needs of Mesa, which
# TTC's GPU renderer links on macOS.

load("@rules_cc//cc:defs.bzl", "cc_library")

package(default_visibility = ["//visibility:private"])

licenses(["notice"])

exports_files(["docs/license.rst"])
""")
    generated_headers = []
    for e in commands:
        name, srcs, outs, cmd = genrule(mesa, e)
        generated_headers += [o for o in outs if o.endswith(".h")]
        out.append(f"""genrule(
    name = {json.dumps(name)},
    srcs = {starlark_list(srcs)},
    outs = {starlark_list(outs)},
    cmd = {json.dumps(cmd)},
    tools = ["@@//tools/mesa:run"],
)
""")
    excludes = []
    for d in UNUSED_HEADER_DIRS:
        excludes.append(d + "/**")
    textual = included_sources(mesa, groups, commands)
    out.append(f"""cc_library(
    name = "headers",
    hdrs = glob(
        [
            "include/**/*.h",
            "src/**/*.h",
            "src/**/*.hpp",
        ],
        exclude = {starlark_list(excludes, 12)},
    ) + {starlark_list(sorted(set(generated_headers)))},
    textual_hdrs = {starlark_list(textual)},
    target_compatible_with = ["@platforms//os:macos"],
)
""")
    names = []
    api_defines = None
    counts = collections.Counter(k[0] for k in groups)
    seen = collections.Counter()
    for (lib, lang, includes, defines, flags), srcs in groups.items():
        seen[lib] += 1
        name = lib if counts[lib] == 1 else f"{lib}_{seen[lib]}"
        names.append(name)
        opts = "conlyopts" if lang == "c" else "cxxopts"
        if lib == "zink_driver" and lang == "c" and api_defines is None:
            api_defines = defines
        deps = [":headers"]
        if lib == "zink_driver":
            deps.append("@moltenvk//:mvk_headers")
        out.append(f"""cc_library(
    name = {json.dumps(name)},
    srcs = {starlark_list(sorted(srcs))},
    {opts} = {starlark_list(list(flags))},
    includes = {starlark_list(list(includes))},
    local_defines = {starlark_list([shell_word(d) for d in defines])},
    target_compatible_with = ["@platforms//os:macos"],
    deps = {json.dumps(deps)},
)
""")
    out.append(f"""cc_library(
    name = "zink",
    linkopts = {starlark_list(LINKOPTS)},
    target_compatible_with = ["@platforms//os:macos"],
    visibility = ["//visibility:public"],
    deps = {starlark_list([":" + n for n in names])},
)
""")
    # Those of Zink's defines that configure Mesa's headers, which code that
    # includes them must agree with.
    api_defines = [d for d in api_defines
                   if re.match(r"^(HAVE_|USE_)", d) and not d.startswith(("HAVE_OPENGL", "HAVE_ZINK"))]
    out.append(f"""# Gallium's interface, Zink's, NIR's, and virgl's protocol, for TTC's
# renderer.
cc_library(
    name = "gallium_api",
    defines = {starlark_list([shell_word(d) for d in api_defines])},
    includes = {starlark_list(API_INCLUDES)},
    target_compatible_with = ["@platforms//os:macos"],
    visibility = ["//visibility:public"],
    deps = [":headers"],
)
""")
    with open(args.out, "w") as f:
        f.write("\n".join(out))
    print(f"{len(commands)} genrules, {len(groups)} libraries, "
          f"{sum(len(s) for s in groups.values())} sources", file=sys.stderr)


if __name__ == "__main__":
    main()
