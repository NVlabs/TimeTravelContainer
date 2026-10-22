#!/usr/bin/env python3

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""TTC's daily releases, for its CI pipelines on GitLab (.gitlab-ci.yml) and
GitHub (.github/workflows/ttc-release.yml).

  release.py last          prints the tag and commit of the last daily
                           release, or nothing if there is none
  release.py publish DIR   makes the release RELEASE_TAG, and its tag at the
                           pipeline's commit, of the files of DIR and their
                           SHA256SUMS, with RELEASE_NOTES naming its
                           description; then deletes the daily releases older
                           than TTC_RELEASE_KEEP_DAYS (default 30), keeping
                           the newest whatever its age
  release.py prune         deletes those releases alone

On GitLab it authenticates with the job's token, CI_JOB_TOKEN. The files go to
the project's generic package registry, as package ttc, version RELEASE_TAG,
and each is a direct asset of the release: the latest's is at
<project>/-/releases/permalink/latest/downloads/<file>. A release that expires
loses its package too, and its tag if the token may delete tags.

On GitHub it authenticates with the workflow's token, GITHUB_TOKEN, which
needs the contents: write permission. Each file is an asset of the release,
which is the latest: the latest's are at
<repository>/releases/latest/download/<file>. A release that expires loses its
tag too.
"""

import datetime
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
        sys.exit(f"release.py: {name} is not set; it runs in a GitLab CI job or a GitHub Actions workflow")
    return value


def keep_days():
    value = os.environ.get("TTC_RELEASE_KEEP_DAYS") or "30"
    try:
        days = int(value)
    except ValueError:
        sys.exit(f"release.py: TTC_RELEASE_KEEP_DAYS={value!r} is not a number of days")
    if days < 1:
        sys.exit(f"release.py: TTC_RELEASE_KEEP_DAYS={value!r}: keep releases one day at least")
    return days


def context():
    ctx = ssl.create_default_context()
    ca = os.environ.get("CI_SERVER_TLS_CA_FILE")
    if ca and os.path.exists(ca):
        ctx.load_verify_locations(ca)
    return ctx


def request(method, url, auth, data=None, headers=None, attempts=4, missing_ok=False, fatal=True):
    """Sends a request, retried where the server is overloaded, and returns
    its JSON response. A 404 returns None if missing_ok; another error ends
    the program if fatal, and returns False otherwise."""
    headers = dict(headers or {})
    headers.update(auth)
    for attempt in range(attempts):
        if hasattr(data, "seek"):
            data.seek(0)
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, context=context(), timeout=600) as resp:
                content = resp.read()
                return json.loads(content) if content else None
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")
            if e.code == 404 and missing_ok:
                return None
            # The server's errors other than overload are the request's.
            if e.code < 500 or attempt == attempts - 1:
                message = f"release.py: {method} {url}: {e.code} {detail}"
                if fatal:
                    sys.exit(message)
                print(message, file=sys.stderr)
                return False
        except (urllib.error.URLError, TimeoutError) as e:
            if attempt == attempts - 1:
                message = f"release.py: {method} {url}: {e}"
                if fatal:
                    sys.exit(message)
                print(message, file=sys.stderr)
                return False
        time.sleep(2 ** attempt)
    return None


def checksums(directory):
    """Writes the SHA256SUMS of the files of directory, and returns their
    names and its."""
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
    return files + ["SHA256SUMS"]


def notes(commit):
    notes_file = os.environ.get("RELEASE_NOTES")
    if notes_file and os.path.exists(notes_file):
        with open(notes_file) as f:
            text = f.read()
        if text:
            return text
    return f"Daily build of {commit}."


def parse_time(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))


def expired(releases):
    """Returns the daily releases, of (tag, created, ...) tuples, that are
    older than the days to keep, but for the newest."""
    daily = sorted((r for r in releases if r[0].startswith(TAG_PREFIX)), key=lambda r: r[1], reverse=True)
    cutoff = datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=keep_days())
    return [r for r in daily[1:] if r[1] < cutoff]


class GitLab:
    def __init__(self):
        self.auth = {"JOB-TOKEN": env("CI_JOB_TOKEN")}

    def url(self, path):
        return f"{env('CI_API_V4_URL')}/projects/{env('CI_PROJECT_ID')}/{path}"

    def releases(self):
        releases, page = [], 1
        while True:
            batch = request("GET", self.url(f"releases?order_by=released_at&sort=desc&per_page=100&page={page}"), self.auth)
            releases.extend(batch or [])
            if not batch or len(batch) < 100:
                return releases
            page += 1

    def last(self):
        for release in self.releases():
            if release["tag_name"].startswith(TAG_PREFIX):
                print(release["tag_name"], release["commit"]["id"])
                return

    def publish(self, directory):
        tag = env("RELEASE_TAG")
        files = checksums(directory)
        package = self.url(f"packages/generic/{PACKAGE}/{urllib.parse.quote(tag)}")
        links = []
        for name in files:
            path = os.path.join(directory, name)
            with open(path, "rb") as f:
                print(f"uploading {name} ({os.path.getsize(path)} bytes)", file=sys.stderr)
                request(
                    "PUT",
                    f"{package}/{urllib.parse.quote(name)}",
                    self.auth,
                    data=f,
                    headers={"Content-Length": str(os.path.getsize(path))},
                )
            links.append({
                "name": name,
                "url": f"{package}/{urllib.parse.quote(name)}",
                "direct_asset_path": f"/{name}",
                "link_type": "package",
            })
        commit = env("CI_COMMIT_SHA")
        release = {
            "name": f"TimeTravelContainer {tag}",
            "tag_name": tag,
            "ref": commit,
            "description": notes(commit),
            "assets": {"links": links},
        }
        request(
            "POST",
            self.url("releases"),
            self.auth,
            data=json.dumps(release).encode(),
            headers={"Content-Type": "application/json"},
        )
        print(f"released {tag}: {env('CI_PROJECT_URL')}/-/releases/{urllib.parse.quote(tag)}", file=sys.stderr)

    def prune(self):
        old = expired([(r["tag_name"], parse_time(r["released_at"] or r["created_at"])) for r in self.releases()])
        for tag, created in old:
            print(f"expiring {tag}, of {created:%Y-%m-%d}", file=sys.stderr)
            quoted = urllib.parse.quote(tag, safe="")
            # Its package, the release, and then its tag, if the token may.
            packages = request(
                "GET",
                self.url(f"packages?package_type=generic&package_name={PACKAGE}&per_page=100"),
                self.auth,
                fatal=False,
            ) or []
            for package in packages:
                if package.get("name") == PACKAGE and package.get("version") == tag:
                    request("DELETE", self.url(f"packages/{package['id']}"), self.auth, fatal=False)
            request("DELETE", self.url(f"releases/{quoted}"), self.auth, missing_ok=True, fatal=False)
            request("DELETE", self.url(f"repository/tags/{quoted}"), self.auth, missing_ok=True, fatal=False)


class GitHub:
    def __init__(self):
        self.auth = {
            "Authorization": f"Bearer {env('GITHUB_TOKEN')}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
        }

    def url(self, path):
        return f"{env('GITHUB_API_URL')}/repos/{env('GITHUB_REPOSITORY')}/{path}"

    def releases(self):
        releases, page = [], 1
        while True:
            batch = request("GET", self.url(f"releases?per_page=100&page={page}"), self.auth)
            releases.extend(r for r in batch or [] if not r.get("draft"))
            if not batch or len(batch) < 100:
                return releases
            page += 1

    def tag_commit(self, tag):
        ref = request("GET", self.url(f"git/ref/tags/{urllib.parse.quote(tag, safe='')}"), self.auth, missing_ok=True)
        if not ref:
            return None
        obj = ref["object"]
        # An annotated tag's ref names the tag object, which names the commit.
        while obj["type"] == "tag":
            obj = request("GET", self.url(f"git/tags/{obj['sha']}"), self.auth)["object"]
        return obj["sha"]

    def last(self):
        daily = [r for r in self.releases() if r["tag_name"].startswith(TAG_PREFIX)]
        daily.sort(key=lambda r: parse_time(r["created_at"]), reverse=True)
        for release in daily:
            commit = self.tag_commit(release["tag_name"])
            if commit:
                print(release["tag_name"], commit)
                return

    def publish(self, directory):
        tag = env("RELEASE_TAG")
        files = checksums(directory)
        commit = env("GITHUB_SHA")
        # A draft first, which makes no tag, so that a release with an asset
        # missing is never published.
        release = request(
            "POST",
            self.url("releases"),
            self.auth,
            data=json.dumps({
                "tag_name": tag,
                "target_commitish": commit,
                "name": f"TimeTravelContainer {tag}",
                "body": notes(commit),
                "draft": True,
            }).encode(),
            headers={"Content-Type": "application/json"},
        )
        uploads = release["upload_url"].split("{", 1)[0]
        for name in files:
            path = os.path.join(directory, name)
            with open(path, "rb") as f:
                print(f"uploading {name} ({os.path.getsize(path)} bytes)", file=sys.stderr)
                request(
                    "POST",
                    f"{uploads}?name={urllib.parse.quote(name)}",
                    self.auth,
                    data=f,
                    headers={
                        "Content-Type": "application/octet-stream",
                        "Content-Length": str(os.path.getsize(path)),
                    },
                )
        request(
            "PATCH",
            self.url(f"releases/{release['id']}"),
            self.auth,
            data=json.dumps({"draft": False, "make_latest": "true"}).encode(),
            headers={"Content-Type": "application/json"},
        )
        print(f"released {tag}: {env('GITHUB_SERVER_URL')}/{env('GITHUB_REPOSITORY')}/releases/tag/{urllib.parse.quote(tag)}", file=sys.stderr)

    def prune(self):
        old = expired([(r["tag_name"], parse_time(r["created_at"]), r["id"]) for r in self.releases()])
        for tag, created, release_id in old:
            print(f"expiring {tag}, of {created:%Y-%m-%d}", file=sys.stderr)
            request("DELETE", self.url(f"releases/{release_id}"), self.auth, missing_ok=True, fatal=False)
            request("DELETE", self.url(f"git/refs/tags/{urllib.parse.quote(tag, safe='')}"), self.auth, missing_ok=True, fatal=False)


def forge():
    if os.environ.get("GITHUB_ACTIONS") == "true":
        return GitHub()
    return GitLab()


def main():
    if len(sys.argv) == 2 and sys.argv[1] == "last":
        forge().last()
    elif len(sys.argv) == 3 and sys.argv[1] == "publish":
        f = forge()
        f.publish(sys.argv[2])
        f.prune()
    elif len(sys.argv) == 2 and sys.argv[1] == "prune":
        forge().prune()
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
