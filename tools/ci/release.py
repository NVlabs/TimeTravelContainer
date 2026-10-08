#!/usr/bin/env python3

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""TTC's releases on GitLab, for its CI pipeline (.gitlab-ci.yml).

  release.py last          prints the tag and commit of the last daily
                           release, or nothing if there is none
  release.py publish DIR   uploads the files of DIR, and their SHA256SUMS, to
                           the project's generic package registry as package
                           ttc, version RELEASE_TAG, and makes the release
                           RELEASE_TAG of them, which makes its tag at
                           CI_COMMIT_SHA; RELEASE_NOTES names its description

It authenticates with the job's token, CI_JOB_TOKEN. A release's files keep
the names that install.sh downloads (ttc-linux-amd64, ttcd-linux-amd64, ...),
and each is a direct asset of the release: the latest's is at
<project>/-/releases/permalink/latest/downloads/<file>.
"""

import hashlib
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

PACKAGE = "ttc"
TAG_PREFIX = "daily-"


def env(name):
    value = os.environ.get(name)
    if not value:
        sys.exit(f"release.py: {name} is not set; it runs in a GitLab CI job")
    return value


def context():
    ctx = ssl.create_default_context()
    ca = os.environ.get("CI_SERVER_TLS_CA_FILE")
    if ca and os.path.exists(ca):
        ctx.load_verify_locations(ca)
    return ctx


def request(method, url, data=None, headers=None, attempts=4):
    headers = dict(headers or {})
    headers["JOB-TOKEN"] = env("CI_JOB_TOKEN")
    for attempt in range(attempts):
        body = data
        if hasattr(data, "seek"):
            data.seek(0)
        req = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, context=context(), timeout=600) as resp:
                content = resp.read()
                return json.loads(content) if content else None
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")
            # The server's errors other than overload are the request's.
            if e.code < 500 or attempt == attempts - 1:
                sys.exit(f"release.py: {method} {url}: {e.code} {detail}")
        except (urllib.error.URLError, TimeoutError) as e:
            if attempt == attempts - 1:
                sys.exit(f"release.py: {method} {url}: {e}")
        time.sleep(2 ** attempt)
    return None


def project_url(path):
    return f"{env('CI_API_V4_URL')}/projects/{env('CI_PROJECT_ID')}/{path}"


def last():
    releases = request("GET", project_url("releases?order_by=released_at&sort=desc&per_page=50"))
    for release in releases or []:
        if release["tag_name"].startswith(TAG_PREFIX):
            print(release["tag_name"], release["commit"]["id"])
            return


def publish(directory):
    tag = env("RELEASE_TAG")
    files = sorted(
        name
        for name in os.listdir(directory)
        if name != "SHA256SUMS" and os.path.isfile(os.path.join(directory, name))
    )
    if not files:
        sys.exit(f"release.py: {directory} has no files to release")
    sums = []
    for name in files:
        with open(os.path.join(directory, name), "rb") as f:
            sums.append(f"{hashlib.sha256(f.read()).hexdigest()}  {name}\n")
    with open(os.path.join(directory, "SHA256SUMS"), "w") as f:
        f.writelines(sums)
    files.append("SHA256SUMS")

    package = project_url(f"packages/generic/{PACKAGE}/{urllib.parse.quote(tag)}")
    links = []
    for name in files:
        path = os.path.join(directory, name)
        with open(path, "rb") as f:
            print(f"uploading {name} ({os.path.getsize(path)} bytes)", file=sys.stderr)
            request(
                "PUT",
                f"{package}/{urllib.parse.quote(name)}",
                data=f,
                headers={"Content-Length": str(os.path.getsize(path))},
            )
        links.append({
            "name": name,
            "url": f"{package}/{urllib.parse.quote(name)}",
            "direct_asset_path": f"/{name}",
            "link_type": "package",
        })

    notes = ""
    notes_file = os.environ.get("RELEASE_NOTES")
    if notes_file and os.path.exists(notes_file):
        with open(notes_file) as f:
            notes = f.read()
    release = {
        "name": f"TimeTravelContainer {tag}",
        "tag_name": tag,
        "ref": env("CI_COMMIT_SHA"),
        "description": notes or f"Daily build of {env('CI_COMMIT_SHA')}.",
        "assets": {"links": links},
    }
    request(
        "POST",
        project_url("releases"),
        data=json.dumps(release).encode(),
        headers={"Content-Type": "application/json"},
    )
    print(f"released {tag}: {env('CI_PROJECT_URL')}/-/releases/{urllib.parse.quote(tag)}", file=sys.stderr)


def main():
    if len(sys.argv) == 2 and sys.argv[1] == "last":
        last()
    elif len(sys.argv) == 3 and sys.argv[1] == "publish":
        publish(sys.argv[2])
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
