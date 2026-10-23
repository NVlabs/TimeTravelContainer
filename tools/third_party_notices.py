#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Writes THIRD_PARTY_NOTICES.md: the third-party components of
TimeTravelContainer, their licenses, and the texts of those licenses and of
the components' notice files.

It reads the modules go.mod requires from the Go module cache (`go mod
download` fetches any missing) and the Go distribution's license from the Go
toolchain, which must be the official release that go.mod names, as Bazel's
is. It fetches from GitHub the license files of the Vulkan headers that the
GPU renderer compiles, and of MoltenVK, which ttcd links on macOS, and of the
components that MoltenVK's library holds, at the revisions of the release
that MODULE.bazel names. Of Mesa, whose Zink ttcd links on macOS, it reads
the tree that Bazel fetched, and the copyright notices of the files that
tools/mesa.BUILD compiles.

Run it from the repository root whenever go.mod, the renderer's Vulkan headers
or venus-protocol, MoltenVK, or Mesa change: python3 tools/third_party_notices.py
"""

import collections
import glob
import json
import os
import re
import subprocess
import sys
import urllib.request

OUTPUT = "THIRD_PARTY_NOTICES.md"

# The files that hold a component's license or notices.
LICENSE_FILE = re.compile(r"^(LICEN[CS]E|COPYING|NOTICE|PATENTS|UNLICENSE)([._-].*)?$", re.IGNORECASE)

# Phrases that identify a license text, in the order checked.
LICENSE_MARKERS = [
    ("Apache-2.0", [r"Apache License", r"Version 2\.0"]),
    ("MPL-2.0", [r"Mozilla Public License,? [Vv]ersion 2\.0"]),
    ("ISC", [r"Permission to use, copy, modify, and(/or)? distribute this software for any"]),
    ("BSL-1.0", [r"Boost Software License - Version 1\.0"]),
    ("MIT", [r"Permission is hereby granted, free of charge"]),
    ("BSD-3-Clause", [r"Redistribution and use in source and binary forms", r"(Neither the name|The names? of .{0,80} may not be used)"]),
    ("BSD-2-Clause", [r"Redistribution and use in source and binary forms"]),
    ("Unlicense", [r"This is free and unencumbered software released into the public domain"]),
    ("CC0-1.0", [r"CC0 1\.0 Universal"]),
]


def run(args, cwd=None):
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True).stdout


def classify(text):
    """Returns the SPDX identifiers of the licenses text holds."""
    found = []
    for spdx, patterns in LICENSE_MARKERS:
        if all(re.search(p, text) for p in patterns):
            if spdx == "BSD-2-Clause" and "BSD-3-Clause" in found:
                continue
            # The Boost license grants permission in the MIT license's words.
            if spdx == "MIT" and "BSL-1.0" in found:
                continue
            found.append(spdx)
    return found


def read_license_files(directory):
    """Returns (name, text) for each license or notice file in directory."""
    files = []
    for name in sorted(os.listdir(directory)):
        path = os.path.join(directory, name)
        if LICENSE_FILE.match(name) and os.path.isfile(path):
            with open(path, encoding="utf-8", errors="replace") as f:
                text = f.read()
            files.append((name, text.replace("\r\n", "\n").strip("\n") + "\n"))
    return files


def is_notice(name):
    return name.upper().startswith(("NOTICE", "PATENTS"))


def component(name, version, directory, kind, declared=None):
    return component_of_files(name, version, read_license_files(directory), directory, kind, declared)


def component_of_files(name, version, files, origin, kind, declared=None):
    licenses = []
    for file_name, text in files:
        if not is_notice(file_name):
            for spdx in classify(text):
                if spdx not in licenses:
                    licenses.append(spdx)
    if not any(not is_notice(f) for f, _ in files):
        sys.exit(f"{name} {version}: no license file in {origin}")
    if not licenses:
        sys.exit(f"{name} {version}: unrecognized license in {origin}")
    return {
        "name": name,
        "version": version,
        "kind": kind,
        "license": declared or " AND ".join(licenses),
        "files": files,
    }


def go_required_modules():
    """Returns (path, version, indirect) for each module go.mod requires."""
    modules = []
    block = False
    with open("go.mod") as f:
        for line in f:
            line = line.strip()
            if line.startswith("require ("):
                block = True
                continue
            if block and line == ")":
                block = False
                continue
            match = None
            if block:
                match = re.match(r"^(\S+)\s+(\S+)(\s*//\s*indirect)?", line)
            elif line.startswith("require "):
                match = re.match(r"^require\s+(\S+)\s+(\S+)(\s*//\s*indirect)?", line)
            if match and not line.startswith("//"):
                modules.append((match.group(1), match.group(2), bool(match.group(3))))
    return modules


def go_components():
    modules = go_required_modules()
    output = run(["go", "mod", "download", "-json"] + [f"{p}@{v}" for p, v, _ in modules])
    directories = {}
    decoder = json.JSONDecoder()
    index = 0
    while index < len(output):
        while index < len(output) and output[index].isspace():
            index += 1
        if index >= len(output):
            break
        info, index = decoder.raw_decode(output, index)
        if info.get("Error"):
            sys.exit(f"downloading {info['Path']}@{info['Version']}: {info['Error']}")
        directories[(info["Path"], info["Version"])] = info["Dir"]
    return [component(p, v, directories[(p, v)], "indirect" if indirect else "direct") for p, v, indirect in modules]


def go_toolchain_component():
    # Bazel's Go SDK is the release go.mod's go directive names.
    root = run(["go", "env", "GOROOT"]).strip()
    version = "go" + re.search(r"^go\s+(\S+)", open("go.mod").read(), re.MULTILINE).group(1)
    return component("Go standard library and runtime", version, root, "linked into ttc and ttcd")


# The components of http_archive repositories that ttc and ttcd link, by the
# repositories' names.
ARCHIVE_COMPONENTS = {"mesa": "Mesa"}


def bazel_patches():
    """Returns (component, patch) for each patch MODULE.bazel applies to the Go
    SDK, to a Go module, or to a component of ARCHIVE_COMPONENTS."""
    with open("MODULE.bazel") as f:
        module = f.read()
    patches = []
    for call in re.finditer(r"(go_sdk\.from_file|go_deps\.module_override|http_archive)\((.*?)\n\)", module, re.DOTALL):
        body = call.group(2)
        listed = re.search(r"patches\s*=\s*\[(.*?)\]", body, re.DOTALL)
        if not listed:
            continue
        if call.group(1) == "go_sdk.from_file":
            name = "Go standard library, runtime and toolchain"
        elif call.group(1) == "http_archive":
            name = ARCHIVE_COMPONENTS.get(re.search(r'name\s*=\s*"([^"]+)"', body).group(1))
            if name is None:
                continue
        else:
            name = re.search(r'path\s*=\s*"([^"]+)"', body).group(1)
        for label in re.findall(r'"//([^:"]*):([^"]+)"', listed.group(1)):
            patches.append((name, "/".join(part for part in label if part)))
    return patches


# The components that MoltenVK's static library holds, besides MoltenVK, as
# its symbols show, with their licenses where their files leave them unsaid:
# they are of the revisions in MoltenVK's ExternalRevisions.
MOLTENVK_HOLDS = [
    ("SPIRV-Cross", "KhronosGroup/SPIRV-Cross", None),
    ("SPIRV-Tools", "KhronosGroup/SPIRV-Tools", None),
    ("SPIRV-Headers", "KhronosGroup/SPIRV-Headers", None),
    ("Vulkan-Headers", "KhronosGroup/Vulkan-Headers", "Apache-2.0 OR MIT"),
    ("cereal", "USCiLab/cereal", None),
]


def fetch(url):
    with urllib.request.urlopen(url, timeout=120) as response:
        return response.read().decode("utf-8", errors="replace")


def github_license_files(repo, ref):
    """Returns (name, text) for each license or notice file of repo at ref:
    those at its root, and, where those only point to them, those in
    LICENSES/."""
    def files_in(path):
        entries = json.loads(fetch(f"https://api.github.com/repos/{repo}/contents/{path}?ref={ref}"))
        return [e["path"] for e in entries if e["type"] == "file"]

    def text(path):
        return fetch(f"https://raw.githubusercontent.com/{repo}/{ref}/{path}").replace("\r\n", "\n").strip("\n") + "\n"

    files = [(path, text(path)) for path in files_in("") if LICENSE_FILE.match(path)]
    if not any(classify(t) for p, t in files if not is_notice(p)):
        files += [(path, text(path)) for path in files_in("LICENSES")]
    return files


# The GPU renderer's code of others: Vulkan's headers, and what venus-protocol
# generates (venus/gen/venusgen.py).
VENUS = "runsc/ttc/gfxrenderer/venus"


def renderer_components():
    with open(f"{VENUS}/vk/include/vulkan/vulkan_core.h") as f:
        core = f.read()
    major, minor = re.search(r"VK_HEADER_VERSION_COMPLETE VK_MAKE_API_VERSION\(0, (\d+), (\d+), VK_HEADER_VERSION\)", core).groups()
    patch = re.search(r"#define VK_HEADER_VERSION (\d+)", core).group(1)
    tag = f"v{major}.{minor}.{patch}"
    headers_files = github_license_files("KhronosGroup/Vulkan-Headers", tag)
    headers = component_of_files("Vulkan-Headers", tag.lstrip("v"), headers_files, "KhronosGroup/Vulkan-Headers",
                                 "compiled into ttcd's GPU renderer", "Apache-2.0 OR MIT")
    # venus-protocol has no license file: what it generates names its
    # copyright holders and the MIT license, whose text Vulkan-Headers has.
    holders, versions = [], set()
    for root, _, names in os.walk(VENUS):
        for name in sorted(names):
            with open(os.path.join(root, name), encoding="utf-8", errors="replace") as f:
                text = f.read()
            versions.update(re.findall(r"from venus-protocol v(\d+(?:\.\d+)*)", text))
            if "generated by venus-protocol" in text and "SPDX-License-Identifier: MIT" in text:
                for holder in re.findall(r"^ \* (Copyright .+)$", text, re.MULTILINE):
                    if holder not in holders:
                        holders.append(holder)
    if len(versions) != 1 or not holders:
        sys.exit(f"venus-protocol: versions {sorted(versions)}, copyright holders {holders} in {VENUS}")
    mit = dict(headers_files)["LICENSES/MIT.txt"]
    text = "\n".join(sorted(holders)) + "\n\n" + mit[mit.index("Permission is hereby granted"):]
    venus = component_of_files("venus-protocol", versions.pop(), [("vn_protocol_renderer_*.h", text)],
                               VENUS, "generated code of ttcd's GPU renderer")
    # The virgl translator's SPIR-V constants are generated of Khronos's
    # grammar of SPIR-V and GLSL.std.450.h, as Mesa holds them, whose
    # copyright and license, MIT, the generated file's header names.
    gen = "runsc/ttc/gfxrenderer/virgl/spirv/spirv_gen.go"
    with open(gen) as f:
        generated = f.read()
    holder = re.search(r"^// SPDX-FileCopyrightText: (\d[^\n]* The Khronos Group Inc\.)$", generated, re.MULTILINE)
    grammar = re.search(r"from the grammar of SPIR-V (\d+\.\d+), revision (\d+)", generated)
    if not holder or not grammar or "// SPDX-License-Identifier: MIT" not in generated:
        sys.exit(f"{gen}: no Khronos copyright, license, or grammar version")
    text = "Copyright " + holder.group(1) + "\n\n" + mit[mit.index("Permission is hereby granted"):]
    grammar = component_of_files("SPIR-V grammar (SPIRV-Headers)", f"{grammar.group(1)} rev. {grammar.group(2)}",
                                 [("spirv.core.grammar.json, GLSL.std.450.h", text)], gen,
                                 "generated code of ttcd's GPU renderer")
    return [headers, venus, grammar]


def moltenvk_components():
    with open("MODULE.bazel") as f:
        module = f.read()
    tag = re.search(r'name = "moltenvk",.*?/releases/download/([^/"]+)/', module, re.DOTALL).group(1)
    repo = "KhronosGroup/MoltenVK"
    components = [component_of_files("MoltenVK", tag.lstrip("v"), github_license_files(repo, tag), repo,
                                     "linked into ttcd on macOS")]
    for name, held, declared in MOLTENVK_HOLDS:
        revision = fetch(f"https://raw.githubusercontent.com/{repo}/{tag}/ExternalRevisions/{name}_repo_revision").split()[0]
        components.append(component_of_files(name, revision[:12], github_license_files(held, revision), held,
                                             "in MoltenVK", declared))
    return components


# The components of others that the part of Mesa that ttcd links holds, by
# the files of theirs that tools/mesa.BUILD compiles, which Mesa's own
# copyright notices leave out.
MESA_FOREIGN = ("src/util/blake3/", "src/util/xxhash.h", "src/util/softfloat.", "src/c11/")


def mesa_tree():
    """Returns the directory of Mesa's tree that Bazel fetched."""
    run(["bazel", "fetch", "@mesa//:all"])
    base = run(["bazel", "info", "output_base"]).strip()
    trees = glob.glob(os.path.join(base, "external", "*mesa"))
    if len(trees) != 1:
        sys.exit(f"Mesa's tree: {trees}")
    return trees[0]


def mesa_license_text(tree, name):
    """Returns the text of Mesa's licenses/name, after its SPDX preamble."""
    with open(os.path.join(tree, "licenses", name)) as f:
        text = f.read()
    return text[text.index("License-Text:") + len("License-Text:"):].strip("\n") + "\n"


def comment_block(path, first, last):
    """Returns the lines of the comment at the top of path from the one
    with first through the one with last, without the comment's markers."""
    with open(path, encoding="utf-8", errors="replace") as f:
        lines = f.read().split("\n")
    start = next(i for i, l in enumerate(lines) if first in l)
    end = next(i for i in range(start, len(lines)) if last in lines[i])
    return "\n".join(re.sub(r"^\s*(/\*+|\*+/?|\*\*|//)? ?", "", l).rstrip() for l in lines[start:end + 1]) + "\n"


def mesa_components():
    with open("MODULE.bazel") as f:
        module = f.read()
    version = re.search(r'name = "mesa",.*?/mesa-(\d+\.\d+\.\d+)\.tar', module, re.DOTALL).group(1)
    tree = mesa_tree()
    with open("tools/mesa.BUILD") as f:
        build = f.read()
    # The files that it compiles, and the headers beside them, which they
    # include.
    compiled = sorted(set(p for p in re.findall(r'"((?:src|include)/[^"*]+\.(?:c|cpp|h|hpp))"', build)
                          if os.path.exists(os.path.join(tree, p))))
    headers = set()
    for d in set(os.path.dirname(p) for p in compiled):
        headers.update(os.path.relpath(h, tree) for h in glob.glob(os.path.join(tree, d, "*.h")))
    holders = set()
    for path in sorted(set(compiled) | headers):
        if path.startswith(MESA_FOREIGN):
            continue
        with open(os.path.join(tree, path), encoding="utf-8", errors="replace") as f:
            head = f.read(6000)
        for line in head.split("\n"):
            m = re.match(r"^\W*(Copyright\b[ :(©cC)]*\d.*?)\s*(\*/)?$", line.strip())
            if m:
                holders.add(" ".join(m.group(1).split()))
    if not holders:
        sys.exit(f"Mesa {version}: no copyright notices in {tree}")
    mit = mesa_license_text(tree, "MIT")
    mit = mit[mit.index("Permission is hereby granted"):]
    components = [component_of_files("Mesa", version, [("copyright notices of the files linked, and licenses/MIT",
                                                        "\n".join(sorted(holders)) + "\n\n" + mit)],
                                     tree, "linked into ttcd on macOS (Zink, Gallium, NIR, and utilities)")]
    with open(os.path.join(tree, "src/util/blake3/README")) as f:
        blake3 = re.search(r"version (\d+(?:\.\d+)+)", f.read()).group(1)
    components.append(component_of_files("BLAKE3", blake3, [("licenses/Apache-2.0", mesa_license_text(tree, "Apache-2.0"))],
                                         tree, "in Mesa", "CC0-1.0 OR Apache-2.0 OR Apache-2.0 WITH LLVM-exception"))
    with open(os.path.join(tree, "src/util/xxhash.h")) as f:
        xxhash = ".".join(re.findall(r"#define XXH_VERSION_(?:MAJOR|MINOR|RELEASE)\s+(\d+)", f.read()))
    components.append(component_of_files("xxHash", xxhash, [("src/util/xxhash.h", comment_block(
        os.path.join(tree, "src/util/xxhash.h"), "xxHash - Extremely Fast Hash algorithm", "POSSIBILITY OF SUCH DAMAGE"))],
        tree, "in Mesa"))
    softfloat = comment_block(os.path.join(tree, "src/util/softfloat.c"), "License for Berkeley SoftFloat",
                              "POSSIBILITY OF SUCH DAMAGE")
    components.append(component_of_files("Berkeley SoftFloat", re.search(r"Release (\w+)", softfloat).group(1),
                                          [("src/util/softfloat.c", softfloat)], tree, "in Mesa"))
    with open(os.path.join(tree, "src/c11/impl/threads_posix.c")) as f:
        c11 = re.search(r"Copyright .+?\.", f.read()).group(0)
    components.append(component_of_files("C11 threads emulation (yohhoy)", version,
                                          [("src/c11/impl/threads_posix.c, licenses/BSL-1.0",
                                            c11 + "\n\n" + mesa_license_text(tree, "BSL-1.0"))],
                                          tree, "in Mesa"))
    # Vulkan's headers, which Zink compiles, carry no license file of their
    # own in Mesa's tree.
    with open(os.path.join(tree, "include/vulkan/vulkan_core.h")) as f:
        core = f.read()
    major, minor = re.search(r"VK_HEADER_VERSION_COMPLETE VK_MAKE_API_VERSION\(0, (\d+), (\d+), VK_HEADER_VERSION\)", core).groups()
    tag = f"v{major}.{minor}." + re.search(r"#define VK_HEADER_VERSION (\d+)", core).group(1)
    components.append(component_of_files("Vulkan-Headers", tag.lstrip("v"),
                                         github_license_files("KhronosGroup/Vulkan-Headers", tag),
                                         "KhronosGroup/Vulkan-Headers", "in Mesa", "Apache-2.0 OR MIT"))
    return components


def gvisor_texts():
    """Returns the MIT and BSD texts that LICENSE notes some of gVisor's files carry."""
    with open("LICENSE") as f:
        sections = f.read().split("------------------")
    return [section.strip("\n") + "\n" for section in sections[1:]]


def table(header, components):
    rows = ["| " + " | ".join(header) + " |", "|" + "---|" * len(header)]
    for c in components:
        rows.append(f"| {c['name']} | {c['version']} | {c['license']} | {c['kind']} |")
    return "\n".join(rows) + "\n"


def fence(text):
    return "````text\n" + text + "````\n"


def main():
    if not os.path.exists("go.mod") or not os.path.isdir("runsc"):
        sys.exit("run from the repository root")
    go = go_components()
    toolchain = go_toolchain_component()
    native = renderer_components() + moltenvk_components() + mesa_components()
    out = []
    out.append("# Third-party notices\n")
    out.append(
        "TimeTravelContainer is built from gVisor and from the third-party components "
        "below. This file names each, with its license, and then reproduces the texts "
        "of those licenses and the components' notice files. It is generated by "
        "`tools/third_party_notices.py`; run it again when `go.mod` or the C and C++ "
        "components change.\n")
    out.append("## gVisor\n")
    out.append(
        "TimeTravelContainer is a modified version of [gVisor](https://github.com/google/gvisor), "
        "Copyright The gVisor Authors, licensed under the Apache License 2.0; some of its "
        "files carry the MIT or a BSD license instead, as their headers say. Files NVIDIA "
        "added or changed say so in their headers. The Apache License 2.0 is in "
        "[LICENSE](LICENSE); the MIT and BSD texts follow below.\n")
    for text in gvisor_texts():
        out.append(fence(text))
    out.append("## Components\n")
    out.append("### Go\n")
    out.append(f"The Go standard library and runtime, {toolchain['version']}, are linked into "
               f"`ttc` and `ttcd`: {toolchain['license']}, with Google's patent grant.\n")
    out.append("### Modified components\n")
    out.append("Bazel builds apply these patches to the components they name.\n")
    out.append("\n".join(["| Component | Patch |", "|---|---|"] +
                         [f"| {name} | [{patch}]({patch}) |" for name, patch in bazel_patches()]) + "\n")
    out.append("### C and C++ components\n")
    out.append("`ttcd`'s GPU renderer compiles Vulkan's headers, the code that venus-protocol "
               "generates, and constants generated of Khronos's SPIR-V grammar, which this "
               "repository holds; on macOS, `ttcd` also links MoltenVK's static library, of its "
               "release, with what that holds, and Zink and what it needs of Mesa, which Bazel "
               "builds from Mesa's release, with what that holds.\n")
    out.append(table(["Component", "Version", "License", "Use"], native))
    out.append("### Go modules\n")
    out.append("Every module `go.mod` requires. `ttc` and `ttcd` link those their packages "
               "import; the others build tests and tools, or only settle versions.\n")
    out.append(table(["Module", "Version", "License", "Requirement"], go))
    out.append("## License and notice texts\n")
    out.append("Components whose license or notice files differ only in spacing share one copy of the text.\n")
    groups = collections.OrderedDict()
    for c in [toolchain] + native + go:
        for file_name, text in c["files"]:
            key = " ".join(text.split())
            groups.setdefault(key, (text, file_name, []))[2].append(f"{c['name']} {c['version']} ({file_name})")
    for text, file_name, owners in groups.values():
        kinds = classify(text)
        if kinds and not is_notice(file_name):
            title = ", ".join(kinds)
        else:
            title = "Patent grant" if file_name.upper().startswith("PATENTS") else "Notice"
        out.append(f"### {title}\n")
        out.append("Of " + "; ".join(owners) + ".\n")
        out.append(fence(text))
    with open(OUTPUT, "w") as f:
        f.write("\n".join(out))
    print(f"wrote {OUTPUT}: {len(go)} Go modules, {len(native)} C and C++ components, "
          f"{len(groups)} texts")


if __name__ == "__main__":
    main()
