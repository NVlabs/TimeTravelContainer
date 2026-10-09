#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Writes THIRD_PARTY_NOTICES.md: the third-party components of
TimeTravelContainer, their licenses, and the texts of those licenses and of
the components' notice files.

It reads the modules go.mod requires from the Go module cache (`go mod
download` fetches any missing), the Go distribution's license from the Go
toolchain, the MCP server's npm dependencies from sdk/node_modules (run `npm
ci` in sdk/ first) and the Rust SDK's crates through `cargo metadata` (run
`cargo fetch` in sdk/rust first). It fetches from GitHub the license files of
the Vulkan headers that the GPU renderer compiles, and of MoltenVK, which
ttcd links on macOS, and of the components that MoltenVK's library holds, at
the revisions of the release that MODULE.bazel names.

Run it from the repository root whenever go.mod, an SDK's dependencies, the
renderer's Vulkan headers or venus-protocol, or MoltenVK change:
python3 tools/third_party_notices.py
"""

import collections
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


def bazel_patches():
    """Returns (component, patch) for each patch MODULE.bazel applies to the Go
    SDK or to a Go module."""
    with open("MODULE.bazel") as f:
        module = f.read()
    patches = []
    for call in re.finditer(r"(go_sdk\.from_file|go_deps\.module_override)\((.*?)\n\)", module, re.DOTALL):
        body = call.group(2)
        listed = re.search(r"patches\s*=\s*\[(.*?)\]", body, re.DOTALL)
        if not listed:
            continue
        if call.group(1) == "go_sdk.from_file":
            name = "Go standard library, runtime and toolchain"
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
    return [headers, venus]


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


def npm_components():
    with open("sdk/package-lock.json") as f:
        lock = json.load(f)
    components = []
    for key, package in sorted(lock["packages"].items()):
        if not key.startswith("node_modules/") or package.get("dev") or package.get("link"):
            continue
        name = key[len("node_modules/"):]
        directory = os.path.join("sdk", key)
        if not os.path.isdir(directory):
            sys.exit(f"{directory} is missing: run `npm ci` in sdk/")
        components.append(component(name, package["version"], directory, "npm, ttcx-mcp", package.get("license")))
    return components


def cargo_components():
    metadata = json.loads(run(["cargo", "metadata", "--offline", "--format-version", "1",
                               "--filter-platform", "x86_64-unknown-linux-gnu"], cwd="sdk/rust"))
    packages = {p["id"]: p for p in metadata["packages"]}
    nodes = {n["id"]: n for n in metadata["resolve"]["nodes"]}
    root = metadata["resolve"]["root"]
    seen, stack = set(), [root]
    while stack:
        current = stack.pop()
        if current in seen:
            continue
        seen.add(current)
        for dep in nodes[current]["deps"]:
            if any(kind.get("kind") in (None, "build") for kind in dep["dep_kinds"]):
                stack.append(dep["pkg"])
    components = []
    for package_id in sorted(seen, key=lambda i: packages[i]["name"]):
        if package_id == root:
            continue
        package = packages[package_id]
        directory = os.path.dirname(package["manifest_path"])
        components.append(component(package["name"], package["version"], directory, "crate, ttcx", package.get("license")))
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
    native = renderer_components() + moltenvk_components()
    npm = npm_components()
    cargo = cargo_components()
    out = []
    out.append("# Third-party notices\n")
    out.append(
        "TimeTravelContainer is built from gVisor and from the third-party components "
        "below. This file names each, with its license, and then reproduces the texts "
        "of those licenses and the components' notice files. It is generated by "
        "`tools/third_party_notices.py`; run it again when `go.mod` or an SDK's "
        "dependencies change.\n")
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
    out.append("`ttcd`'s GPU renderer compiles Vulkan's headers and the code that venus-protocol "
               "generates, which this repository holds; on macOS, `ttcd` also links MoltenVK's "
               "static library, of its release, with what that holds.\n")
    out.append(table(["Component", "Version", "License", "Use"], native))
    out.append("### Go modules\n")
    out.append("Every module `go.mod` requires. `ttc` and `ttcd` link those their packages "
               "import; the others build tests and tools, or only settle versions.\n")
    out.append(table(["Module", "Version", "License", "Requirement"], go))
    out.append("### SDK dependencies\n")
    out.append("Not in this repository or in TimeTravelContainer's binaries: npm fetches these "
               "when the MCP server (`sdk/mcp`) is installed, and Cargo when the Rust SDK "
               "(`sdk/rust`) is built. The TypeScript and Python SDKs have no runtime "
               "dependencies.\n")
    out.append(table(["Package", "Version", "License", "Used by"], npm + cargo))
    out.append("## License and notice texts\n")
    out.append("Components whose license or notice files differ only in spacing share one copy of the text.\n")
    groups = collections.OrderedDict()
    for c in [toolchain] + native + go + npm + cargo:
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
          f"{len(npm) + len(cargo)} SDK dependencies, {len(groups)} texts")


if __name__ == "__main__":
    main()
