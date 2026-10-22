#!/usr/bin/env python3
"""在独立的官方 Tag 检出中重放补丁，记录来源并打包二进制。"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import struct
import subprocess
import sys
import tarfile


ROOT = Path(__file__).resolve().parent.parent
MANIFEST = ROOT / "custom/manifest.json"
STATE = "koko-custom-release.json"
STABLE_TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+\Z")


def run(args, cwd=None):
    env = dict(os.environ, GOWORK="off")
    env.setdefault("GOMAXPROCS", "2")
    env.setdefault("GOMEMLIMIT", "1GiB")
    with subprocess.Popen(args, cwd=cwd, env=env, text=True, stdout=subprocess.PIPE, start_new_session=True) as process:
        try:
            output, _ = process.communicate(timeout=300)
        except BaseException:
            # 超时或中断时一并回收编译器子进程，避免后台构建继续占用资源。
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            process.communicate()
            raise
        if process.returncode:
            raise subprocess.CalledProcessError(process.returncode, args, output)
        return output.strip()


def manifest():
    data = json.loads(MANIFEST.read_text())
    if data["upstream_repository"] != "https://github.com/jumpserver/koko.git":
        raise ValueError("upstream repository must be the official jumpserver/koko")
    return data


def patch_paths(data):
    base = MANIFEST.parent.resolve()
    paths = [(base / name).resolve() for name in data["patches"]]
    if not paths or any(base not in path.parents or not path.is_file() for path in paths):
        raise ValueError("patches must be existing files inside custom/")
    return paths


def patch_fingerprint(data):
    digest = hashlib.sha256()
    paths = [MANIFEST, Path(__file__).resolve(), ROOT / ".github/workflows/build-ghcr-image.yml"] + patch_paths(data)
    for path in sorted(paths):
        digest.update(str(path.relative_to(ROOT)).encode() + b"\0" + path.read_bytes())
    return digest.hexdigest()


def state_path(source):
    # 工作树和普通检出都通过 Git 定位，元数据不混入上游源码差异。
    git_dir = Path(run(["git", "rev-parse", "--absolute-git-dir"], source))
    return git_dir / STATE


def write_state(source, data):
    state_path(source).write_text(json.dumps(data, indent=2) + "\n")


def read_state(source):
    if source == ROOT or source in ROOT.parents:
        raise ValueError("candidate must be separate from the maintenance checkout")
    data = json.loads(state_path(source).read_text())
    if run(["git", "rev-parse", "HEAD"], source) != data["upstream_sha"]:
        raise ValueError("candidate HEAD changed; prepare a new source directory")
    if data["patch_set_sha256"] != patch_fingerprint(manifest()):
        raise ValueError("patch set or release tooling changed; prepare a new candidate")
    return data


def prepare(args):
    data = manifest()
    tag = args.tag or data["default_tag"]
    revision = args.revision if args.revision is not None else data["patch_revision"]
    if not STABLE_TAG.fullmatch(tag) or revision < 1:
        raise ValueError("require a stable vX.Y.Z tag and a positive patch revision")
    source = args.destination.resolve()
    if source.exists() or source == ROOT or source in ROOT.parents:
        raise ValueError("destination must be a new directory outside the maintenance checkout itself")
    paths = patch_paths(data)
    for patch in paths:
        # 防止修改维护分支的功能后忘记重新导出补丁，构建过期实现。
        run(["git", "apply", "--reverse", "--check", str(patch)], ROOT)
    source.parent.mkdir(parents=True, exist_ok=True)
    run(["git", "init", "--quiet", str(source)])
    run(["git", "remote", "add", "upstream", data["upstream_repository"]], source)
    run(["git", "fetch", "--depth=1", "--no-tags", "upstream", "refs/tags/" + tag], source)
    run(["git", "switch", "--create", "custom/" + tag, "FETCH_HEAD"], source)
    upstream_sha = run(["git", "rev-parse", "HEAD"], source)
    baseline = data["validated_baseline"]
    if tag == baseline["tag"] and upstream_sha != baseline["sha"]:
        raise ValueError("validated upstream tag moved; review its new commit before continuing")
    if not re.search(r"(?m)^module github\.com/jumpserver/koko\s*$", (source / "go.mod").read_text()):
        raise ValueError("upstream Go module changed; manual adaptation required")
    for patch in paths:
        run(["git", "apply", "--check", str(patch)], source)
        run(["git", "apply", "--index", str(patch)], source)
    state = {
        "upstream_version": tag,
        "upstream_sha": upstream_sha,
        "version": f"{tag}-fwd.{revision}",
        "custom_patch_sha": run(["git", "rev-parse", "HEAD"], ROOT),
        "custom_source_dirty": bool(run(["git", "status", "--porcelain"], ROOT)),
        "patch_set_sha256": patch_fingerprint(data),
        "patches": data["patches"],
        "dependency_fixes": data["dependency_fixes"],
        "fixes_checked": False,
    }
    write_state(source, state)
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"version={state['version']}\nupstream_tag={tag}\nupstream_sha={upstream_sha}\n")
    print(f"Prepared {tag} ({upstream_sha}) at {source}; review staged forwarding patch before release.")


def version_key(value):
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)(?:-(.+))?", value)
    if not match:
        raise ValueError("unrecognized Go module version: " + value)
    return tuple(int(match[i]) for i in (1, 2, 3)) + (match[4] is None,)


def module_info(source, name):
    info = json.loads(run(["go", "list", "-mod=readonly", "-m", "-json", name], source))
    if info.get("Replace"):
        raise ValueError("module replacement requires manual fix review: " + name)
    return info


def dependencies(args):
    source = args.source.resolve()
    state = read_state(source)
    if state["fixes_checked"]:
        raise ValueError("dependency fixes already checked; use a new candidate for another dependency change")
    results = []
    for fix in state["dependency_fixes"]:
        name, minimum = fix["module"], fix["minimum"]
        before = module_info(source, name)["Version"]
        needed = version_key(before) < version_key(minimum)
        if needed:
            # 让目标版本的 Go 工具解析必要的传递依赖，不搬运另一版本的 go.mod。
            run(["go", "get", name + "@" + minimum], source)
        after = module_info(source, name)["Version"]
        if version_key(after) < version_key(minimum):
            raise ValueError("dependency fix did not resolve to a fixed version: " + name)
        result = dict(fix, before=before, resolved=after, status="applied" if needed else "already-present")
        results.append(result)
        print(f"{fix['id']}: {result['status']} ({before} -> {after})")
    state["dependency_fixes"] = results
    state["dependency_files_sha256"] = {name: sha256(source / name) for name in ("go.mod", "go.sum")}
    state["fixes_checked"] = True
    write_state(source, state)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def check_binary(path):
    with path.open("rb") as stream:
        header = stream.read(64)
        if len(header) != 64 or header[:6] != b"\x7fELF\x02\x01" or struct.unpack_from("<H", header, 18)[0] != 62:
            raise ValueError("binary must be a Linux amd64 ELF")
        offset = struct.unpack_from("<Q", header, 32)[0]
        size, count = struct.unpack_from("<HH", header, 54)
        if size < 4 or count > 1024 or offset + size * count > path.stat().st_size:
            raise ValueError("invalid ELF program headers")
        for index in range(count):
            stream.seek(offset + index * size)
            if struct.unpack("<I", stream.read(4))[0] == 3:
                raise ValueError("binary has a dynamic interpreter; adapt the upstream build before release")


def build(args):
    source = args.source.resolve()
    state = read_state(source)
    if not state["fixes_checked"]:
        raise ValueError("run dependencies before packaging")
    if run(["git", "ls-files", "--others", "--exclude-standard"], source):
        raise ValueError("candidate has untracked source files; review and stage them before packaging")
    if any(sha256(source / name) != digest for name, digest in state["dependency_files_sha256"].items()):
        raise ValueError("module files changed after dependency checks; prepare a new candidate")
    for fix in state["dependency_fixes"]:
        if module_info(source, fix["module"])["Version"] != fix["resolved"]:
            raise ValueError("dependency graph changed after fix check; prepare a new candidate")
    destination = args.destination.resolve()
    name = "koko-" + state["version"] + "-linux-amd64"
    folder, archive = destination / name, destination / (name + ".tar.gz")
    if folder.exists() or archive.exists():
        raise ValueError("package already exists; use a new output directory or patch revision")
    # 与上游命令一致，但强制执行非 PHONY 目标，避免旧文件被当作新版本交付。
    run(["make", "-B", "build", "-s", "GOOS=linux", "GOARCH=amd64",
         "GOFLAGS=-p=2 -mod=readonly", "VERSION=" + state["version"]], source)
    binary = source / "build/koko"
    check_binary(binary)
    folder.mkdir(parents=True)
    shutil.copy2(binary, folder / "koko")
    diff = subprocess.check_output(["git", "diff", "HEAD", "--binary"], cwd=source, timeout=30)
    crypto = module_info(source, "golang.org/x/crypto")["Version"]
    fields = {
        "VERSION": state["version"], "UPSTREAM_VERSION": state["upstream_version"],
        "UPSTREAM_SHA": state["upstream_sha"], "CUSTOM_PATCH_SHA": state["custom_patch_sha"],
        "CUSTOM_SOURCE_DIRTY": str(state["custom_source_dirty"]).lower(),
        "PATCH_SET_SHA256": state["patch_set_sha256"],
        "SOURCE_DIFF_SHA256": hashlib.sha256(diff).hexdigest(),
        "GO_VERSION": run(["go", "env", "GOVERSION"], source),
        "GOOS": "linux", "GOARCH": "amd64", "LINKAGE": "static", "X_CRYPTO_VERSION": crypto,
        "DEPENDENCY_FIXES": ",".join(fix["id"] + ":" + fix["status"] for fix in state["dependency_fixes"]),
    }
    (folder / "BUILD_INFO.txt").write_text("".join(key + "=" + value + "\n" for key, value in fields.items()))
    (folder / "SHA256SUMS").write_text("".join(sha256(folder / item) + "  " + item + "\n" for item in ("koko", "BUILD_INFO.txt")))
    with tarfile.open(archive, "w:gz") as output:
        output.add(folder, arcname=name)
    print("Package: " + str(archive))
    print("SHA256: " + sha256(archive))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("prepare")
    create.add_argument("--tag")
    create.add_argument("--revision", type=int)
    create.add_argument("--destination", type=Path, required=True)
    create.set_defaults(action=prepare)
    fix = commands.add_parser("dependencies")
    fix.add_argument("--source", type=Path, required=True)
    fix.set_defaults(action=dependencies)
    pack = commands.add_parser("build")
    pack.add_argument("--source", type=Path, required=True)
    pack.add_argument("--destination", type=Path, required=True)
    pack.set_defaults(action=build)
    args = parser.parse_args()
    try:
        args.action(args)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("Custom release stopped: " + str(error), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
